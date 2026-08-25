//go:build integration_testdb

package tests

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

type contentEnv struct {
	ctx     context.Context
	db      *db.Database
	contact *repository.Contact
	ir      *repository.InteractionRepository
	cr      *repository.CommsMessageRepository
	tr      *repository.TelegramMessageRepository
	mr      *repository.MessagesMessageRepository
	nr      *repository.MeetingNoteRepository
	er      *repository.CalendarEventRepository
	pr      *repository.PhoneCallRepository
}

func newContentEnv(t *testing.T) *contentEnv {
	t.Helper()
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	contact, err := h.SeedContact(ctx, h.Generator().Contact())
	require.NoError(t, err)
	return &contentEnv{ctx: ctx, db: database, contact: contact,
		ir: repository.NewInteractionRepository(database.Queries),
		cr: repository.NewCommsMessageRepository(database.Queries),
		tr: repository.NewTelegramMessageRepository(database.Queries),
		mr: repository.NewMessagesMessageRepository(database.Queries),
		nr: repository.NewMeetingNoteRepository(database.Queries),
		er: repository.NewCalendarEventRepository(database.Queries),
		pr: repository.NewPhoneCallRepository(database.Queries)}
}

func (e *contentEnv) svc(q db.Querier) *service.InteractionContentService {
	return service.NewInteractionContentService(repository.NewInteractionRepository(q), repository.NewCommsMessageRepository(q), repository.NewTelegramMessageRepository(q), repository.NewMessagesMessageRepository(q), repository.NewMeetingNoteRepository(q), repository.NewCalendarEventRepository(q), repository.NewPhoneCallRepository(q))
}

func requireContentIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
}

func seedInteraction(t *testing.T, e *contentEnv, contactID uuid.UUID, source string, at time.Time, ref *string) repository.Interaction {
	t.Helper()
	row, err := e.ir.TestInsertInteraction(e.ctx, uuid.New(), contactID, source, ref, at, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	return *row
}

func seedComms(t *testing.T, e *contentEnv, interactionID, contactID uuid.UUID, source, thread, external string, at time.Time, subject, body string) repository.CommsMessage {
	t.Helper()
	threadID, subjectValue, bodyValue, peer := thread, subject, body, "peer-"+external
	row, err := e.cr.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{Source: source, ExternalID: external, ThreadID: &threadID, Subject: &subjectValue, Body: &bodyValue, PeerHandle: &peer, Direction: repository.InteractionDirectionInbound, SentAt: at, MatchedContactID: contactID})
	require.NoError(t, err)
	require.NoError(t, e.cr.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interactionID))
	return *row
}

func seedCommsNoThread(t *testing.T, e *contentEnv, interactionID, contactID uuid.UUID, source, external string, at time.Time) repository.CommsMessage {
	t.Helper()
	row, err := e.db.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: source, ExternalID: external, Direction: repository.InteractionDirectionInbound, SentAt: at, MatchedContactID: &contactID, InteractionID: &interactionID})
	require.NoError(t, err)
	return repository.CommsMessage{ID: row.ID, Source: row.Source, ExternalID: row.ExternalID, ThreadID: row.ThreadID, Subject: row.Subject, Body: row.Body, PeerHandle: row.PeerHandle, Direction: row.Direction, SentAt: row.SentAt, SourceMetadata: row.SourceMetadata, InteractionID: row.InteractionID}
}

func seedTelegram(t *testing.T, e *contentEnv, interactionID, contactID uuid.UUID, chatID int64, msgID int32, at time.Time, chatType, title, body string, deleted *time.Time, first, last, username *string) *db.TelegramMessage {
	t.Helper()
	titleValue, bodyValue := title, body
	row, err := e.db.Queries.InsertFullTelegramMessageForTest(e.ctx, db.InsertFullTelegramMessageForTestParams{TelegramMessageID: msgID, TelegramChatID: chatID, ChatType: chatType, ChatTitle: &titleValue, MessageText: &bodyValue, MessageType: "text", SentAt: at, IsOutgoing: false, PeerUsername: username, PeerFirstName: first, PeerLastName: last, MatchedContactID: &contactID, InteractionID: &interactionID, DeletedAt: deleted})
	require.NoError(t, err)
	return row
}

func seedMessages(t *testing.T, e *contentEnv, interactionID, contactID uuid.UUID, chat, guid string, at time.Time, group bool) repository.MessagesMessage {
	t.Helper()
	body := "message-" + guid
	row, err := e.mr.UpsertMessage(e.ctx, repository.UpsertMessagesMessageParams{Guid: guid, ChatGuid: chat, PeerHandle: "peer-" + guid, Text: &body, MessageType: "text", SentAt: at, IsGroupChat: group, MatchedContactID: &contactID})
	require.NoError(t, err)
	require.NoError(t, e.mr.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interactionID))
	return *row
}

func seedPhone(t *testing.T, e *contentEnv, interactionID, contactID uuid.UUID, at time.Time) {
	t.Helper()
	answered := true
	call, err := e.pr.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{CallUniqueID: uuid.NewString(), PeerHandle: "peer-call", PeerNormalized: "peer-call", Service: repository.PhoneCallServiceVoice, Direction: repository.PhoneCallDirectionInbound, Answered: &answered, HasVoicemail: true, DurationSeconds: 31, StartedAt: at, MatchedContactID: &contactID})
	require.NoError(t, err)
	require.NoError(t, e.pr.MarkProcessed(e.ctx, repository.MarkProcessedParams{ID: call.ID, InteractionID: &interactionID}))
}

func insertNote(t *testing.T, e *contentEnv, sessionID uuid.UUID, eventID *uuid.UUID, deleted bool, at time.Time) {
	t.Helper()
	var kind *string
	if eventID != nil {
		linkedKind := repository.LinkedKindEvent
		kind = &linkedKind
	}
	title, summary, memo := "Note title", "Note summary", "Note memo"
	tx, err := e.db.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer tx.Rollback(e.ctx)
	_, err = e.nr.InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{AnarlogSessionID: sessionID, Title: &title, Summary: &summary, Memo: &memo, LinkedKind: kind, LinkedID: eventID, LinkageState: repository.LinkageStateLinked, InputHash: "", ResolvedSetHash: "", MeetingAt: at})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	if deleted {
		tx, err = e.db.Pool.Begin(e.ctx)
		require.NoError(t, err)
		require.NoError(t, e.nr.SoftDeleteMeetingNoteBySessionIDTx(e.ctx, tx, sessionID))
		require.NoError(t, tx.Commit(e.ctx))
	}
}

// spec: IXN-011
func TestInteractionContentService_GroupThreadWindow(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	interaction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base, nil)
	h := synthetic.NewHarnessForNamespace(t, e.ctx, e.db, syntheticNS(t)+"-speaker", factory.DefaultSeed)
	speaker, err := h.SeedContact(e.ctx, h.Generator().Contact())
	require.NoError(t, err)
	thread := "window-thread"
	first := seedComms(t, e, interaction.ID, e.contact.ID, repository.InteractionSourceGChat, thread, "first", base, "", "first")
	second := seedComms(t, e, interaction.ID, e.contact.ID, repository.InteractionSourceGChat, thread, "second", base, "", "second")
	middle := seedComms(t, e, interaction.ID, e.contact.ID, repository.InteractionSourceGChat, thread, "middle", base.Add(time.Second), "", "middle")
	_, err = e.db.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "before", ThreadID: &thread, Direction: repository.InteractionDirectionInbound, SentAt: base.Add(-time.Second), MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	_, err = e.db.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "after", ThreadID: &thread, Direction: repository.InteractionDirectionInbound, SentAt: base.Add(2 * time.Second), MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	other, err := e.db.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "speaker-other", ThreadID: &thread, Direction: repository.InteractionDirectionInbound, SentAt: base.Add(time.Second), MatchedContactID: &speaker.ID})
	require.NoError(t, err)
	// A NULL-thread FK row must render directly instead of disappearing.
	containerless := seedCommsNoThread(t, e, interaction.ID, e.contact.ID, repository.InteractionSourceGChat, "containerless", base.Add(time.Second))
	content, err := e.svc(e.db.Queries).GetContent(e.ctx, interaction.ID)
	require.NoError(t, err)
	require.Len(t, content.Messages, 5)
	enrichment, err := e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{interaction})
	require.NoError(t, err)
	require.Equal(t, 4, enrichment[interaction.ID].MessageCount)
	ids := make([]uuid.UUID, len(content.Messages))
	for i := range content.Messages {
		ids[i] = content.Messages[i].ID
	}
	require.Contains(t, ids, first.ID)
	require.Contains(t, ids, second.ID)
	require.Contains(t, ids, middle.ID)
	require.Contains(t, ids, other.ID)
	require.Contains(t, ids, containerless.ID)
	expected := []struct {
		id   uuid.UUID
		when time.Time
	}{{first.ID, first.SentAt}, {second.ID, second.SentAt}, {middle.ID, middle.SentAt}, {other.ID, base.Add(time.Second)}, {containerless.ID, containerless.SentAt}}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].when.Equal(expected[j].when) {
			return bytes.Compare(expected[i].id[:], expected[j].id[:]) < 0
		}
		return expected[i].when.Before(expected[j].when)
	})
	for i := range expected {
		require.Equal(t, expected[i].id, content.Messages[i].ID)
	}
	for _, row := range content.Messages {
		require.NotEqual(t, "before", row.Body)
		require.NotEqual(t, "after", row.Body)
	}
	for _, row := range content.Messages {
		if row.ID == containerless.ID {
			require.Empty(t, row.VenueKey)
		}
	}
	one := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base.Add(10*time.Second), nil)
	oneRow := seedComms(t, e, one.ID, e.contact.ID, repository.InteractionSourceGChat, "one-message", "only", base.Add(10*time.Second), "", "only")
	oneContent, err := e.svc(e.db.Queries).GetContent(e.ctx, one.ID)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{oneRow.ID}, []uuid.UUID{oneContent.Messages[0].ID})
}

func TestInteractionContentService_ReplicaDedup(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	h := synthetic.NewHarnessForNamespace(t, e.ctx, e.db, syntheticNS(t)+"-other", factory.DefaultSeed)
	other, err := h.SeedContact(e.ctx, h.Generator().Contact())
	require.NoError(t, err)
	when := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	firstInteraction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, when, nil)
	secondInteraction := seedInteraction(t, e, other.ID, repository.InteractionSourceGChat, when, nil)
	first := seedComms(t, e, firstInteraction.ID, e.contact.ID, repository.InteractionSourceGChat, "fanout-thread", "same-source-id", when, "", "original")
	second := seedComms(t, e, secondInteraction.ID, other.ID, repository.InteractionSourceGChat, "fanout-thread", "same-source-id", when, "", "replica")
	content, err := e.svc(e.db.Queries).GetContent(e.ctx, firstInteraction.ID)
	require.NoError(t, err)
	require.Len(t, content.Messages, 1)
	require.Equal(t, first.ID, content.Messages[0].ID)
	require.NotEqual(t, second.ID, content.Messages[0].ID)
}

// spec: IXN-003
func TestInteractionContentService_VenueFromContent(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	when := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	first := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, when, nil)
	seedComms(t, e, first.ID, e.contact.ID, repository.InteractionSourceGChat, "content-container", "content-1", when, "", "body")
	page, err := e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{first})
	require.NoError(t, err)
	require.Equal(t, "gchat:content-container", page[first.ID].VenueTags[0].Key)
	second := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, when.Add(time.Second), nil)
	seedComms(t, e, second.ID, e.contact.ID, repository.InteractionSourceGChat, "actual-container", "content-2", when.Add(time.Second), "", "body")
	tx, err := e.db.Pool.Begin(e.ctx)
	require.NoError(t, err)
	venueNodeID, err := repository.NewVenueRepository(e.db.Queries).ResolveVenueForInteraction(e.ctx, tx, "gchat", repository.VenueKindGroupChat, "different-container", "")
	require.NoError(t, err)
	require.NoError(t, e.ir.UpdateInteractionVenueTx(e.ctx, tx, second.ID, &venueNodeID))
	require.NoError(t, tx.Commit(e.ctx))
	page, err = e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{second})
	require.NoError(t, err)
	require.Equal(t, "gchat:actual-container", page[second.ID].VenueTags[0].Key)
	require.NotEqual(t, "gchat:different-container", page[second.ID].VenueTags[0].Key)
}

func TestInteractionContentService_ReadOnlyEnforced(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	when := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	comms := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceEmail, when, nil)
	seedComms(t, e, comms.ID, e.contact.ID, repository.InteractionSourceEmail, "ro-thread", "ro-comms", when, "", "ro")
	tg := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTelegram, when, nil)
	seedTelegram(t, e, tg.ID, e.contact.ID, chatID(tg.ID), 1, when, "group", "RO", "tg", nil, ptrString("Peer"), ptrString("One"), nil)
	msgs := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceMessages, when, nil)
	seedMessages(t, e, msgs.ID, e.contact.ID, "ro-chat", "ro-msg", when, true)
	event, err := e.er.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: "ro-event", GcalCalendarID: "ro-calendar", GoogleAccountID: "ro-account", Title: ptrString("RO event"), StartTime: when, EndTime: when.Add(time.Hour), Status: "confirmed", SyncedAt: when, MatchedContactIDs: []uuid.UUID{e.contact.ID}})
	require.NoError(t, err)
	gcalRef := event.ID.String()
	gcal := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, when, &gcalRef)
	insertNote(t, e, uuid.New(), &event.ID, false, when)
	session := uuid.New()
	aref := fmt.Sprintf("anarlog:%s:%s", session, e.contact.ID)
	anarlog := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceAnarlogSessions, when, &aref)
	insertNote(t, e, session, nil, false, when)
	call := seedInteraction(t, e, e.contact.ID, repository.InteractionSourcePhoneCalls, when, nil)
	seedPhone(t, e, call.ID, e.contact.ID, when)
	manual := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceManual, when, nil)
	todo := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTodoist, when, nil)
	gcalWithoutNote := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, when, nil)
	rows := []repository.Interaction{comms, tg, msgs, gcal, gcalWithoutNote, anarlog, call, manual, todo}
	tx, err := e.db.Pool.BeginTx(e.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	defer tx.Rollback(e.ctx)
	ro := e.svc(db.New(tx))
	enriched, err := ro.EnrichPage(e.ctx, rows)
	require.NoError(t, err)
	require.Len(t, enriched, len(rows))
	require.Equal(t, 1, enriched[comms.ID].MessageCount)
	require.Equal(t, 1, enriched[tg.ID].MessageCount)
	require.Equal(t, 1, enriched[msgs.ID].MessageCount)
	require.Equal(t, "meeting_note", enriched[gcal.ID].ContentKind)
	require.Equal(t, "none", enriched[gcalWithoutNote.ID].ContentKind)
	require.Equal(t, "meeting_note", enriched[anarlog.ID].ContentKind)
	require.Equal(t, "call", enriched[call.ID].ContentKind)
	for _, tc := range []struct {
		id        uuid.UUID
		kind      string
		msgCount  int
		noteCount int
	}{
		{comms.ID, "messages", 1, 0},
		{tg.ID, "messages", 1, 0},
		{msgs.ID, "messages", 1, 0},
		{gcal.ID, "meeting_note", 0, 1},
		{anarlog.ID, "meeting_note", 0, 1},
	} {
		content, err := ro.GetContent(e.ctx, tc.id)
		require.NoError(t, err)
		require.Equal(t, tc.kind, content.Kind)
		require.Len(t, content.Messages, tc.msgCount)
		require.Len(t, content.MeetingNotes, tc.noteCount)
	}
	venues, err := ro.ListContactVenues(e.ctx, e.contact.ID)
	require.NoError(t, err)
	require.Len(t, venues, 3)
	listRepo := repository.NewInteractionRepository(db.New(tx))
	list, err := listRepo.ListContactInteractionsFiltered(e.ctx, repository.InteractionListFilterParams{ContactID: e.contact.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, list, len(rows))
	count, err := listRepo.CountContactInteractionsFiltered(e.ctx, repository.InteractionListFilterParams{ContactID: e.contact.ID})
	require.NoError(t, err)
	require.Equal(t, int64(len(rows)), count)
}

func TestInteractionContentService_LatestRowMetadata(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	tgInteraction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTelegram, base, nil)
	chat := chatID(tgInteraction.ID)
	seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat, 1, base, "group", "Alpha Old", "old", nil, nil, nil, nil)
	seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat, 2, base.Add(time.Second), "group", "Zebra Mid", "mid", nil, nil, nil, nil)
	seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat, 3, base.Add(2*time.Second), "group", "Mango New", "new", nil, nil, nil, nil)
	tombstoneAt := base.Add(3 * time.Second)
	seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat, 4, tombstoneAt, "group", "Tombstoned", "tombstone", &tombstoneAt, nil, nil, nil)
	dmInteraction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTelegram, base, nil)
	dmChat := chatID(dmInteraction.ID)
	seedTelegram(t, e, dmInteraction.ID, e.contact.ID, dmChat, 11, base, "group", "Older group", "old", nil, nil, nil, nil)
	seedTelegram(t, e, dmInteraction.ID, e.contact.ID, dmChat, 12, base.Add(time.Second), "group", "Older group 2", "mid", nil, nil, nil, nil)
	seedTelegram(t, e, dmInteraction.ID, e.contact.ID, dmChat, 13, base.Add(2*time.Second), "private", "Private title", "new", nil, nil, nil, nil)
	ref := "metadata-email"
	emailInteraction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceEmail, base, &ref)
	seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-thread", "old-subject", base, "Alpha Old Subject", "old")
	seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-thread", "mid-subject", base.Add(time.Second), "Zebra Mid Subject", "mid")
	seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-thread", "new-subject", base.Add(2*time.Second), "Mango New Subject", "new")
	tombstonedEmail := seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-thread", "tombstoned-subject", base.Add(3*time.Second), "Tombstoned Subject", "tombstoned")
	require.NoError(t, e.cr.SoftDeleteByID(e.ctx, tombstonedEmail.ID))
	emailTieA := seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-tie", "tie-a", base.Add(4*time.Second), "Tie Subject A", "tie-a")
	emailTieB := seedComms(t, e, emailInteraction.ID, e.contact.ID, repository.InteractionSourceEmail, "metadata-tie", "tie-b", base.Add(4*time.Second), "Tie Subject B", "tie-b")
	page, err := e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{tgInteraction, dmInteraction, emailInteraction})
	require.NoError(t, err)
	require.Equal(t, "Mango New", page[tgInteraction.ID].VenueTags[0].Label)
	require.Equal(t, "group_chat", page[tgInteraction.ID].VenueTags[0].Kind)
	require.Equal(t, "DM", page[dmInteraction.ID].VenueTags[0].Label)
	require.Equal(t, "dm", page[dmInteraction.ID].VenueTags[0].Kind)
	require.Equal(t, "Mango New Subject", page[emailInteraction.ID].VenueTags[0].Label)
	venues, err := e.svc(e.db.Queries).ListContactVenues(e.ctx, e.contact.ID)
	require.NoError(t, err)
	labels := map[string]string{}
	for _, venue := range venues {
		labels[venue.Key] = venue.Label
	}
	require.Equal(t, "Mango New", labels[fmt.Sprintf("telegram:%d", chat)])
	require.Equal(t, "DM", labels[fmt.Sprintf("telegram:%d", dmChat)])
	require.Equal(t, "Mango New Subject", labels["email:metadata-thread"])
	tieA := seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat+1, 5, base.Add(4*time.Second), "group", "Tie A", "tie-a", nil, nil, nil, nil)
	tieB := seedTelegram(t, e, tgInteraction.ID, e.contact.ID, chat+1, 6, base.Add(4*time.Second), "group", "Tie B", "tie-b", nil, nil, nil, nil)
	wantTie := tieA.ChatTitle
	if bytes.Compare(tieA.ID[:], tieB.ID[:]) < 0 {
		wantTie = tieB.ChatTitle
	}
	venues, err = e.svc(e.db.Queries).ListContactVenues(e.ctx, e.contact.ID)
	require.NoError(t, err)
	found := false
	for _, venue := range venues {
		if venue.Key == fmt.Sprintf("telegram:%d", chat+1) {
			found = true
			require.Equal(t, *wantTie, venue.Label)
		}
	}
	require.True(t, found)
	wantEmailTie := emailTieA.Subject
	if bytes.Compare(emailTieA.ID[:], emailTieB.ID[:]) < 0 {
		wantEmailTie = emailTieB.Subject
	}
	found = false
	for _, venue := range venues {
		if venue.Key == "email:metadata-tie" {
			found = true
			require.Equal(t, *wantEmailTie, venue.Label)
		}
	}
	require.True(t, found)
}

func TestInteractionContentService_SoftDeleteFourFamilies(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	ci := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base, nil)
	ciLive := seedComms(t, e, ci.ID, e.contact.ID, repository.InteractionSourceGChat, "soft-comms", "live", base, "", "live")
	deadComms := seedComms(t, e, ci.ID, e.contact.ID, repository.InteractionSourceGChat, "soft-comms", "dead", base.Add(time.Minute), "", "dead")
	require.NoError(t, e.cr.SoftDeleteByID(e.ctx, deadComms.ID))
	// Keep the non-FK row in the same thread so a missing ID-list tombstone
	// predicate expands the window and incorrectly returns it.
	commsWindowThread := "soft-comms"
	_, err := e.db.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "comms-window-live-thread", ThreadID: &commsWindowThread, Direction: repository.InteractionDirectionInbound, SentAt: base.Add(30 * time.Second), MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	tg := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTelegram, base, nil)
	tgLive := seedTelegram(t, e, tg.ID, e.contact.ID, chatID(tg.ID), 1, base, "group", "live", "live", nil, nil, nil, nil)
	deadTGAt := base.Add(time.Minute)
	seedTelegram(t, e, tg.ID, e.contact.ID, chatID(tg.ID), 2, deadTGAt, "group", "dead", "dead", &deadTGAt, nil, nil, nil)
	tgWindowTitle, tgWindowBody := "window", "window"
	_, err = e.db.Queries.InsertFullTelegramMessageForTest(e.ctx, db.InsertFullTelegramMessageForTestParams{TelegramMessageID: 3, TelegramChatID: chatID(tg.ID), ChatType: "group", ChatTitle: &tgWindowTitle, MessageText: &tgWindowBody, MessageType: "text", SentAt: base.Add(30 * time.Second), MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	mi := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceMessages, base, nil)
	miLive := seedMessages(t, e, mi.ID, e.contact.ID, "soft-messages", "live", base, false)
	deadMsg := seedMessages(t, e, mi.ID, e.contact.ID, "soft-messages", "dead", base.Add(time.Minute), false)
	require.NoError(t, e.mr.TestSoftDeleteMessage(e.ctx, deadMsg.ID))
	_, err = e.db.Queries.TestInsertMessagesMessageLinked(e.ctx, db.TestInsertMessagesMessageLinkedParams{Guid: "messages-window-live", ChatGuid: "soft-messages", PeerHandle: "window-peer", SentAt: base.Add(30 * time.Second), InteractionID: nil})
	require.NoError(t, err)
	deadOnly := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base, nil)
	deadOnlyRow := seedComms(t, e, deadOnly.ID, e.contact.ID, repository.InteractionSourceGChat, "dead-only", "dead-only", base, "", "dead")
	require.NoError(t, e.cr.SoftDeleteByID(e.ctx, deadOnlyRow.ID))
	deadEvent, err := e.er.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: "dead-note-event", GcalCalendarID: "calendar", GoogleAccountID: "account", Title: ptrString("dead event"), StartTime: base, EndTime: base.Add(time.Hour), Status: "confirmed", SyncedAt: base, MatchedContactIDs: []uuid.UUID{e.contact.ID}})
	require.NoError(t, err)
	deadEventRef := deadEvent.ID.String()
	deadGCal := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, base, &deadEventRef)
	insertNote(t, e, uuid.New(), &deadEvent.ID, true, base)
	deadSession := uuid.New()
	deadAnarlogRef := fmt.Sprintf("anarlog:%s:%s", deadSession, e.contact.ID)
	deadAnarlog := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceAnarlogSessions, base, &deadAnarlogRef)
	insertNote(t, e, deadSession, nil, true, base)
	page, err := e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{ci, tg, mi, deadOnly})
	require.NoError(t, err)
	require.Equal(t, 1, page[ci.ID].MessageCount)
	require.Equal(t, 1, page[tg.ID].MessageCount)
	require.Equal(t, 1, page[mi.ID].MessageCount)
	require.Equal(t, 0, page[deadOnly.ID].MessageCount)
	notePage, err := e.svc(e.db.Queries).EnrichPage(e.ctx, []repository.Interaction{deadGCal, deadAnarlog})
	require.NoError(t, err)
	require.Equal(t, "none", notePage[deadGCal.ID].ContentKind)
	require.Equal(t, "meeting_note", notePage[deadAnarlog.ID].ContentKind)
	deadGCalContent, err := e.svc(e.db.Queries).GetContent(e.ctx, deadGCal.ID)
	require.NoError(t, err)
	require.Equal(t, "none", deadGCalContent.Kind)
	require.Empty(t, deadGCalContent.MeetingNotes)
	mixedEvent, err := e.er.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: "mixed-note-event", GcalCalendarID: "calendar", GoogleAccountID: "account", Title: ptrString("mixed event"), StartTime: base, EndTime: base.Add(time.Hour), Status: "confirmed", SyncedAt: base, MatchedContactIDs: []uuid.UUID{e.contact.ID}})
	require.NoError(t, err)
	mixedEventRef := mixedEvent.ID.String()
	mixedGCal := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, base, &mixedEventRef)
	insertNote(t, e, uuid.New(), &mixedEvent.ID, false, base)
	insertNote(t, e, uuid.New(), &mixedEvent.ID, true, base.Add(time.Second))
	mixedGCalContent, err := e.svc(e.db.Queries).GetContent(e.ctx, mixedGCal.ID)
	require.NoError(t, err)
	require.Equal(t, "meeting_note", mixedGCalContent.Kind)
	require.Len(t, mixedGCalContent.MeetingNotes, 1)
	deadAnarlogContent, err := e.svc(e.db.Queries).GetContent(e.ctx, deadAnarlog.ID)
	require.NoError(t, err)
	require.Equal(t, "meeting_note", deadAnarlogContent.Kind)
	require.Empty(t, deadAnarlogContent.MeetingNotes)
	for _, tc := range []struct {
		id     uuid.UUID
		wantID uuid.UUID
	}{
		{ci.ID, ciLive.ID},
		{tg.ID, tgLive.ID},
		{mi.ID, miLive.ID},
	} {
		content, err := e.svc(e.db.Queries).GetContent(e.ctx, tc.id)
		require.NoError(t, err)
		require.Len(t, content.Messages, 1)
		require.Equal(t, tc.wantID, content.Messages[0].ID)
	}
	venues, err := e.svc(e.db.Queries).ListContactVenues(e.ctx, e.contact.ID)
	require.NoError(t, err)
	for _, venue := range venues {
		require.NotEqual(t, "gchat:dead-only", venue.Key)
	}
	deadInteraction := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceManual, base, nil)
	require.NoError(t, e.ir.SoftDeleteInteraction(e.ctx, deadInteraction.ID))
	_, err = e.svc(e.db.Queries).GetContent(e.ctx, deadInteraction.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

type countingDBTX struct {
	inner            db.DBTX
	exec, query, row atomic.Int64
}

func (c *countingDBTX) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	c.exec.Add(1)
	return c.inner.Exec(ctx, sql, args...)
}
func (c *countingDBTX) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	c.query.Add(1)
	return c.inner.Query(ctx, sql, args...)
}
func (c *countingDBTX) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	c.row.Add(1)
	return c.inner.QueryRow(ctx, sql, args...)
}
func (c *countingDBTX) total() int64 { return c.exec.Load() + c.query.Load() + c.row.Load() }
func measureEnrich(t *testing.T, e *contentEnv, rows []repository.Interaction) int64 {
	counter := &countingDBTX{inner: e.db.Pool}
	_, err := e.svc(db.New(counter)).EnrichPage(e.ctx, rows)
	require.NoError(t, err)
	return counter.total()
}

// spec: IXN-012
func TestInteractionContentService_QueryCount(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	seedPage := func(n int) []repository.Interaction {
		rows := make([]repository.Interaction, 0, n*9)
		batch := uuid.NewString()
		for i := 0; i < n; i++ {
			at := base.Add(time.Duration(i) * time.Second)
			ci := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceEmail, at, nil)
			seedComms(t, e, ci.ID, e.contact.ID, repository.InteractionSourceEmail, fmt.Sprintf("count-%s-%d", batch, i), fmt.Sprintf("count-email-%s-%d", batch, i), at, "", "body")
			rows = append(rows, ci)
			tg := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTelegram, at, nil)
			seedTelegram(t, e, tg.ID, e.contact.ID, chatID(tg.ID), 1, at, "private", "", "body", nil, nil, nil, nil)
			rows = append(rows, tg)
			mi := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceMessages, at, nil)
			seedMessages(t, e, mi.ID, e.contact.ID, fmt.Sprintf("count-chat-%s-%d", batch, i), fmt.Sprintf("count-msg-%s-%d", batch, i), at, false)
			rows = append(rows, mi)
			event, err := e.er.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: fmt.Sprintf("count-event-%s-%d", batch, i), GcalCalendarID: "count-calendar", GoogleAccountID: "count-account", Title: ptrString("event"), StartTime: at, EndTime: at.Add(time.Hour), Status: "confirmed", SyncedAt: at, MatchedContactIDs: []uuid.UUID{e.contact.ID}})
			require.NoError(t, err)
			ref := event.ID.String()
			gi := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, at, &ref)
			insertNote(t, e, uuid.New(), &event.ID, false, at)
			rows = append(rows, gi)
			gn := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGCal, at, nil)
			rows = append(rows, gn)
			session := uuid.New()
			aref := fmt.Sprintf("anarlog:%s:%s", session, e.contact.ID)
			ai := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceAnarlogSessions, at, &aref)
			insertNote(t, e, session, nil, false, at)
			rows = append(rows, ai)
			pi := seedInteraction(t, e, e.contact.ID, repository.InteractionSourcePhoneCalls, at, nil)
			seedPhone(t, e, pi.ID, e.contact.ID, at)
			rows = append(rows, pi)
			rows = append(rows, seedInteraction(t, e, e.contact.ID, repository.InteractionSourceManual, at, nil), seedInteraction(t, e, e.contact.ID, repository.InteractionSourceTodoist, at, nil))
		}
		return rows
	}
	small, large := seedPage(1), seedPage(5)
	require.Len(t, small, 9)
	require.Len(t, large, 45)
	require.Equal(t, measureEnrich(t, e, small), measureEnrich(t, e, large))
}

func TestInteractionContentFilteredList(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	from, to := base, base.Add(4*time.Second)
	var ids []uuid.UUID
	var filterRows []repository.Interaction
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if i == 2 {
			at = base.Add(time.Second)
		}
		if i == 3 {
			at = to
		}
		row := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, at, nil)
		seedComms(t, e, row.ID, e.contact.ID, repository.InteractionSourceGChat, "filter-container", fmt.Sprintf("filter-%d", i), at, "", "body")
		if at.Before(to) {
			ids = append(ids, row.ID)
			filterRows = append(filterRows, row)
		}
	}
	differentContainer := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base.Add(2*time.Second), nil)
	seedComms(t, e, differentContainer.ID, e.contact.ID, repository.InteractionSourceGChat, "other-container", "other", differentContainer.OccurredAt, "", "body")
	tomb := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, base.Add(3*time.Second), nil)
	require.NoError(t, e.ir.SoftDeleteInteraction(e.ctx, tomb.ID))
	venueSource, venueContainer := repository.InteractionSourceGChat, "filter-container"
	p := repository.InteractionListFilterParams{ContactID: e.contact.ID, VenueSource: &venueSource, VenueContainer: &venueContainer, From: &from, To: &to, Limit: 2}
	page1, err := e.ir.ListContactInteractionsFiltered(e.ctx, p)
	require.NoError(t, err)
	p.Offset = 2
	page2, err := e.ir.ListContactInteractionsFiltered(e.ctx, p)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Len(t, page2, 1)
	got := append(pageIDs(page1), pageIDs(page2)...)
	for _, id := range got {
		require.Contains(t, ids, id)
		require.NotEqual(t, tomb.ID, id)
	}
	require.NotContains(t, got, differentContainer.ID)
	count, err := e.ir.CountContactInteractionsFiltered(e.ctx, p)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	sort.Slice(filterRows, func(i, j int) bool {
		if filterRows[i].OccurredAt.Equal(filterRows[j].OccurredAt) {
			return bytes.Compare(filterRows[i].ID[:], filterRows[j].ID[:]) > 0
		}
		return filterRows[i].OccurredAt.After(filterRows[j].OccurredAt)
	})
	require.Equal(t, []uuid.UUID{filterRows[0].ID, filterRows[1].ID, filterRows[2].ID}, got)
	p.Offset = 0
	p.Limit = 20
	rows, err := e.ir.ListContactInteractionsFiltered(e.ctx, p)
	require.NoError(t, err)
	require.Contains(t, pageIDs(rows), ids[0])
	require.NotContains(t, pageIDs(rows), differentContainer.ID)
}
func pageIDs(rows []repository.Interaction) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func TestInteractionContentReads_HeavyContact(t *testing.T) {
	requireContentIntegration(t)
	t.Parallel()
	e := newContentEnv(t)
	base := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	rows := make([]repository.Interaction, 0, 1000)
	for i := 0; i < 1000; i++ {
		at := base.Add(-time.Duration(i) * time.Second)
		row := seedInteraction(t, e, e.contact.ID, repository.InteractionSourceGChat, at, nil)
		for j := 0; j < 3; j++ {
			seedComms(t, e, row.ID, e.contact.ID, repository.InteractionSourceGChat, fmt.Sprintf("heavy-%d", i), fmt.Sprintf("heavy-%d-%d", i, j), at.Add(time.Duration(j)*time.Millisecond), "", "body")
		}
		rows = append(rows, row)
	}
	require.Len(t, rows, 1000)
	started := time.Unix(0, accelerated.GetCurrentTime().UnixNano())
	page, err := e.ir.ListContactInteractionsFiltered(e.ctx, repository.InteractionListFilterParams{ContactID: e.contact.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, page, 100)
	require.Less(t, time.Since(started), 2*time.Second)
	started = time.Unix(0, accelerated.GetCurrentTime().UnixNano())
	content, err := e.svc(e.db.Queries).GetContent(e.ctx, rows[0].ID)
	require.NoError(t, err)
	require.Len(t, content.Messages, 3)
	require.Less(t, time.Since(started), 2*time.Second)
	started = time.Unix(0, accelerated.GetCurrentTime().UnixNano())
	venues, err := e.svc(e.db.Queries).ListContactVenues(e.ctx, e.contact.ID)
	require.NoError(t, err)
	require.Len(t, venues, 1000)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Equal(t, measureEnrich(t, e, rows[:9]), measureEnrich(t, e, rows[:100]))
}

func chatID(id uuid.UUID) int64 {
	value := int64(binary.BigEndian.Uint64(id[:8]) & 0x3fffffffffffffff)
	if value == 0 {
		return 1
	}
	return value
}
func ptrString(value string) *string { return &value }
