//go:build integration_testdb

package threadctx_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type builderEnv struct {
	ctx      context.Context
	database *db.Database
	harness  *synthetic.Harness
	builder  *threadctx.Builder
	comms    *repository.CommsMessageRepository
	notes    *repository.MeetingNoteRepository
	venues   *repository.VenueRepository
	base     time.Time
}

func newBuilderEnv(t *testing.T) *builderEnv {
	t.Helper()
	ctx := t.Context()
	harness := synthetic.NewHarnessForNamespace(t, ctx, "t"+uuid.NewString()[:8], factory.DefaultSeed)
	database := harness.Database()
	venues := repository.NewVenueRepository(database.Queries)
	return &builderEnv{
		ctx:      ctx,
		database: database,
		harness:  harness,
		builder:  threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		comms:    repository.NewCommsMessageRepository(database.Queries),
		notes:    repository.NewMeetingNoteRepository(database.Queries),
		venues:   venues,
		base:     accelerated.GetCurrentTime().UTC().Add(-72 * time.Hour).Truncate(time.Second),
	}
}

func (e *builderEnv) at(seconds int) time.Time {
	return e.base.Add(time.Duration(seconds) * time.Second)
}

func (e *builderEnv) contact(t *testing.T) uuid.UUID {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	return c.ID
}

func (e *builderEnv) venue(t *testing.T, source, kind, container string) threadctx.VenueID {
	t.Helper()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	id, err := e.venues.ResolveVenueForInteraction(e.ctx, tx, source, kind, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return id
}

// commsCopy stores one per-contact copy of a comms message.
type commsCopy struct {
	source, thread, external, direction, peer, body string
	contact                                         uuid.UUID
	at                                              time.Time
	metadata                                        map[string]any
}

func (e *builderEnv) store(t *testing.T, c commsCopy) {
	t.Helper()
	metadata := []byte("{}")
	if c.metadata != nil {
		var err error
		metadata, err = json.Marshal(c.metadata)
		require.NoError(t, err)
	}
	thread, body, peer := c.thread, c.body, c.peer
	_, err := e.comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{Source: c.source, ExternalID: c.external, ThreadID: &thread, Body: &body, PeerHandle: &peer, PeerNormalized: &peer, Direction: c.direction, SentAt: c.at, SourceMetadata: metadata, MatchedContactID: c.contact})
	require.NoError(t, err)
}

// telegram stores one Telegram message from peer (0 for the user's own).
func (e *builderEnv) telegram(t *testing.T, chat int64, chatType string, msg int32, peer int64, first string, contact *uuid.UUID, at time.Time) {
	t.Helper()
	text := "message " + first
	params := db.InsertFullTelegramMessageForTestParams{TelegramMessageID: msg, TelegramChatID: chat, ChatType: chatType, MessageText: &text, MessageType: "text", SentAt: at, IsOutgoing: peer == 0, MatchedContactID: contact}
	if peer != 0 {
		params.PeerUserID, params.PeerFirstName = &peer, &first
	}
	_, err := e.database.Queries.InsertFullTelegramMessageForTest(e.ctx, params)
	require.NoError(t, err)
}

func (e *builderEnv) build(t *testing.T, thread threadctx.VenueID, cutOff time.Time) threadctx.Context {
	t.Helper()
	c, err := e.builder.Build(e.ctx, thread, cutOff)
	require.NoError(t, err)
	return c
}

func roleOf(t *testing.T, c threadctx.Context, contact uuid.UUID) threadctx.Role {
	t.Helper()
	for role, p := range c.Participants {
		if p.ContactID != nil && *p.ContactID == contact {
			return role
		}
	}
	t.Fatalf("no participant carries contact %s", contact)
	return ""
}

func roles(c threadctx.Context) []threadctx.Role {
	out := make([]threadctx.Role, len(c.Entries))
	for i, entry := range c.Entries {
		out[i] = entry.Role
	}
	return out
}

func texts(c threadctx.Context) []string {
	out := make([]string, len(c.Entries))
	for i, entry := range c.Entries {
		out[i] = entry.Text
	}
	return out
}

// A thread context holds every message at or before the cut-off once,
// however many per-contact copies the store keeps, and nothing after it.
func TestBuild_EachMessageOnceUpToCutOff(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)

	t.Run("a group message stored once per member contact", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		space := "spaces/" + uuid.NewString()
		thread := e.venue(t, repository.InteractionSourceGChat, repository.VenueKindGroupChat, space)
		mine := uuid.NewString()
		for _, member := range []uuid.UUID{a, b} {
			e.store(t, commsCopy{source: repository.InteractionSourceGChat, thread: space, external: mine, direction: repository.InteractionDirectionOutbound, peer: "me@example.test", body: "lunch on Friday?", contact: member, at: e.at(1)})
		}
		e.store(t, commsCopy{source: repository.InteractionSourceGChat, thread: space, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "a@example.test", body: "yes", contact: a, at: e.at(2)})

		c := e.build(t, thread, e.at(2))
		assert.Equal(t, threadctx.KindConversation, c.Kind)
		assert.Equal(t, []string{"lunch on Friday?", "yes"}, texts(c))
		assert.Equal(t, []threadctx.Role{threadctx.RoleMe, roleOf(t, c, a)}, roles(c))
		roleOf(t, c, b)
	})

	t.Run("an email thread stored per recipient", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		gmailThread := "thread-" + uuid.NewString()
		thread := e.venue(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, gmailThread)
		sent := uuid.NewString()
		headers := map[string]any{"from": "me@example.test", "to": []string{"a@example.test", "b@example.test"}}
		e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: sent, direction: repository.InteractionDirectionOutbound, peer: "a@example.test", body: "agenda attached", contact: a, at: e.at(1), metadata: headers})
		e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: sent, direction: repository.InteractionDirectionOutbound, peer: "b@example.test", body: "agenda attached", contact: b, at: e.at(1), metadata: headers})
		e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "b@example.test", body: "thanks", contact: b, at: e.at(2), metadata: map[string]any{"from": "b@example.test", "to": []string{"me@example.test"}}})

		c := e.build(t, thread, e.at(2))
		assert.Equal(t, []string{"agenda attached", "thanks"}, texts(c))
		assert.Equal(t, []threadctx.Role{threadctx.RoleMe, roleOf(t, c, b)}, roles(c))
		roleOf(t, c, a)
	})

	t.Run("a message exactly at the cut-off and one a second after it", func(t *testing.T) {
		a := e.contact(t)
		chat := "chat-" + uuid.NewString() + "@s.whatsapp.net"
		thread := e.venue(t, repository.InteractionSourceWhatsApp, repository.VenueKindDM, chat)
		for i, body := range []string{"before", "at the cut-off", "a second after"} {
			e.store(t, commsCopy{source: repository.InteractionSourceWhatsApp, thread: chat, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "a@s.whatsapp.net", body: body, contact: a, at: e.at(9 + i)})
		}

		c := e.build(t, thread, e.at(10))
		assert.Equal(t, []string{"before", "at the cut-off"}, texts(c))
		assert.True(t, e.at(10).Equal(c.CutOff))
	})
}

// Participants are labeled by role from source identity, never from
// display names; each keeps one role across the context with its contact or none.
func TestBuild_RolesFromSourceIdentity(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	group := func(t *testing.T) (int64, threadctx.VenueID) {
		chat := -rand.Int64N(1 << 40)
		return chat, e.venue(t, repository.InteractionSourceTelegram, repository.VenueKindGroupChat, strconv.FormatInt(chat, 10))
	}

	t.Run("two participants sharing a display name", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		chat, thread := group(t)
		e.telegram(t, chat, "group", 1, 101, "Sam", &a, e.at(1))
		e.telegram(t, chat, "group", 2, 102, "Sam", &b, e.at(2))
		e.telegram(t, chat, "group", 3, 0, "", nil, e.at(3))

		c := e.build(t, thread, e.at(3))
		roleA, roleB := roleOf(t, c, a), roleOf(t, c, b)
		assert.NotEqual(t, roleA, roleB)
		assert.Equal(t, []threadctx.Role{roleA, roleB, threadctx.RoleMe}, roles(c))
		assert.True(t, c.Participants[threadctx.RoleMe].IsUser)
	})

	t.Run("a participant whose display name changes mid-thread", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		chat, thread := group(t)
		e.telegram(t, chat, "group", 1, 201, "Sam", &a, e.at(1))
		e.telegram(t, chat, "group", 2, 202, "Robin", &b, e.at(2))
		e.telegram(t, chat, "group", 3, 201, "Samuel", &a, e.at(3))

		c := e.build(t, thread, e.at(3))
		roleA := roleOf(t, c, a)
		assert.Equal(t, []threadctx.Role{roleA, roleOf(t, c, b), roleA}, roles(c))
		assert.Len(t, c.Participants, 3)
	})

	t.Run("a participant who is not a CRM contact", func(t *testing.T) {
		a := e.contact(t)
		chat, thread := group(t)
		e.telegram(t, chat, "group", 1, 301, "Ari", &a, e.at(1))
		e.telegram(t, chat, "group", 2, 302, "Lee", nil, e.at(2))
		e.telegram(t, chat, "group", 3, 302, "Lee", nil, e.at(3))

		c := e.build(t, thread, e.at(3))
		stranger := c.Entries[1].Role
		assert.Equal(t, []threadctx.Role{roleOf(t, c, a), stranger, stranger}, roles(c))
		assert.NotEqual(t, threadctx.RoleMe, stranger)
		assert.Nil(t, c.Participants[stranger].ContactID)
		assert.False(t, c.Participants[stranger].IsUser)
	})
}

// The input hash is equal for equal content up to the cut-off, changes
// when that content changes, and ignores content after the cut-off.
func TestBuild_InputHash(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	a := e.contact(t)
	space := "spaces/" + uuid.NewString()
	thread := e.venue(t, repository.InteractionSourceGChat, repository.VenueKindGroupChat, space)
	edited := uuid.NewString()
	e.store(t, commsCopy{source: repository.InteractionSourceGChat, thread: space, external: edited, direction: repository.InteractionDirectionInbound, peer: "a@example.test", body: "see you at 6", contact: a, at: e.at(1)})
	e.store(t, commsCopy{source: repository.InteractionSourceGChat, thread: space, external: uuid.NewString(), direction: repository.InteractionDirectionOutbound, peer: "me@example.test", body: "great", contact: a, at: e.at(2)})
	before := e.build(t, thread, e.at(2)).InputHash
	require.NotEmpty(t, before)

	t.Run("an unchanged rebuild", func(t *testing.T) {
		assert.Equal(t, before, e.build(t, thread, e.at(2)).InputHash)
	})

	t.Run("a new message after the cut-off", func(t *testing.T) {
		e.store(t, commsCopy{source: repository.InteractionSourceGChat, thread: space, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "a@example.test", body: "running late", contact: a, at: e.at(3)})
		assert.Equal(t, before, e.build(t, thread, e.at(2)).InputHash)
	})

	t.Run("an edited message before the cut-off", func(t *testing.T) {
		body := "see you at 7"
		rows, err := e.comms.ApplyEditByExternalID(e.ctx, repository.InteractionSourceGChat, edited, &body, &body, e.at(4).Format(time.RFC3339), e.at(4).Format(time.RFC3339))
		require.NoError(t, err)
		require.EqualValues(t, 1, rows)
		assert.NotEqual(t, before, e.build(t, thread, e.at(2)).InputHash)
	})
}

// The input hash changes when a participant is matched, rematched or
// unmatched, since its contact decides its questions and where their outcomes
// land, and returns to its earlier value when content and bindings do.
func TestBuild_InputHashFollowsContactBindings(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	comms := repository.NewCommsMessageRepository(e.database.Queries)
	comms.SetPool(e.database.Pool)
	contacts := repository.NewContactRepository(e.database.Queries)
	chat := "chat-" + uuid.NewString() + "@s.whatsapp.net"
	peer := uuid.NewString()[:12] + "@s.whatsapp.net"
	thread := e.venue(t, repository.InteractionSourceWhatsApp, repository.VenueKindDM, chat)
	for i, direction := range []string{repository.InteractionDirectionInbound, repository.InteractionDirectionOutbound} {
		body := "see you at 6"
		_, err := comms.UpsertChatMessage(e.ctx, repository.UpsertChatMessageParams{Source: repository.InteractionSourceWhatsApp, ExternalID: uuid.NewString(), ThreadID: chat, Body: &body, PeerHandle: &peer, Direction: direction, SentAt: e.at(i + 1)})
		require.NoError(t, err)
	}
	unmatched := e.build(t, thread, e.at(2))
	require.Nil(t, unmatched.Participants[threadctx.RoleThem].ContactID)

	a, b := e.contact(t), e.contact(t)
	var matched string
	t.Run("a participant matched after a judgment", func(t *testing.T) {
		attached, _, err := comms.AttachUnmatchedByPeer(e.ctx, repository.InteractionSourceWhatsApp, nil, &peer, a)
		require.NoError(t, err)
		require.EqualValues(t, 2, attached)
		matched = e.build(t, thread, e.at(2)).InputHash
		assert.NotEqual(t, unmatched.InputHash, matched)
	})

	t.Run("a participant rematched after a judgment", func(t *testing.T) {
		tx, err := e.database.Pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		require.NoError(t, comms.RepointContactForMergeTx(e.ctx, tx, a, b))
		require.NoError(t, tx.Commit(e.ctx))
		assert.NotEqual(t, matched, e.build(t, thread, e.at(2)).InputHash)
	})

	t.Run("a participant unmatched after a judgment", func(t *testing.T) {
		require.NoError(t, contacts.SoftDeleteContact(e.ctx, b))
		assert.Equal(t, unmatched.InputHash, e.build(t, thread, e.at(2)).InputHash)
	})
}

// A thread's latest content time and the threads that gained content in
// a window agree with the contexts built at those cut-offs.
func TestContentFacts_AgreeWithContexts(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	after, upTo := e.at(100), e.at(200)
	conversation := func(t *testing.T, at ...time.Time) threadctx.VenueID {
		a := e.contact(t)
		chat := "chat-" + uuid.NewString() + "@s.whatsapp.net"
		thread := e.venue(t, repository.InteractionSourceWhatsApp, repository.VenueKindDM, chat)
		for _, instant := range at {
			e.store(t, commsCopy{source: repository.InteractionSourceWhatsApp, thread: chat, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "a@s.whatsapp.net", body: "hello", contact: a, at: instant})
		}
		return thread
	}
	// agree checks both facts against the context built at upTo and reports
	// whether the thread is in the window.
	agree := func(t *testing.T, thread threadctx.VenueID) bool {
		t.Helper()
		threads, err := e.builder.ThreadsWithContentIn(e.ctx, after, upTo)
		require.NoError(t, err)
		inWindow := slices.Contains(threads, thread)
		c := e.build(t, thread, upTo)
		gained := slices.ContainsFunc(c.Entries, func(entry threadctx.Entry) bool { return entry.SentAt.After(after) })
		assert.Equal(t, gained, inWindow, "window membership agrees with the context at its upper bound")
		latest, ok, err := e.builder.LatestContentAt(e.ctx, thread, upTo)
		require.NoError(t, err)
		require.Equal(t, len(c.Entries) > 0, ok)
		if ok {
			assert.True(t, c.Entries[len(c.Entries)-1].SentAt.Equal(latest), "latest content time is the context's last entry")
		}
		return inWindow
	}

	t.Run("content exactly at the window's upper bound", func(t *testing.T) {
		assert.True(t, agree(t, conversation(t, e.at(50), upTo)))
	})

	t.Run("content exactly at its lower bound", func(t *testing.T) {
		assert.False(t, agree(t, conversation(t, e.at(50), after)))
	})

	t.Run("a thread with content only before the window", func(t *testing.T) {
		thread := conversation(t, e.at(50), e.at(60), e.at(201))
		assert.False(t, agree(t, thread))
		latest, ok, err := e.builder.LatestContentAt(e.ctx, thread, upTo)
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, e.at(60).Equal(latest))
	})

	t.Run("a meeting venue with notes in the window", func(t *testing.T) {
		session := uuid.New()
		thread := e.venue(t, repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, session.String())
		title, summary := "Planning", "agreed on the venue"
		tx, err := e.database.Pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		_, err = e.notes.InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{AnarlogSessionID: session, Title: &title, Summary: &summary, LinkageState: repository.LinkageStateOrphanNeedsReview, MeetingAt: e.at(150)})
		require.NoError(t, err)
		require.NoError(t, tx.Commit(e.ctx))

		assert.True(t, agree(t, thread))
		c := e.build(t, thread, upTo)
		assert.Equal(t, threadctx.KindMeeting, c.Kind)
		assert.Equal(t, []string{"agreed on the venue"}, texts(c))
	})

	// The meeting venue comes from the resolver that records calendar
	// interactions, so the view's calendar venue key is checked against it.
	t.Run("a calendar meeting with notes in the window", func(t *testing.T) {
		calendar := repository.NewCalendarEventRepository(e.database.Queries)
		title := "Planning | review"
		event, err := calendar.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: "event|" + uuid.NewString(), GcalCalendarID: "calendar:" + uuid.NewString(), GoogleAccountID: "account-" + uuid.NewString(), Title: &title, StartTime: e.at(140), EndTime: e.at(160), Status: "confirmed", SyncedAt: e.at(140)})
		require.NoError(t, err)
		tx, err := e.database.Pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		thread, err := repository.NewVenueResolverRegistry(e.venues, nil, calendar).ResolveGCalVenueTx(e.ctx, tx, event.ID)
		require.NoError(t, err)
		require.NotNil(t, thread)
		summary, linked := "agreed on the agenda", "event"
		_, err = e.notes.InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{AnarlogSessionID: uuid.New(), Summary: &summary, LinkedKind: &linked, LinkedID: &event.ID, LinkageState: repository.LinkageStateLinked, MeetingAt: e.at(150)})
		require.NoError(t, err)
		require.NoError(t, tx.Commit(e.ctx))

		assert.True(t, agree(t, *thread))
		c := e.build(t, *thread, upTo)
		assert.Equal(t, threadctx.KindMeeting, c.Kind)
		assert.Equal(t, []string{"agreed on the agenda"}, texts(c))
	})
}

// The names the source gives the recipient of the user's own message are
// masked as that recipient's role.
func TestMask_RecipientNamesFromTheSource(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	masker, err := threadctx.LoadMasker(e.ctx, repository.NewThreadContentRepository(e.database.Queries))
	require.NoError(t, err)
	maskOnly := func(t *testing.T, thread threadctx.VenueID) threadctx.MaskedRequest {
		t.Helper()
		masked, err := masker.Mask(e.ctx, e.build(t, thread, e.at(1)), nil)
		require.NoError(t, err)
		return masked
	}
	// toTelegram stores the user's own message in a private chat with peer.
	toTelegram := func(t *testing.T, contact *uuid.UUID) threadctx.VenueID {
		t.Helper()
		peer := rand.Int64N(1 << 40)
		thread := e.venue(t, repository.InteractionSourceTelegram, repository.VenueKindDM, strconv.FormatInt(peer, 10))
		text, first := "Zerelia can do Friday", "Zerelia"
		_, err := e.database.Queries.InsertFullTelegramMessageForTest(e.ctx, db.InsertFullTelegramMessageForTestParams{TelegramMessageID: 1, TelegramChatID: peer, ChatType: "private", MessageText: &text, MessageType: "text", SentAt: e.at(1), IsOutgoing: true, MatchedContactID: contact, PeerUserID: &peer, PeerFirstName: &first})
		require.NoError(t, err)
		return thread
	}

	t.Run("an outgoing Telegram message to a contact", func(t *testing.T) {
		a := e.contact(t)
		masked := maskOnly(t, toTelegram(t, &a))
		assert.Equal(t, "THEM can do Friday", masked.Entries()[0].Text)
	})

	t.Run("an outgoing Telegram message to someone who is not a contact", func(t *testing.T) {
		masked := maskOnly(t, toTelegram(t, nil))
		assert.Equal(t, "THEM can do Friday", masked.Entries()[0].Text)
	})

	t.Run("an email recipient named in the address headers", func(t *testing.T) {
		a := e.contact(t)
		gmailThread := "thread-" + uuid.NewString()
		thread := e.venue(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, gmailThread)
		e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: uuid.NewString(), direction: repository.InteractionDirectionOutbound, peer: "z.q@example.test", body: "Zerelia, the agenda is attached", contact: a, at: e.at(1), metadata: map[string]any{"from": "me@example.test", "to": []string{"z.q@example.test"}, "to_names": []string{"Zerelia Quill"}}})

		masked := maskOnly(t, thread)
		assert.Equal(t, "THEM, the agenda is attached", masked.Entries()[0].Text)
		assertGone(t, masked, "zerelia", "quill", "z.q@")
	})
}

// The masker built from the CRM masks a stored contact's name.
func TestLoadMasker_MasksStoredContactNames(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	contact, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	masker, err := threadctx.LoadMasker(e.ctx, repository.NewThreadContentRepository(e.database.Queries))
	require.NoError(t, err)

	// Lower case at the start of the text, where only the dictionary masks it.
	c := threadctx.Context{Kind: threadctx.KindConversation, Entries: []threadctx.Entry{{Role: threadctx.RoleMe, Text: strings.ToLower(contact.FullName) + " asked about it"}}}
	masked, err := masker.Mask(e.ctx, c, nil)
	require.NoError(t, err)
	text := strings.ToLower(masked.Entries()[0].Text)
	for _, word := range strings.Fields(strings.ToLower(contact.FullName)) {
		assert.NotContains(t, text, word)
	}
}
