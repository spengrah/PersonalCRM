package consumer

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// stubCommsEngine records every AggregateForContact call.
type stubCommsEngine struct {
	calls   []stubCommsEngCall
	failErr error
}

type stubCommsEngCall struct {
	contactID uuid.UUID
	chatID    string
}

func (s *stubCommsEngine) AggregateForContact(_ context.Context, contactID uuid.UUID, chatID string) error {
	s.calls = append(s.calls, stubCommsEngCall{contactID, chatID})
	return s.failErr
}

// stubCommsInserter records every Insert call.
type stubCommsInserter struct {
	calls []consumerjobs.MessagingAggregateForContactArgs
	err   error
}

func (s *stubCommsInserter) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if a, ok := args.(consumerjobs.MessagingAggregateForContactArgs); ok {
		s.calls = append(s.calls, a)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &rivertype.JobInsertResult{}, nil
}

func buildGChatEnvelope(t *testing.T, kind events.Kind, peerRef string) *events.Envelope {
	t.Helper()
	contactID := uuid.New()
	var payloadBytes []byte
	var err error
	if kind == events.KindMessageReceived {
		payloadBytes, err = events.Marshal(kind, events.MessageReceivedPayload{
			Version:   1,
			ContactID: &contactID,
			PeerRef:   peerRef,
		})
	} else {
		payloadBytes, err = events.Marshal(kind, events.MessageSentPayload{
			Version:   1,
			ContactID: &contactID,
			PeerRef:   peerRef,
		})
	}
	require.NoError(t, err)
	return &events.Envelope{
		Source:  "gchat",
		Kind:    kind,
		Payload: payloadBytes,
	}
}

// TestCommsReenqueuer_HappyPath asserts both the fresh-River-job enqueue and
// the chat-aware sync pass fire. Critically, the gchat space resource name
// retains its `/` — the prefix strip is "gchat:", not a path split.
func TestCommsReenqueuer_HappyPath(t *testing.T) {
	eng := &stubCommsEngine{}
	ins := &stubCommsInserter{}
	r := NewCommsAggregatorReenqueuer(eng, ins, "gchat")
	contactID := uuid.New()

	env := buildGChatEnvelope(t, events.KindMessageReceived, "gchat:spaces/AAA")
	err := r.Reenqueue(context.Background(), env, contactID)
	require.NoError(t, err)
	require.Len(t, ins.calls, 1)
	require.Equal(t, contactID, ins.calls[0].ContactID)
	require.Equal(t, "gchat", ins.calls[0].Source)
	require.Len(t, eng.calls, 1)
	require.Equal(t, contactID, eng.calls[0].contactID)
	require.Equal(t, "spaces/AAA", eng.calls[0].chatID, "chatID must retain the '/' from the space resource name")
}

// TestCommsReenqueuer_OutboundKindAlsoParses confirms KindMessageSent parses.
func TestCommsReenqueuer_OutboundKindAlsoParses(t *testing.T) {
	eng := &stubCommsEngine{}
	r := NewCommsAggregatorReenqueuer(eng, nil, "gchat")
	contactID := uuid.New()

	env := buildGChatEnvelope(t, events.KindMessageSent, "gchat:spaces/BBB")
	err := r.Reenqueue(context.Background(), env, contactID)
	require.NoError(t, err)
	require.Len(t, eng.calls, 1)
	require.Equal(t, "spaces/BBB", eng.calls[0].chatID)
}

// TestCommsReenqueuer_WrongPrefix_LogsAndReturnsNil verifies a PeerRef with a
// different source prefix is treated as unparseable (the engine is not called).
func TestCommsReenqueuer_WrongPrefix_LogsAndReturnsNil(t *testing.T) {
	eng := &stubCommsEngine{}
	r := NewCommsAggregatorReenqueuer(eng, nil, "gchat")

	env := buildGChatEnvelope(t, events.KindMessageReceived, "messages:chat-A")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls, "engine MUST NOT be invoked when the prefix does not match the source")
}

// TestCommsReenqueuer_EmptyChatID_LogsAndReturnsNil verifies an empty chat
// scope (prefix present, body empty) is non-fatal.
func TestCommsReenqueuer_EmptyChatID_LogsAndReturnsNil(t *testing.T) {
	eng := &stubCommsEngine{}
	r := NewCommsAggregatorReenqueuer(eng, nil, "gchat")

	env := buildGChatEnvelope(t, events.KindMessageReceived, "gchat:")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls, "engine MUST NOT be invoked when chat ID is empty")
}

// TestCommsReenqueuer_NilEngine_NoOpsButStillEnqueues confirms a nil engine
// doesn't crash; the fresh-enqueue path still fires.
func TestCommsReenqueuer_NilEngine_NoOpsButStillEnqueues(t *testing.T) {
	ins := &stubCommsInserter{}
	r := NewCommsAggregatorReenqueuer(nil, ins, "gchat")
	env := buildGChatEnvelope(t, events.KindMessageReceived, "gchat:spaces/CCC")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Len(t, ins.calls, 1, "fresh-enqueue path must fire even when engine is nil")
}

// TestCommsReenqueuer_InserterError_LogsAndContinues verifies an enqueue
// failure does NOT prevent the chat-aware sync pass.
func TestCommsReenqueuer_InserterError_LogsAndContinues(t *testing.T) {
	eng := &stubCommsEngine{}
	ins := &stubCommsInserter{err: errors.New("river down")}
	r := NewCommsAggregatorReenqueuer(eng, ins, "gchat")

	env := buildGChatEnvelope(t, events.KindMessageReceived, "gchat:spaces/DDD")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Len(t, eng.calls, 1, "engine pass must still fire after enqueue failure")
}

// TestCommsReenqueuer_NonMessageKind_ReturnsNil confirms the parser falls
// through cleanly on unexpected kinds (sweeper sentinels carry no PeerRef).
func TestCommsReenqueuer_NonMessageKind_ReturnsNil(t *testing.T) {
	eng := &stubCommsEngine{}
	r := NewCommsAggregatorReenqueuer(eng, nil, "gchat")

	env := &events.Envelope{Source: "gchat", Kind: events.KindInteractionRecorded}
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls)
}
