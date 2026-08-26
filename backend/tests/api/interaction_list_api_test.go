//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type interactionAPIDB struct {
	ctx             context.Context
	database        *db.Database
	contact         *repository.Contact
	interactionRepo *repository.InteractionRepository
	commsRepo       *repository.CommsMessageRepository
	telegramRepo    *repository.TelegramMessageRepository
	messagesRepo    *repository.MessagesMessageRepository
	eventRepo       *repository.CalendarEventRepository
	phoneRepo       *repository.PhoneCallRepository
	noteRepo        *repository.MeetingNoteRepository
	router          *gin.Engine
}

func strptr(value string) *string { return &value }

func newInteractionAPITest(t *testing.T) *interactionAPIDB {
	t.Helper()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Synthetic interaction contact"})
	require.NoError(t, err)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	telegramRepo := repository.NewTelegramMessageRepository(database.Queries)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)
	eventRepo := repository.NewCalendarEventRepository(database.Queries)
	phoneRepo := repository.NewPhoneCallRepository(database.Queries)
	noteRepo := repository.NewMeetingNoteRepository(database.Queries)
	content := service.NewInteractionContentService(interactionRepo, commsRepo, telegramRepo, messagesRepo, noteRepo, eventRepo, phoneRepo, repository.NewContactRepository(database.Queries))
	handler := handlers.NewInteractionHandler(interactionRepo, nil, content)
	router := gin.New()
	handlers.RegisterContactRoutes(router.Group("/api/v1"), handlers.ContactRouteDeps{
		Contact: &handlers.ContactHandler{}, Interaction: handler, Note: &handlers.NoteHandler{}, ContactMethod: &handlers.ContactMethodHandler{},
	})
	return &interactionAPIDB{ctx: ctx, database: database, contact: contact, interactionRepo: interactionRepo, commsRepo: commsRepo, telegramRepo: telegramRepo, messagesRepo: messagesRepo, eventRepo: eventRepo, phoneRepo: phoneRepo, noteRepo: noteRepo, router: router}
}

func seedAPIInteraction(t *testing.T, e *interactionAPIDB, source string, at time.Time, description *string) repository.Interaction {
	return seedAPIInteractionWithRef(t, e, uuid.New(), source, nil, at, description)
}

func seedAPIInteractionWithID(t *testing.T, e *interactionAPIDB, id uuid.UUID, source string, at time.Time, description *string) repository.Interaction {
	return seedAPIInteractionWithRef(t, e, id, source, nil, at, description)
}

func seedAPIInteractionWithRef(t *testing.T, e *interactionAPIDB, id uuid.UUID, source string, sourceRef *string, at time.Time, description *string) repository.Interaction {
	t.Helper()
	row, err := e.interactionRepo.TestInsertInteraction(e.ctx, id, e.contact.ID, source, sourceRef, at, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	if description != nil {
		row, err = e.interactionRepo.UpdateInteractionTimestamp(e.ctx, row.ID, at, description)
		require.NoError(t, err)
	}
	return *row
}

func seedAPIComms(t *testing.T, e *interactionAPIDB, interactionID uuid.UUID, source, thread, external string, at time.Time) repository.CommsMessage {
	t.Helper()
	subject, body, peer := "Synthetic subject", "Synthetic body", "synthetic-peer"
	return seedAPICommsWith(t, e, interactionID, source, thread, external, at, &subject, &body, &peer, repository.InteractionDirectionInbound, nil)
}

func seedAPICommsWith(t *testing.T, e *interactionAPIDB, interactionID uuid.UUID, source, thread, external string, at time.Time, subject, body, peer *string, direction string, metadata []byte) repository.CommsMessage {
	t.Helper()
	row, err := e.commsRepo.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{Source: source, ExternalID: external, ThreadID: &thread, Subject: subject, Body: body, PeerHandle: peer, Direction: direction, SentAt: at, SourceMetadata: metadata, MatchedContactID: e.contact.ID})
	require.NoError(t, err)
	require.NoError(t, e.commsRepo.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interactionID))
	return *row
}

func seedAPICommsUnlinked(t *testing.T, e *interactionAPIDB, source, thread, external string, at time.Time, body, peer string) repository.CommsMessage {
	t.Helper()
	row, err := e.commsRepo.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{Source: source, ExternalID: external, ThreadID: &thread, Body: &body, PeerHandle: &peer, Direction: repository.InteractionDirectionInbound, SentAt: at, MatchedContactID: e.contact.ID})
	require.NoError(t, err)
	return *row
}

func seedAPITelegram(t *testing.T, e *interactionAPIDB, interactionID uuid.UUID, chatID int64, msgID int32, at time.Time, chatType, title, body string, outgoing bool, username, first, last *string) repository.TelegramMessage {
	t.Helper()
	row, err := e.telegramRepo.UpsertMessage(e.ctx, repository.UpsertTelegramMessageParams{TelegramMessageID: msgID, TelegramChatID: chatID, ChatType: chatType, ChatTitle: &title, MessageText: &body, MessageType: "text", SentAt: at, IsOutgoing: outgoing, PeerUsername: username, PeerFirstName: first, PeerLastName: last})
	require.NoError(t, err)
	require.NoError(t, e.telegramRepo.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interactionID))
	return *row
}

func seedAPIMessages(t *testing.T, e *interactionAPIDB, interactionID uuid.UUID, chat, guid string, at time.Time, group, outgoing bool, text *string) repository.MessagesMessage {
	t.Helper()
	row, err := e.messagesRepo.UpsertMessage(e.ctx, repository.UpsertMessagesMessageParams{Guid: guid, ChatGuid: chat, PeerHandle: "synthetic-peer", Text: text, MessageType: "text", SentAt: at, IsOutgoing: outgoing, IsGroupChat: group, MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	require.NoError(t, e.messagesRepo.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interactionID))
	return *row
}

func seedAPICall(t *testing.T, e *interactionAPIDB, interactionID uuid.UUID, at time.Time) repository.PhoneCall {
	t.Helper()
	answered := true
	row, err := e.phoneRepo.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{CallUniqueID: "synthetic-call-" + interactionID.String(), PeerHandle: "synthetic-peer", PeerNormalized: "synthetic-peer", Service: repository.PhoneCallServiceFaceTimeVideo, Direction: repository.PhoneCallDirectionInbound, Answered: &answered, HasVoicemail: true, DurationSeconds: 47, StartedAt: at, MatchedContactID: &e.contact.ID})
	require.NoError(t, err)
	require.NoError(t, e.phoneRepo.MarkProcessed(e.ctx, repository.MarkProcessedParams{ID: row.ID, InteractionID: &interactionID}))
	return *row
}

func seedAPINote(t *testing.T, e *interactionAPIDB, sessionID uuid.UUID, linkedID *uuid.UUID, title, summary, memo string) repository.MeetingNote {
	t.Helper()
	var linkedKind *string
	if linkedID != nil {
		kind := repository.LinkedKindEvent
		linkedKind = &kind
	}
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer tx.Rollback(e.ctx)
	row, err := e.noteRepo.InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{AnarlogSessionID: sessionID, Title: &title, Summary: &summary, Memo: &memo, LinkedKind: linkedKind, LinkedID: linkedID, LinkageState: repository.LinkageStateLinked, InputHash: "", ResolvedSetHash: "", MeetingAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return *row
}

func seedAPIEvent(t *testing.T, e *interactionAPIDB, at time.Time, title, location, link string) repository.CalendarEvent {
	t.Helper()
	titleValue, locationValue, linkValue := title, location, link
	event, err := e.eventRepo.Upsert(e.ctx, repository.UpsertCalendarEventRequest{GcalEventID: "synthetic-event-" + title, GcalCalendarID: "synthetic-calendar", GoogleAccountID: "synthetic-account", Title: &titleValue, Location: &locationValue, Attendees: []repository.Attendee{{Email: "attendee-a@example.test"}, {Email: "attendee-b@example.test"}, {Email: "attendee-c@example.test"}}, StartTime: at.Add(time.Hour), EndTime: at.Add(2 * time.Hour), Status: "confirmed", SyncedAt: at, HtmlLink: &linkValue, MatchedContactIDs: []uuid.UUID{e.contact.ID}})
	require.NoError(t, err)
	return *event
}

func getInteractionList(t *testing.T, e *interactionAPIDB, query string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts/"+e.contact.ID.String()+"/interactions"+query, nil)
	e.router.ServeHTTP(w, req)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return w.Code, envelope
}

// spec: IXN-013.enriched-row-fields
// spec: IXN-013.calendar-event-summary
// spec: IXN-013.call-summary
func TestInteractionListAPI_EnrichedRows(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	description := "description precedence"
	manual := seedAPIInteraction(t, e, repository.InteractionSourceManual, when, nil)
	email := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(time.Minute), nil)
	seedAPICommsWith(t, e, email.ID, repository.InteractionSourceEmail, "email-thread", "email-message", when, strptr("Email subject"), strptr("Email body"), strptr("email-peer"), repository.InteractionDirectionInbound, nil)
	gchat := seedAPIInteraction(t, e, repository.InteractionSourceGChat, when.Add(2*time.Minute), nil)
	seedAPIComms(t, e, gchat.ID, repository.InteractionSourceGChat, "gchat-room", "gchat-message", when)
	whatsappGroup := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(3*time.Minute), nil)
	seedAPIComms(t, e, whatsappGroup.ID, repository.InteractionSourceWhatsApp, "15550001@g.us", "whatsapp-group", when)
	whatsappDM := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(4*time.Minute), nil)
	seedAPIComms(t, e, whatsappDM.ID, repository.InteractionSourceWhatsApp, "15550002", "whatsapp-dm", when)
	telegram := seedAPIInteraction(t, e, repository.InteractionSourceTelegram, when.Add(5*time.Minute), nil)
	seedAPITelegram(t, e, telegram.ID, 7101, 1, when, "group", "Telegram room", "Telegram body", false, nil, nil, nil)
	messages := seedAPIInteraction(t, e, repository.InteractionSourceMessages, when.Add(6*time.Minute), nil)
	seedAPIMessages(t, e, messages.ID, "messages-room", "messages-1", when, true, false, strptr("Messages body"))
	event := seedAPIEvent(t, e, when, "Calendar title", "Calendar room", "https://calendar.example.test/synthetic-event")
	eventRef := event.ID.String()
	gcalWithNote := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceGCal, &eventRef, when.Add(7*time.Minute), nil)
	_ = seedAPINote(t, e, uuid.New(), &event.ID, "Calendar note", "Calendar summary", "Calendar memo")
	session := uuid.New()
	anarlogRef := "anarlog:" + session.String() + ":" + e.contact.ID.String()
	_ = seedAPINote(t, e, session, nil, "Anarlog title", "Anarlog summary", "Anarlog memo")
	anarlog := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceAnarlogSessions, &anarlogRef, when.Add(9*time.Minute), nil)
	_ = anarlog
	eventWithoutNote := seedAPIEvent(t, e, when.Add(time.Minute), "Calendar no-note", "Calendar room 2", "https://calendar.example.test/synthetic-no-note")
	eventWithoutNoteRef := eventWithoutNote.ID.String()
	gcalWithoutNote := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceGCal, &eventWithoutNoteRef, when.Add(8*time.Minute), &description)
	call := seedAPIInteraction(t, e, repository.InteractionSourcePhoneCalls, when.Add(10*time.Minute), nil)
	seedAPICall(t, e, call.ID, when)
	todo := seedAPIInteraction(t, e, repository.InteractionSourceTodoist, when.Add(11*time.Minute), nil)
	_ = todo
	// The source-ref-bearing rows are created through the repository's create
	// path below; this keeps the fixture on the same write boundary as callers.
	_ = eventRef

	status, envelope := getInteractionList(t, e, "")
	require.Equal(t, http.StatusOK, status)
	data := envelope["data"].(map[string]any)
	items := data["items"].([]any)
	require.Len(t, items, 12)
	bySource := map[string][]map[string]any{}
	for _, raw := range items {
		row := raw.(map[string]any)
		for _, key := range []string{"id", "contact_id", "source", "occurred_at", "direction", "created_at", "label", "content_kind", "message_count", "is_group", "venue_tags"} {
			assert.Contains(t, row, key)
		}
		bySource[row["source"].(string)] = append(bySource[row["source"].(string)], row)
	}
	assert.Equal(t, "Logged interaction", bySource[repository.InteractionSourceManual][0]["label"])
	_ = manual
	assert.Equal(t, "none", bySource[repository.InteractionSourceManual][0]["content_kind"])
	assert.Equal(t, float64(0), bySource[repository.InteractionSourceManual][0]["message_count"])
	assert.False(t, bySource[repository.InteractionSourceManual][0]["is_group"].(bool))
	assert.Equal(t, []any{}, bySource[repository.InteractionSourceManual][0]["venue_tags"])
	assert.Equal(t, "Email subject", bySource[repository.InteractionSourceEmail][0]["label"])
	assert.Equal(t, "messages", bySource[repository.InteractionSourceEmail][0]["content_kind"])
	assert.Equal(t, float64(1), bySource[repository.InteractionSourceEmail][0]["message_count"])
	emailTag := bySource[repository.InteractionSourceEmail][0]["venue_tags"].([]any)
	require.Len(t, emailTag, 1)
	assert.Equal(t, "email:email-thread", emailTag[0].(map[string]any)["key"])
	assert.Equal(t, "Email subject", emailTag[0].(map[string]any)["label"])
	assert.Equal(t, "email_thread", emailTag[0].(map[string]any)["kind"])
	assert.False(t, emailTag[0].(map[string]any)["is_group"].(bool))
	assert.False(t, bySource[repository.InteractionSourceEmail][0]["is_group"].(bool))
	gchatTag := bySource[repository.InteractionSourceGChat][0]["venue_tags"].([]any)
	require.Len(t, gchatTag, 1)
	assert.Equal(t, "gchat:gchat-room", gchatTag[0].(map[string]any)["key"])
	assert.Equal(t, "Group chat", gchatTag[0].(map[string]any)["label"])
	assert.Equal(t, "group_chat", gchatTag[0].(map[string]any)["kind"])
	assert.True(t, gchatTag[0].(map[string]any)["is_group"].(bool))
	assert.Equal(t, "messages", bySource[repository.InteractionSourceGChat][0]["content_kind"])
	assert.Equal(t, float64(1), bySource[repository.InteractionSourceGChat][0]["message_count"])
	assert.True(t, bySource[repository.InteractionSourceGChat][0]["is_group"].(bool))
	var whatsappGroupRow, whatsappDMRow map[string]any
	for _, row := range bySource[repository.InteractionSourceWhatsApp] {
		if row["is_group"].(bool) {
			whatsappGroupRow = row
		} else {
			whatsappDMRow = row
		}
	}
	require.NotNil(t, whatsappGroupRow)
	require.NotNil(t, whatsappDMRow)
	whatsappGroupTag := whatsappGroupRow["venue_tags"].([]any)
	require.Len(t, whatsappGroupTag, 1)
	assert.Equal(t, "whatsapp:15550001@g.us", whatsappGroupTag[0].(map[string]any)["key"])
	assert.Equal(t, "Group chat", whatsappGroupTag[0].(map[string]any)["label"])
	assert.Equal(t, "group_chat", whatsappGroupTag[0].(map[string]any)["kind"])
	assert.True(t, whatsappGroupTag[0].(map[string]any)["is_group"].(bool))
	assert.Equal(t, "messages", whatsappGroupRow["content_kind"])
	assert.Equal(t, float64(1), whatsappGroupRow["message_count"])
	whatsappDMTag := whatsappDMRow["venue_tags"].([]any)
	require.Len(t, whatsappDMTag, 1)
	assert.Equal(t, "whatsapp:15550002", whatsappDMTag[0].(map[string]any)["key"])
	assert.Equal(t, "DM", whatsappDMTag[0].(map[string]any)["label"])
	assert.Equal(t, "dm", whatsappDMTag[0].(map[string]any)["kind"])
	assert.False(t, whatsappDMTag[0].(map[string]any)["is_group"].(bool))
	assert.Equal(t, "messages", whatsappDMRow["content_kind"])
	assert.Equal(t, float64(1), whatsappDMRow["message_count"])
	assert.False(t, whatsappDMRow["is_group"].(bool))
	assert.Equal(t, "Telegram room", bySource[repository.InteractionSourceTelegram][0]["label"])
	assert.Equal(t, "messages", bySource[repository.InteractionSourceTelegram][0]["content_kind"])
	assert.Equal(t, float64(1), bySource[repository.InteractionSourceTelegram][0]["message_count"])
	telegramTag := bySource[repository.InteractionSourceTelegram][0]["venue_tags"].([]any)
	require.Len(t, telegramTag, 1)
	assert.Equal(t, "telegram:7101", telegramTag[0].(map[string]any)["key"])
	assert.Equal(t, "Telegram room", telegramTag[0].(map[string]any)["label"])
	assert.Equal(t, "group_chat", telegramTag[0].(map[string]any)["kind"])
	assert.True(t, telegramTag[0].(map[string]any)["is_group"].(bool))
	assert.True(t, bySource[repository.InteractionSourceTelegram][0]["is_group"].(bool))
	assert.Equal(t, "Group chat", bySource[repository.InteractionSourceMessages][0]["label"])
	assert.Equal(t, "messages", bySource[repository.InteractionSourceMessages][0]["content_kind"])
	assert.Equal(t, float64(1), bySource[repository.InteractionSourceMessages][0]["message_count"])
	messagesTag := bySource[repository.InteractionSourceMessages][0]["venue_tags"].([]any)
	require.Len(t, messagesTag, 1)
	assert.Equal(t, "messages:messages-room", messagesTag[0].(map[string]any)["key"])
	assert.Equal(t, "Group chat", messagesTag[0].(map[string]any)["label"])
	assert.Equal(t, "group_chat", messagesTag[0].(map[string]any)["kind"])
	assert.True(t, messagesTag[0].(map[string]any)["is_group"].(bool))
	assert.True(t, bySource[repository.InteractionSourceMessages][0]["is_group"].(bool))
	assert.Equal(t, "description precedence", bySource[repository.InteractionSourceGCal][0]["label"])
	var eventRaw map[string]any
	var gcalWithNoteRow, gcalWithoutNoteRow map[string]any
	for _, row := range bySource[repository.InteractionSourceGCal] {
		if row["event"] != nil {
			eventRaw = row["event"].(map[string]any)
		}
		if row["label"] == "Calendar title" {
			gcalWithNoteRow = row
		} else {
			gcalWithoutNoteRow = row
		}
	}
	require.NotNil(t, gcalWithNoteRow)
	require.NotNil(t, gcalWithoutNoteRow)
	assert.Equal(t, "meeting_note", gcalWithNoteRow["content_kind"])
	assert.Equal(t, float64(0), gcalWithNoteRow["message_count"])
	assert.False(t, gcalWithNoteRow["is_group"].(bool))
	assert.Equal(t, []any{}, gcalWithNoteRow["venue_tags"])
	assert.Equal(t, "none", gcalWithoutNoteRow["content_kind"])
	assert.Equal(t, float64(0), gcalWithoutNoteRow["message_count"])
	assert.False(t, gcalWithoutNoteRow["is_group"].(bool))
	assert.Equal(t, []any{}, gcalWithoutNoteRow["venue_tags"])
	assert.Equal(t, "Calendar title", eventRaw["title"])
	assert.Equal(t, "Calendar room", eventRaw["location"])
	assert.Equal(t, float64(3), eventRaw["attendee_count"])
	assert.Equal(t, event.StartTime.Format(time.RFC3339Nano), eventRaw["start_time"])
	assert.Equal(t, event.EndTime.Format(time.RFC3339Nano), eventRaw["end_time"])
	assert.Equal(t, "https://calendar.example.test/synthetic-event", eventRaw["html_link"])
	callRaw := bySource[repository.InteractionSourcePhoneCalls][0]["call"].(map[string]any)
	assert.Equal(t, "Phone call", bySource[repository.InteractionSourcePhoneCalls][0]["label"])
	assert.Equal(t, "call", bySource[repository.InteractionSourcePhoneCalls][0]["content_kind"])
	assert.Equal(t, float64(0), bySource[repository.InteractionSourcePhoneCalls][0]["message_count"])
	assert.False(t, bySource[repository.InteractionSourcePhoneCalls][0]["is_group"].(bool))
	assert.Equal(t, []any{}, bySource[repository.InteractionSourcePhoneCalls][0]["venue_tags"])
	assert.Equal(t, "facetime_video", callRaw["service"])
	assert.Equal(t, true, callRaw["answered"])
	assert.Equal(t, true, callRaw["has_voicemail"])
	assert.Equal(t, float64(47), callRaw["duration_seconds"])
	assert.Equal(t, "Todoist task", bySource[repository.InteractionSourceTodoist][0]["label"])
	assert.Equal(t, "none", bySource[repository.InteractionSourceTodoist][0]["content_kind"])
	assert.Equal(t, float64(0), bySource[repository.InteractionSourceTodoist][0]["message_count"])
	assert.False(t, bySource[repository.InteractionSourceTodoist][0]["is_group"].(bool))
	assert.Equal(t, []any{}, bySource[repository.InteractionSourceTodoist][0]["venue_tags"])
	assert.Equal(t, "Meeting", bySource[repository.InteractionSourceAnarlogSessions][0]["label"])
	assert.Equal(t, "meeting_note", bySource[repository.InteractionSourceAnarlogSessions][0]["content_kind"])
	assert.Equal(t, float64(0), bySource[repository.InteractionSourceAnarlogSessions][0]["message_count"])
	assert.False(t, bySource[repository.InteractionSourceAnarlogSessions][0]["is_group"].(bool))
	assert.Equal(t, []any{}, bySource[repository.InteractionSourceAnarlogSessions][0]["venue_tags"])
	assert.Equal(t, []any{}, bySource[repository.InteractionSourceManual][0]["venue_tags"])
	assert.Equal(t, []any{}, bySource[repository.InteractionSourcePhoneCalls][0]["venue_tags"])
	assert.Nil(t, bySource[repository.InteractionSourceGChat][0]["event"])
	_ = gcalWithNote
	_ = gcalWithoutNote
}

// spec: IXN-013.venue-filter-matches-content
// spec: IXN-013.date-range-half-open
// spec: IXN-013.filtered-pagination-totals
// spec: IXN-013.unknown-venue-empty
// spec: IXN-013.venue-options-unfiltered
func TestInteractionListAPI_Filters(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	from, to := when, when.Add(2*time.Minute)
	atFrom := seedAPIInteraction(t, e, repository.InteractionSourceEmail, from, nil)
	seedAPIComms(t, e, atFrom.ID, repository.InteractionSourceEmail, "filter-thread", "filter-from", from)
	between := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(time.Minute), nil)
	seedAPIComms(t, e, between.ID, repository.InteractionSourceEmail, "filter-thread", "filter-between", between.OccurredAt)
	atTo := seedAPIInteraction(t, e, repository.InteractionSourceEmail, to, nil)
	seedAPIComms(t, e, atTo.ID, repository.InteractionSourceEmail, "filter-thread", "filter-to", to)
	otherVenue := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(time.Minute), nil)
	seedAPIComms(t, e, otherVenue.ID, repository.InteractionSourceEmail, "other-thread", "filter-other", when.Add(time.Minute))
	status, unfiltered := getInteractionList(t, e, "")
	require.Equal(t, http.StatusOK, status)
	allOptions := unfiltered["data"].(map[string]any)["venue_options"]
	status, filtered := getInteractionList(t, e, "?venue=email:filter-thread&from=2026-08-02T12:00:00Z&to=2026-08-02T12:02:00Z&limit=1")
	require.Equal(t, http.StatusOK, status)
	data := filtered["data"].(map[string]any)
	assert.Len(t, data["items"], 1)
	pagination := filtered["meta"].(map[string]any)["pagination"].(map[string]any)
	assert.Equal(t, float64(2), pagination["total"])
	assert.Equal(t, float64(2), pagination["pages"])
	assert.True(t, reflect.DeepEqual(allOptions, data["venue_options"]))
	// Equality at from is included, equality at to is excluded.
	assert.NotEqual(t, atTo.ID.String(), data["items"].([]any)[0].(map[string]any)["id"])
	status, filteredPage2 := getInteractionList(t, e, "?venue=email:filter-thread&from=2026-08-02T12:00:00Z&to=2026-08-02T12:02:00Z&limit=1&page=2")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, atFrom.ID.String(), filteredPage2["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["id"])
	status, unknown := getInteractionList(t, e, "?venue=email:no-such-thread")
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, unknown["data"].(map[string]any)["items"])
	assert.Equal(t, float64(0), unknown["meta"].(map[string]any)["pagination"].(map[string]any)["total"])
	assert.True(t, reflect.DeepEqual(allOptions, unknown["data"].(map[string]any)["venue_options"]))
	status, inverted := getInteractionList(t, e, "?from=2026-08-03T00:00:00Z&to=2026-08-02T00:00:00Z")
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, inverted["data"].(map[string]any)["items"])
}

// spec: IXN-013.invalid-params-400
func TestInteractionListAPI_Validation(t *testing.T) {
	e := newInteractionAPITest(t)
	for _, query := range []string{"?from=notatime", "?to=2026-13-99", "?venue=BADVENUE", "?venue=email", "?venue=Email:x", "?venue=email:", "?page=abc", "?page=0", "?limit=abc", "?limit=0", "?page=100000000&limit=100", "?page=9223372036854775807&limit=100"} {
		status, _ := getInteractionList(t, e, query)
		assert.Equal(t, http.StatusBadRequest, status, query)
	}
	status, envelope := getInteractionList(t, e, "?limit=101")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, float64(100), envelope["meta"].(map[string]any)["pagination"].(map[string]any)["limit"])
	status, envelope = getInteractionList(t, e, "?venue=email:part:two")
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, envelope["data"].(map[string]any)["items"])
	// A valid multi-colon venue must reach the repository unchanged.
	row := seedAPIInteraction(t, e, repository.InteractionSourceEmail, time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC), nil)
	seedAPIComms(t, e, row.ID, repository.InteractionSourceEmail, "part:two", "multi-colon", row.OccurredAt)
	status, envelope = getInteractionList(t, e, "?venue=email:part:two")
	assert.Equal(t, http.StatusOK, status)
	assert.Len(t, envelope["data"].(map[string]any)["items"], 1)
	status, _ = getInteractionList(t, e, "?page=1&limit=20")
	assert.Equal(t, http.StatusOK, status)
}

// spec: IXN-013.stable-order-tiebreaker
// spec: IXN-013.soft-deleted-excluded
func TestInteractionListAPI_OrderAndSoftDelete(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ids := []uuid.UUID{
		uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		uuid.MustParse("00000000-0000-0000-0000-000000000004"),
		uuid.MustParse("00000000-0000-0000-0000-000000000005"),
		uuid.MustParse("00000000-0000-0000-0000-000000000006"),
	}
	for _, id := range ids {
		seedAPIInteractionWithID(t, e, id, repository.InteractionSourceManual, when, nil)
	}
	deleted := ids[5]
	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(e.ctx, deleted))
	status, envelope := getInteractionList(t, e, "?from=2026-08-03T11:59:00Z&to=2026-08-03T12:01:00Z&limit=2&page=1")
	require.Equal(t, http.StatusOK, status)
	items := envelope["data"].(map[string]any)["items"].([]any)
	require.Len(t, items, 2)
	pageIDs := []string{items[0].(map[string]any)["id"].(string), items[1].(map[string]any)["id"].(string)}
	status, page2 := getInteractionList(t, e, "?from=2026-08-03T11:59:00Z&to=2026-08-03T12:01:00Z&limit=2&page=2")
	require.Equal(t, http.StatusOK, status)
	for _, raw := range page2["data"].(map[string]any)["items"].([]any) {
		pageIDs = append(pageIDs, raw.(map[string]any)["id"].(string))
	}
	status, page3 := getInteractionList(t, e, "?from=2026-08-03T11:59:00Z&to=2026-08-03T12:01:00Z&limit=2&page=3")
	require.Equal(t, http.StatusOK, status)
	for _, raw := range page3["data"].(map[string]any)["items"].([]any) {
		pageIDs = append(pageIDs, raw.(map[string]any)["id"].(string))
	}
	require.Equal(t, []string{ids[4].String(), ids[3].String(), ids[2].String(), ids[1].String(), ids[0].String()}, pageIDs)
	meta := envelope["meta"].(map[string]any)["pagination"].(map[string]any)
	assert.Equal(t, float64(5), meta["total"])
	assert.Equal(t, float64(3), meta["pages"])

	// A tombstoned content row is excluded from enrichment counts while its
	// live sibling remains visible.
	burst := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(-time.Hour), nil)
	live := seedAPIComms(t, e, burst.ID, repository.InteractionSourceEmail, "burst-thread", "burst-live", when.Add(-time.Hour))
	dead := seedAPIComms(t, e, burst.ID, repository.InteractionSourceEmail, "burst-thread", "burst-dead", when.Add(-time.Hour).Add(time.Second))
	require.NoError(t, e.commsRepo.SoftDeleteByID(e.ctx, dead.ID))
	_, burstEnvelope := getInteractionList(t, e, "?venue=email:burst-thread")
	burstRow := burstEnvelope["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
	_ = live
	assert.Equal(t, float64(1), burstRow["message_count"])

	// A tombstoned-only container is absent from both venue options and its
	// filtered result, while the live burst container remains available.
	only := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(-2*time.Hour), nil)
	onlyRow := seedAPIComms(t, e, only.ID, repository.InteractionSourceEmail, "dead-only-thread", "dead-only", when.Add(-2*time.Hour))
	require.NoError(t, e.commsRepo.SoftDeleteByID(e.ctx, onlyRow.ID))
	_, all := getInteractionList(t, e, "")
	options := all["data"].(map[string]any)["venue_options"].([]any)
	for _, option := range options {
		assert.NotEqual(t, "email:dead-only-thread", option.(map[string]any)["key"])
	}
	status, deadFilter := getInteractionList(t, e, "?venue=email:dead-only-thread")
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, deadFilter["data"].(map[string]any)["items"])
}
