//go:build integration_testdb

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getInteractionContent(t *testing.T, e *interactionAPIDB, id string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/interactions/"+id+"/content", nil)
	e.router.ServeHTTP(w, req)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return w.Code, body
}

func contentData(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	return body["data"].(map[string]any)
}

func assertEmptyArrays(t *testing.T, data map[string]any, messages, notes []any) {
	t.Helper()
	assert.Equal(t, messages, data["messages"])
	assert.Equal(t, notes, data["meeting_notes"])
}

// spec: IXN-014.kind-per-source
// spec: IXN-014.empty-arrays-not-null
func TestInteractionContentAPI_KindPerSource(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	empty := []any{}
	manual := seedAPIInteraction(t, e, repository.InteractionSourceManual, when, nil)
	todo := seedAPIInteraction(t, e, repository.InteractionSourceTodoist, when.Add(time.Minute), nil)
	call := seedAPIInteraction(t, e, repository.InteractionSourcePhoneCalls, when.Add(2*time.Minute), nil)
	seedAPICall(t, e, call.ID, when)
	email := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when.Add(3*time.Minute), nil)
	seedAPIComms(t, e, email.ID, repository.InteractionSourceEmail, "kind-email", "kind-email-message", when)
	telegram := seedAPIInteraction(t, e, repository.InteractionSourceTelegram, when.Add(4*time.Minute), nil)
	seedAPITelegram(t, e, telegram.ID, 7201, 1, when, "private", "", "telegram", false, strptr("tg-user"), nil, nil)
	messages := seedAPIInteraction(t, e, repository.InteractionSourceMessages, when.Add(5*time.Minute), nil)
	seedAPIMessages(t, e, messages.ID, "kind-messages", "kind-messages-message", when, false, false, strptr("messages"))
	whatsapp := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(6*time.Minute), nil)
	seedAPIComms(t, e, whatsapp.ID, repository.InteractionSourceWhatsApp, "7202", "kind-whatsapp", when)
	event := seedAPIEvent(t, e, when, "Kind event", "Kind location", "https://calendar.example.test/kind")
	eventRef := event.ID.String()
	gcalWithNote := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceGCal, &eventRef, when.Add(7*time.Minute), nil)
	seedAPINote(t, e, uuid.New(), &event.ID, "Kind note", "Kind summary", "Kind memo")
	eventWithoutNote := seedAPIEvent(t, e, when.Add(time.Minute), "Kind no-note", "Kind location 2", "https://calendar.example.test/kind-no-note")
	eventWithoutNoteRef := eventWithoutNote.ID.String()
	gcalWithoutNote := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceGCal, &eventWithoutNoteRef, when.Add(8*time.Minute), nil)
	session := uuid.New()
	anarlogRef := "anarlog:" + session.String() + ":" + e.contact.ID.String()
	anarlog := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceAnarlogSessions, &anarlogRef, when.Add(9*time.Minute), nil)
	seedAPINote(t, e, session, nil, "Session note", "Session summary", "Session memo")

	rows := []struct {
		id   uuid.UUID
		kind string
	}{
		{manual.ID, "none"}, {todo.ID, "none"}, {call.ID, "call"},
		{email.ID, "messages"}, {telegram.ID, "messages"}, {messages.ID, "messages"},
		{whatsapp.ID, "messages"}, {gcalWithNote.ID, "meeting_note"},
		{gcalWithoutNote.ID, "none"}, {anarlog.ID, "meeting_note"},
	}
	for _, tc := range rows {
		status, body := getInteractionContent(t, e, tc.id.String())
		require.Equal(t, http.StatusOK, status, tc.kind)
		data := contentData(t, body)
		require.Equal(t, tc.kind, data["kind"])
		require.NotNil(t, data["messages"])
		require.NotNil(t, data["meeting_notes"])
		if tc.kind == "messages" {
			assert.Equal(t, empty, data["meeting_notes"])
		} else if tc.kind == "meeting_note" {
			assert.Equal(t, empty, data["messages"])
		} else {
			assertEmptyArrays(t, data, empty, empty)
		}
	}

	deadSession := uuid.New()
	deadRef := "anarlog:" + deadSession.String() + ":" + e.contact.ID.String()
	deadAnarlog := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceAnarlogSessions, &deadRef, when.Add(10*time.Minute), nil)
	seedAPINote(t, e, deadSession, nil, "Dead session note", "dead", "dead")
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	require.NoError(t, e.noteRepo.SoftDeleteMeetingNoteBySessionIDTx(e.ctx, tx, deadSession))
	require.NoError(t, tx.Commit(e.ctx))
	status, body := getInteractionContent(t, e, deadAnarlog.ID.String())
	require.Equal(t, http.StatusOK, status)
	data := contentData(t, body)
	assert.Equal(t, "meeting_note", data["kind"])
	assertEmptyArrays(t, data, empty, empty)
}

// spec: IXN-014.full-thread-window
// spec: IXN-014.window-boundary-closed
// spec: IXN-014.message-order-deterministic
// spec: IXN-014.one-row-per-source-message
func TestInteractionContentAPI_ThreadWindow(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	interaction := seedAPIInteraction(t, e, repository.InteractionSourceGChat, when, nil)
	first := seedAPIComms(t, e, interaction.ID, repository.InteractionSourceGChat, "window-one", "window-first", when)
	second := seedAPIComms(t, e, interaction.ID, repository.InteractionSourceGChat, "window-one", "window-second", when.Add(time.Second))
	equal := seedAPIComms(t, e, interaction.ID, repository.InteractionSourceGChat, "window-one", "window-equal", when.Add(time.Second))
	third := seedAPIComms(t, e, interaction.ID, repository.InteractionSourceGChat, "window-two", "window-third", when.Add(2*time.Second))
	fourth := seedAPIComms(t, e, interaction.ID, repository.InteractionSourceGChat, "window-two", "window-fourth", when.Add(3*time.Second))
	contactRepo := repository.NewContactRepository(e.database.Queries)
	other, err := contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: "Synthetic thread speaker"})
	require.NoError(t, err)
	thread := "window-one"
	otherRow, err := e.database.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "window-other-speaker", ThreadID: &thread, Direction: repository.InteractionDirectionInbound, SentAt: when.Add(time.Second), MatchedContactID: &other.ID})
	require.NoError(t, err)
	// Boundary rows are not linked to the interaction and sit just outside
	// each constituent container's closed window.
	seedAPICommsUnlinked(t, e, repository.InteractionSourceGChat, "window-one", "window-before", when.Add(-time.Second), "before", "peer-before")
	seedAPICommsUnlinked(t, e, repository.InteractionSourceGChat, "window-two", "window-after", when.Add(4*time.Second), "after", "peer-after")
	// A replica of the first row is linked to this interaction under another
	// contact; the expanded interaction's own replica must win deduplication.
	replica, err := e.database.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{Source: repository.InteractionSourceGChat, ExternalID: "window-first", ThreadID: &thread, Direction: repository.InteractionDirectionInbound, SentAt: when, MatchedContactID: &other.ID})
	require.NoError(t, err)

	status, body := getInteractionContent(t, e, interaction.ID.String())
	require.Equal(t, http.StatusOK, status)
	rows := contentData(t, body)["messages"].([]any)
	require.Len(t, rows, 6)
	assert.Equal(t, first.ID.String(), rows[0].(map[string]any)["id"])
	returnedIDs := make(map[string]bool, len(rows))
	for _, raw := range rows {
		row := raw.(map[string]any)
		returnedIDs[row["id"].(string)] = true
		assert.NotEqual(t, "before", row["body"])
		assert.NotEqual(t, "after", row["body"])
	}
	exactVenueKeys := map[string]string{
		first.ID.String():    "gchat:window-one",
		second.ID.String():   "gchat:window-one",
		equal.ID.String():    "gchat:window-one",
		third.ID.String():    "gchat:window-two",
		fourth.ID.String():   "gchat:window-two",
		otherRow.ID.String(): "gchat:window-one",
	}
	for _, raw := range rows {
		row := raw.(map[string]any)
		assert.Equal(t, exactVenueKeys[row["id"].(string)], row["venue_key"])
	}
	assert.False(t, returnedIDs[replica.ID.String()])
	assert.True(t, returnedIDs[second.ID.String()])
	assert.True(t, returnedIDs[equal.ID.String()])
	assert.True(t, returnedIDs[third.ID.String()])
	assert.True(t, returnedIDs[fourth.ID.String()])
	assert.True(t, returnedIDs[otherRow.ID.String()])
	type expectedMessage struct {
		id   uuid.UUID
		when time.Time
	}
	expected := []expectedMessage{{first.ID, when}, {second.ID, when.Add(time.Second)}, {equal.ID, when.Add(time.Second)}, {third.ID, when.Add(2 * time.Second)}, {fourth.ID, when.Add(3 * time.Second)}, {otherRow.ID, when.Add(time.Second)}}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].when.Equal(expected[j].when) {
			return bytes.Compare(expected[i].id[:], expected[j].id[:]) < 0
		}
		return expected[i].when.Before(expected[j].when)
	})
	for i, want := range expected {
		assert.Equal(t, want.id.String(), rows[i].(map[string]any)["id"])
	}

	oneMessageInteraction := seedAPIInteraction(t, e, repository.InteractionSourceGChat, when.Add(10*time.Second), nil)
	oneMessage := seedAPIComms(t, e, oneMessageInteraction.ID, repository.InteractionSourceGChat, "window-one-message", "window-one-message", when.Add(10*time.Second))
	status, body = getInteractionContent(t, e, oneMessageInteraction.ID.String())
	require.Equal(t, http.StatusOK, status)
	oneMessageRows := contentData(t, body)["messages"].([]any)
	require.Len(t, oneMessageRows, 1)
	assert.Equal(t, oneMessage.ID.String(), oneMessageRows[0].(map[string]any)["id"])
	assert.Equal(t, "gchat:window-one-message", oneMessageRows[0].(map[string]any)["venue_key"])
}

// spec: IXN-014.message-field-shape
// spec: IXN-014.sender-fallback-precedence
// spec: IXN-014.nullable-body-empty-string
func TestInteractionContentAPI_MessageShape(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	email := seedAPIInteraction(t, e, repository.InteractionSourceEmail, when, nil)
	outbound := seedAPICommsWith(t, e, email.ID, repository.InteractionSourceEmail, "shape-thread", "shape-outbound", when, nil, nil, nil, repository.InteractionDirectionOutbound, nil)
	status, body := getInteractionContent(t, e, email.ID.String())
	require.Equal(t, http.StatusOK, status)
	row := contentData(t, body)["messages"].([]any)[0].(map[string]any)
	assert.Equal(t, outbound.ID.String(), row["id"])
	assert.Equal(t, "Me", row["sender"])
	assert.Equal(t, true, row["is_outgoing"])
	assert.Equal(t, "", row["body"])
	assert.Equal(t, "email:shape-thread", row["venue_key"])
	assert.Equal(t, outbound.SentAt.Format(time.RFC3339Nano), row["sent_at"])

	telegram := seedAPIInteraction(t, e, repository.InteractionSourceTelegram, when.Add(time.Minute), nil)
	seedAPITelegram(t, e, telegram.ID, 7301, 1, when, "private", "", "tg", false, strptr("tg-user"), nil, nil)
	telegramNamed := seedAPIInteraction(t, e, repository.InteractionSourceTelegram, when.Add(5*time.Minute), nil)
	seedAPITelegram(t, e, telegramNamed.ID, 7302, 2, when, "private", "", "tg named", false, strptr("tg-user-2"), strptr("First"), strptr("Last"))
	whatsappPush := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(2*time.Minute), nil)
	push := seedAPICommsWith(t, e, whatsappPush.ID, repository.InteractionSourceWhatsApp, "push-peer", "push", when, nil, strptr("push body"), nil, repository.InteractionDirectionInbound, []byte(`{"push_name":"Push Name"}`))
	whatsappPeer := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(3*time.Minute), nil)
	peer := seedAPICommsWith(t, e, whatsappPeer.ID, repository.InteractionSourceWhatsApp, "peer-container", "peer", when, nil, strptr("peer body"), strptr("Peer Handle"), repository.InteractionDirectionInbound, nil)
	whatsappUnknown := seedAPIInteraction(t, e, repository.InteractionSourceWhatsApp, when.Add(4*time.Minute), nil)
	unknown := seedAPICommsWith(t, e, whatsappUnknown.ID, repository.InteractionSourceWhatsApp, "unknown-container", "unknown", when, nil, strptr("unknown body"), nil, repository.InteractionDirectionInbound, []byte(`{}`))
	for _, tc := range []struct {
		interaction uuid.UUID
		id, sender  string
	}{
		// A name the SOURCE supplied always wins over the CRM's own: the first
		// three keep their Telegram/WhatsApp identities even though every
		// seeded row carries the same matched contact. Only a sender that
		// resolved to a raw identifier ("Peer Handle") or to no identifier at
		// all ("Unknown") is replaced by the matched contact's name.
		{telegram.ID, "", "tg-user"}, {telegramNamed.ID, "", "First Last"}, {whatsappPush.ID, push.ID.String(), "Push Name"},
		{whatsappPeer.ID, peer.ID.String(), e.contact.FullName}, {whatsappUnknown.ID, unknown.ID.String(), e.contact.FullName},
	} {
		status, body = getInteractionContent(t, e, tc.interaction.String())
		require.Equal(t, http.StatusOK, status)
		messages := contentData(t, body)["messages"].([]any)
		require.Len(t, messages, 1)
		message := messages[0].(map[string]any)
		assert.Equal(t, tc.sender, message["sender"])
		if tc.id != "" {
			assert.Equal(t, tc.id, message["id"])
		}
	}

	// Soft-deleting the contact withdraws the name: the lookup filters
	// deleted_at, so each sender falls back to what it resolved to before,
	// rather than resurrecting a name the rest of the app no longer shows.
	require.NoError(t, repository.NewContactRepository(e.database.Queries).SoftDeleteContact(e.ctx, e.contact.ID))
	for _, tc := range []struct {
		interaction uuid.UUID
		sender      string
	}{
		{whatsappPeer.ID, "Peer Handle"}, {whatsappUnknown.ID, "Unknown"}, {whatsappPush.ID, "Push Name"},
	} {
		status, body = getInteractionContent(t, e, tc.interaction.String())
		require.Equal(t, http.StatusOK, status)
		messages := contentData(t, body)["messages"].([]any)
		require.Len(t, messages, 1)
		assert.Equal(t, tc.sender, messages[0].(map[string]any)["sender"])
	}
}

// spec: IXN-014.note-fields-verbatim
// spec: IXN-014.not-found-404
// spec: IXN-014.soft-deleted-404
func TestInteractionContentAPI_NotesAndNotFound(t *testing.T) {
	e := newInteractionAPITest(t)
	when := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	event := seedAPIEvent(t, e, when, "Notes event", "Notes room", "https://calendar.example.test/notes")
	ref := event.ID.String()
	interaction := seedAPIInteractionWithRef(t, e, uuid.New(), repository.InteractionSourceGCal, &ref, when, nil)
	first := seedAPINote(t, e, uuid.New(), &event.ID, "First title", "First summary", "First memo")
	time.Sleep(2 * time.Millisecond)
	second := seedAPINote(t, e, uuid.New(), &event.ID, "Second title", "Second summary", "Second memo")
	require.NotEqual(t, first.CreatedAt, second.CreatedAt)
	status, body := getInteractionContent(t, e, interaction.ID.String())
	require.Equal(t, http.StatusOK, status)
	notes := contentData(t, body)["meeting_notes"].([]any)
	require.Len(t, notes, 2)
	assert.Equal(t, "First title", notes[0].(map[string]any)["title"])
	assert.Equal(t, "First summary", notes[0].(map[string]any)["summary"])
	assert.Equal(t, "First memo", notes[0].(map[string]any)["memo"])
	assert.Equal(t, "Second title", notes[1].(map[string]any)["title"])
	assert.Equal(t, "Second summary", notes[1].(map[string]any)["summary"])
	assert.Equal(t, "Second memo", notes[1].(map[string]any)["memo"])
	liveSibling := seedAPIInteraction(t, e, repository.InteractionSourceManual, when.Add(time.Minute), nil)
	tombstone := seedAPIInteraction(t, e, repository.InteractionSourceManual, when.Add(2*time.Minute), nil)
	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(e.ctx, tombstone.ID))
	status, _ = getInteractionContent(t, e, uuid.New().String())
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = getInteractionContent(t, e, tombstone.ID.String())
	assert.Equal(t, http.StatusNotFound, status)
	status, body = getInteractionContent(t, e, liveSibling.ID.String())
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "none", contentData(t, body)["kind"])
}
