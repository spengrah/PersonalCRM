package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReenqueuer records every Reenqueue invocation so tests can assert
// dispatch source + contactID.
type fakeReenqueuer struct {
	mu    sync.Mutex
	calls []reenqueueCall
	err   error
}

type reenqueueCall struct {
	source    string
	contactID uuid.UUID
}

func (f *fakeReenqueuer) Reenqueue(_ context.Context, env *events.Envelope, contactID uuid.UUID) error {
	f.mu.Lock()
	f.calls = append(f.calls, reenqueueCall{source: env.Source, contactID: contactID})
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	return nil
}

// TestAggregatorReenqueuerRegistry_DispatchesBySource locks the
// source-dispatch invariant: the registry routes Reenqueue calls to the
// entry keyed by env.Source.
func TestAggregatorReenqueuerRegistry_DispatchesBySource(t *testing.T) {
	tg := &fakeReenqueuer{}
	msgs := &fakeReenqueuer{}
	reg := NewAggregatorReenqueuerRegistry(map[string]AggregatorReenqueuer{
		"telegram": tg,
		"messages": msgs,
	})

	contactA := uuid.New()
	contactB := uuid.New()
	require.NoError(t, reg.Reenqueue(context.Background(), &events.Envelope{Source: "telegram"}, contactA))
	require.NoError(t, reg.Reenqueue(context.Background(), &events.Envelope{Source: "messages"}, contactB))

	require.Len(t, tg.calls, 1)
	assert.Equal(t, "telegram", tg.calls[0].source)
	assert.Equal(t, contactA, tg.calls[0].contactID)
	require.Len(t, msgs.calls, 1)
	assert.Equal(t, "messages", msgs.calls[0].source)
	assert.Equal(t, contactB, msgs.calls[0].contactID)
}

// TestAggregatorReenqueuerRegistry_UnknownSourceIsLoggedNoop confirms
// the registry returns nil for sources without an entry (so the
// consumer's post-commit reenqueue path does not roll back the
// already-committed interaction on a missing entry).
func TestAggregatorReenqueuerRegistry_UnknownSourceIsLoggedNoop(t *testing.T) {
	reg := NewAggregatorReenqueuerRegistry(map[string]AggregatorReenqueuer{})
	err := reg.Reenqueue(context.Background(), &events.Envelope{Source: "telegram"}, uuid.New())
	assert.NoError(t, err, "unknown source must NOT error")
}

// TestNoopAggregatorReenqueuer_AlwaysReturnsNil locks the messages-source
// PR3 contract: the no-op entry tolerates every input and never errors.
func TestNoopAggregatorReenqueuer_AlwaysReturnsNil(t *testing.T) {
	r := NoopAggregatorReenqueuer{}
	assert.NoError(t, r.Reenqueue(context.Background(), &events.Envelope{}, uuid.New()))
}

// fakeTelegramEngine captures invocations of the chat-aware vs batch
// aggregator paths so tests can assert which one the reenqueuer chose.
type fakeTelegramEngine struct {
	mu                sync.Mutex
	aggregateForCalls []aggregateForCall
	batchCalls        []uuid.UUID
}

type aggregateForCall struct {
	contactID uuid.UUID
	chatID    int64
}

func (f *fakeTelegramEngine) AggregateForContact(_ context.Context, contactID uuid.UUID, chatID int64) error {
	f.mu.Lock()
	f.aggregateForCalls = append(f.aggregateForCalls, aggregateForCall{contactID, chatID})
	f.mu.Unlock()
	return nil
}

func (f *fakeTelegramEngine) AggregateForContactBatch(_ context.Context, contactID uuid.UUID) error {
	f.mu.Lock()
	f.batchCalls = append(f.batchCalls, contactID)
	f.mu.Unlock()
	return nil
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// TestTelegramAggregatorReenqueuer_ParsesChatIDFromPeerRef proves the
// chat-aware path: a message.received envelope with PeerRef="tg:12345"
// triggers AggregateForContact(contactID, 12345). This preserves
// extend/bridge/coalescing semantics for rows arriving in the
// Stage 2 → Stage 3 window.
func TestTelegramAggregatorReenqueuer_ParsesChatIDFromPeerRef(t *testing.T) {
	eng := &fakeTelegramEngine{}
	r := NewTelegramAggregatorReenqueuer(eng)
	contactID := uuid.New()

	env := &events.Envelope{
		Source: "telegram",
		Kind:   events.KindMessageReceived,
		Payload: mustMarshal(t, events.MessageReceivedPayload{
			Version:           1,
			PeerRef:           "tg:12345",
			MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			ExternalMessageID: "tg:12345:50001",
		}),
	}
	require.NoError(t, r.Reenqueue(context.Background(), env, contactID))

	require.Len(t, eng.aggregateForCalls, 1, "chat-aware path called")
	assert.Equal(t, contactID, eng.aggregateForCalls[0].contactID)
	assert.Equal(t, int64(12345), eng.aggregateForCalls[0].chatID)
	assert.Empty(t, eng.batchCalls, "batch path NOT called")
}

// TestTelegramAggregatorReenqueuer_MessageSentAlsoParses confirms the
// chat-aware path fires for KindMessageSent envelopes too.
func TestTelegramAggregatorReenqueuer_MessageSentAlsoParses(t *testing.T) {
	eng := &fakeTelegramEngine{}
	r := NewTelegramAggregatorReenqueuer(eng)
	contactID := uuid.New()

	env := &events.Envelope{
		Source: "telegram",
		Kind:   events.KindMessageSent,
		Payload: mustMarshal(t, events.MessageSentPayload{
			Version:           1,
			PeerRef:           "tg:99999",
			MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			ExternalMessageID: "tg:99999:1",
		}),
	}
	require.NoError(t, r.Reenqueue(context.Background(), env, contactID))

	require.Len(t, eng.aggregateForCalls, 1)
	assert.Equal(t, int64(99999), eng.aggregateForCalls[0].chatID)
}

// TestTelegramAggregatorReenqueuer_NonParseablePeerRefFallsBackToBatch
// covers the fallback path: a sentinel envelope (e.g., synthesized by a
// future sweeper) without a parseable PeerRef triggers
// AggregateForContactBatch instead. This is the ONE place batch is
// acceptable: no specific stranded session to extend, just a generic
// catch-up.
func TestTelegramAggregatorReenqueuer_NonParseablePeerRefFallsBackToBatch(t *testing.T) {
	eng := &fakeTelegramEngine{}
	r := NewTelegramAggregatorReenqueuer(eng)
	contactID := uuid.New()

	// Payload with non-"tg:<int>" PeerRef.
	env := &events.Envelope{
		Source: "telegram",
		Kind:   events.KindMessageReceived,
		Payload: mustMarshal(t, events.MessageReceivedPayload{
			Version:           1,
			PeerRef:           "sentinel:weird-ref",
			MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			ExternalMessageID: "x",
		}),
	}
	require.NoError(t, r.Reenqueue(context.Background(), env, contactID))

	assert.Empty(t, eng.aggregateForCalls, "non-parseable PeerRef falls back to batch")
	require.Len(t, eng.batchCalls, 1)
	assert.Equal(t, contactID, eng.batchCalls[0])
}

// TestTelegramAggregatorReenqueuer_UnsupportedKindFallsBackToBatch
// confirms the fallback fires when the envelope kind is not one of
// the two message kinds (defensive — parser returns false for unknown
// kinds).
func TestTelegramAggregatorReenqueuer_UnsupportedKindFallsBackToBatch(t *testing.T) {
	eng := &fakeTelegramEngine{}
	r := NewTelegramAggregatorReenqueuer(eng)
	contactID := uuid.New()

	env := &events.Envelope{
		Source:  "telegram",
		Kind:    events.KindCalendarAttended, // not a message kind
		Payload: mustMarshal(t, map[string]any{}),
	}
	require.NoError(t, r.Reenqueue(context.Background(), env, contactID))

	assert.Empty(t, eng.aggregateForCalls)
	require.Len(t, eng.batchCalls, 1)
}

// TestAggregatorReenqueuerRegistry_PropagatesError confirms an entry's
// error bubbles up so the consumer worker can log it. (The worker
// catches it and logs a warning — the interaction is already
// committed, so the error does NOT roll back work.)
func TestAggregatorReenqueuerRegistry_PropagatesError(t *testing.T) {
	tg := &fakeReenqueuer{err: errors.New("simulated reenqueue failure")}
	reg := NewAggregatorReenqueuerRegistry(map[string]AggregatorReenqueuer{"telegram": tg})

	err := reg.Reenqueue(context.Background(), &events.Envelope{Source: "telegram"}, uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated reenqueue failure")
}
