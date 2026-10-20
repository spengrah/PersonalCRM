//go:build integration_testdb

package judgment_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return envelope.Data
}

// threadList reads one page of the threads list.
func (b *benchEnv) threadList(t *testing.T, query string) (handlers.ThreadListResponse, api.PaginationMeta) {
	t.Helper()
	w := b.get("/api/v1/threads?" + query)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data handlers.ThreadListResponse `json:"data"`
		Meta api.Meta                    `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Meta.Pagination)
	return envelope.Data, *envelope.Meta.Pagination
}

// readThread reads a thread.
func (b *benchEnv) readThread(t *testing.T, thread, query string) handlers.ThreadReadResponse {
	t.Helper()
	return decodeData[handlers.ThreadReadResponse](t, b.get("/api/v1/threads/"+thread+"?"+query))
}

// venueThread creates a venue of the source and kind for the container, with
// the title (none when empty), and returns it as a thread.
func (e *env) venueThread(t *testing.T, source, kind, container, title string) thread {
	t.Helper()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	id, err := e.venues.ResolveVenueForInteraction(e.ctx, tx, source, kind, container, title)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return thread{id: id, container: container, source: source}
}

// say stores one message on the thread as its source stores it, from the
// contact when fromUser is false, else the user's own, and returns the
// message's key. A meeting thread stores a meeting note instead.
func (e *env) say(t *testing.T, th thread, contact uuid.UUID, fromUser bool, at time.Time) string {
	t.Helper()
	switch th.source {
	case repository.InteractionSourceTelegram:
		chat, err := strconv.ParseInt(th.container, 10, 64)
		require.NoError(t, err)
		id := int32(rand.Int31n(1 << 30)) //nolint:gosec // a test message ID
		text, name := "synthetic message body", "Synthetic"
		_, err = repository.NewTelegramMessageRepository(e.database.Queries).UpsertMessage(e.ctx, repository.UpsertTelegramMessageParams{
			TelegramMessageID: id, TelegramChatID: chat, ChatType: "private", MessageText: &text, MessageType: "text",
			SentAt: at, IsOutgoing: fromUser, PeerFirstName: &name,
		})
		require.NoError(t, err)
		return fmt.Sprintf("telegram:%d:%d", chat, id)
	case repository.InteractionSourceMessages:
		guid, text, handle := uuid.NewString(), "synthetic message body", "peer-"+contact.String()
		_, err := repository.NewMessagesMessageRepository(e.database.Queries).UpsertMessage(e.ctx, repository.UpsertMessagesMessageParams{
			Guid: guid, ChatGuid: th.container, PeerHandle: handle, Text: &text, MessageType: "text", SentAt: at,
			IsOutgoing: fromUser, MatchedContactID: &contact,
		})
		require.NoError(t, err)
		return "messages:" + guid
	case repository.InteractionSourceAnarlogSessions:
		session, err := uuid.Parse(th.container)
		require.NoError(t, err)
		tx, err := e.database.Pool.Begin(e.ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(e.ctx) }()
		summary := "synthetic meeting summary"
		_, err = repository.NewMeetingNoteRepository(e.database.Queries).InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{
			AnarlogSessionID: session, Summary: &summary, LinkageState: repository.LinkageStateOrphanNeedsReview, MeetingAt: at,
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit(e.ctx))
		return "anarlog_sessions:" + session.String()
	default:
		params := messageParams(th, contact, fromUser, at)
		params.Source = th.source
		_, err := e.comms.UpsertMessage(e.ctx, params)
		require.NoError(t, err)
		return params.Source + ":" + params.ExternalID
	}
}

// joined records the contact as a participant of the thread.
func (e *env) joined(t *testing.T, th thread, contact uuid.UUID, at time.Time) {
	t.Helper()
	e.record(t, th, contact, repository.InteractionDirectionInbound, at)
}

// sayCopy stores the contact's copy of the message the external ID names, as a
// group chat stores a message once per member, and returns the message's key.
func (e *env) sayCopy(t *testing.T, th thread, externalID string, contact uuid.UUID, fromUser bool, at time.Time) string {
	t.Helper()
	params := messageParams(th, contact, fromUser, at)
	params.Source, params.ExternalID = th.source, externalID
	_, err := e.comms.UpsertMessage(e.ctx, params)
	require.NoError(t, err)
	return params.Source + ":" + externalID
}

// contactName is the name the CRM holds for the contact.
func (e *env) contactName(t *testing.T, contact uuid.UUID) string {
	t.Helper()
	names, err := e.people.ListContactNamesByIDs(e.ctx, []uuid.UUID{contact})
	require.NoError(t, err)
	return names[contact]
}

// runOf returns a run the judgment appears in.
func (b *benchEnv) runOf(t *testing.T, judgmentID string) judgment.RunID {
	t.Helper()
	runs, err := b.store.ListRuns(b.ctx)
	require.NoError(t, err)
	for _, run := range runs {
		views, err := b.store.ListJudgments(b.ctx, judgment.JudgmentQuery{Run: &run.ID})
		require.NoError(t, err)
		for _, v := range views {
			if v.ID.String() == judgmentID {
				return run.ID
			}
		}
	}
	require.Failf(t, "no run", "no run holds judgment %s", judgmentID)
	return judgment.RunID{}
}

// confirmNones posts Confirm Nones for the proposal's night, in a run that
// holds it, and returns the answer.
func (b *benchEnv) confirmNones(t *testing.T, p handlers.BenchProposalResponse) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(t, "/api/v1/judgments/"+p.JudgmentID+"/confirm-nones?run_id="+b.runOf(t, p.JudgmentID).String(), nil)
}

// closeThread posts the close of the thread in the run and returns the answer.
func (b *benchEnv) closeThread(t *testing.T, th thread, run judgment.RunID) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(t, "/api/v1/threads/"+th.id.String()+"/close?run_id="+run.String(), nil)
}

// compared returns the i-th compared method: a method the registry lists, which
// has display thresholds and a clearing floor.
func (b *benchEnv) compared(i int) judgment.MethodRef {
	return judgment.MethodRef{Key: b.methods.Keys()[i], Version: "v1", WordingVersion: "w1"}
}

// threshold is the display threshold of the method's flags of the kind.
func (b *benchEnv) threshold(t *testing.T, m judgment.MethodRef, kind judgment.FlagKind) float64 {
	t.Helper()
	v, err := b.store.DisplayThreshold(b.ctx, m.Key, kind)
	require.NoError(t, err)
	return v
}

// startRun starts a run of the method.
func (b *benchEnv) startRun(t *testing.T, kind judgment.RunKind, m judgment.MethodRef) judgment.RunID {
	t.Helper()
	run, err := b.store.StartRun(b.ctx, judgment.RunSpec{Kind: kind, Method: m})
	require.NoError(t, err)
	return run
}

// talk stores n messages on the thread, one a day from day 1, from the
// contacts in turn, and returns their keys in thread order.
func (b *benchEnv) talk(t *testing.T, th thread, n int, contacts ...uuid.UUID) []string {
	t.Helper()
	keys := make([]string, n)
	for i := range keys {
		c := contacts[i%len(contacts)]
		keys[i] = b.say(t, th, c, false, b.day(i+1))
		b.joined(t, th, c, b.day(i+1))
	}
	return keys
}

// judged records, in the run, a judgment of the thread at the cut-off that read
// a window starting at windowStart and answered the new messages' flags, and
// returns its ID. The flags are given as keyed probabilities: for each new
// message, the commitment, big moment and claim probabilities.
func (b *benchEnv) judged(t *testing.T, run judgment.RunID, m judgment.MethodRef, th thread, cutOff time.Time, windowStart string, newKeys []string, probabilities map[string][3]float64) uuid.UUID {
	t.Helper()
	j := windowed(m, th, cutOff, "h-"+cutOff.UTC().Format(time.RFC3339Nano)+"-"+windowStart, windowStart, newKeys, judgment.Outcomes{})
	for _, key := range newKeys {
		p := probabilities[key]
		j.Flags = append(j.Flags, flagsOf(key, p[0], p[1], p[2])...)
	}
	_, err := b.store.RecordJudgment(b.ctx, run, j)
	require.NoError(t, err)
	return b.viewOf(t, th, m, cutOff).ID
}

// review reads the thread's review state in the run.
func (b *benchEnv) reviewOf(t *testing.T, th thread, run judgment.RunID) handlers.ThreadReviewResponse {
	t.Helper()
	read := b.readThread(t, th.id.String(), "run_id="+run.String())
	require.NotNil(t, read.Review)
	return *read.Review
}

// chipOf returns the chip of the kind on the message in the night, nil when it
// shows none.
func chipOf(night handlers.NightResponse, key string, kind judgment.FlagKind) *handlers.FlagChipResponse {
	for _, m := range night.Messages {
		if m.MessageKey != key {
			continue
		}
		for i, c := range m.Chips {
			if c.Kind == string(kind) {
				return &m.Chips[i]
			}
		}
	}
	return nil
}

// nightMessage returns the message in the night.
func nightMessage(t *testing.T, night handlers.NightResponse, key string) handlers.NightMessageResponse {
	t.Helper()
	for _, m := range night.Messages {
		if m.MessageKey == key {
			return m
		}
	}
	require.Failf(t, "no message", "night holds no %s", key)
	return handlers.NightMessageResponse{}
}

func nightKeys(night handlers.NightResponse) []string {
	keys := make([]string, len(night.Messages))
	for i, m := range night.Messages {
		keys[i] = m.MessageKey
	}
	return keys
}
