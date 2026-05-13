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

// stubMessagesEngine records every AggregateForContact call.
type stubMessagesEngine struct {
	calls   []stubMsgEngCall
	failErr error
}

type stubMsgEngCall struct {
	contactID uuid.UUID
	chatID    string
}

func (s *stubMessagesEngine) AggregateForContact(_ context.Context, contactID uuid.UUID, chatID string) error {
	s.calls = append(s.calls, stubMsgEngCall{contactID, chatID})
	return s.failErr
}

// stubMsgInserter records every Insert call.
type stubMsgInserter struct {
	calls []consumerjobs.MessagingAggregateForContactArgs
	err   error
}

func (s *stubMsgInserter) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if a, ok := args.(consumerjobs.MessagingAggregateForContactArgs); ok {
		s.calls = append(s.calls, a)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &rivertype.JobInsertResult{}, nil
}

func buildMessageEnvelope(t *testing.T, kind events.Kind, peerRef string) *events.Envelope {
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
		Source:  "messages",
		Kind:    kind,
		Payload: payloadBytes,
	}
}

// TestReenqueuer_EnqueuesFreshJobAndCallsEngine asserts the happy path:
// both R10B (fresh River job) and the chat-aware sync pass fire on
// a successful Reenqueue call.
func TestReenqueuer_EnqueuesFreshJobAndCallsEngine(t *testing.T) {
	eng := &stubMessagesEngine{}
	ins := &stubMsgInserter{}
	r := NewMessagesAggregatorReenqueuer(eng, ins, "messages")
	contactID := uuid.New()

	env := buildMessageEnvelope(t, events.KindMessageReceived, "messages:chat-A")
	err := r.Reenqueue(context.Background(), env, contactID)
	require.NoError(t, err)
	require.Len(t, ins.calls, 1)
	require.Equal(t, contactID, ins.calls[0].ContactID)
	require.Equal(t, "messages", ins.calls[0].Source)
	require.Len(t, eng.calls, 1)
	require.Equal(t, contactID, eng.calls[0].contactID)
	require.Equal(t, "chat-A", eng.calls[0].chatID)
}

// TestReenqueuer_OutboundKindAlsoParses confirms the parser handles
// KindMessageSent.
func TestReenqueuer_OutboundKindAlsoParses(t *testing.T) {
	eng := &stubMessagesEngine{}
	r := NewMessagesAggregatorReenqueuer(eng, nil, "messages")
	contactID := uuid.New()

	env := buildMessageEnvelope(t, events.KindMessageSent, "messages:chat-B")
	err := r.Reenqueue(context.Background(), env, contactID)
	require.NoError(t, err)
	require.Len(t, eng.calls, 1)
	require.Equal(t, "chat-B", eng.calls[0].chatID)
}

// TestReenqueuer_MalformedPeerRef_LogsAndReturnsNil verifies the
// reenqueuer treats an unparseable PeerRef as a non-fatal warning.
// The recorder has already committed; rolling back is not an option.
func TestReenqueuer_MalformedPeerRef_LogsAndReturnsNil(t *testing.T) {
	eng := &stubMessagesEngine{}
	r := NewMessagesAggregatorReenqueuer(eng, nil, "messages")

	env := buildMessageEnvelope(t, events.KindMessageReceived, "tg:1234567")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls, "engine MUST NOT be invoked when PeerRef is unparseable")
}

// TestReenqueuer_EmptyChatID_LogsAndReturnsNil verifies the
// reenqueuer treats an empty chat scope as a non-fatal warning.
func TestReenqueuer_EmptyChatID_LogsAndReturnsNil(t *testing.T) {
	eng := &stubMessagesEngine{}
	r := NewMessagesAggregatorReenqueuer(eng, nil, "messages")

	env := buildMessageEnvelope(t, events.KindMessageReceived, "messages:")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls, "engine MUST NOT be invoked when chat ID is empty")
}

// TestReenqueuer_NilEngine_NoOpsButStillEnqueues confirms that a nil
// engine (test mode) doesn't crash the reenqueuer; the R10B fresh-
// enqueue path still fires.
func TestReenqueuer_NilEngine_NoOpsButStillEnqueues(t *testing.T) {
	ins := &stubMsgInserter{}
	r := NewMessagesAggregatorReenqueuer(nil, ins, "messages")
	env := buildMessageEnvelope(t, events.KindMessageReceived, "messages:chat-X")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Len(t, ins.calls, 1, "fresh-enqueue path must fire even when engine is nil")
}

// TestReenqueuer_InserterError_LogsAndContinues verifies an enqueue
// failure does NOT prevent the chat-aware sync pass — both paths are
// best-effort independent.
func TestReenqueuer_InserterError_LogsAndContinues(t *testing.T) {
	eng := &stubMessagesEngine{}
	ins := &stubMsgInserter{err: errors.New("river down")}
	r := NewMessagesAggregatorReenqueuer(eng, ins, "messages")

	env := buildMessageEnvelope(t, events.KindMessageReceived, "messages:chat-Z")
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err) // best-effort path; logger.Warn only
	require.Len(t, eng.calls, 1, "engine pass must still fire after enqueue failure")
}

// TestReenqueuer_NonMessageKind_ReturnsNil confirms the parser falls
// through cleanly on unexpected kinds (defensive — the registry should
// not dispatch non-message envelopes here, but the parser handles them).
func TestReenqueuer_NonMessageKind_ReturnsNil(t *testing.T) {
	eng := &stubMessagesEngine{}
	r := NewMessagesAggregatorReenqueuer(eng, nil, "messages")

	env := &events.Envelope{Source: "messages", Kind: events.KindInteractionRecorded}
	err := r.Reenqueue(context.Background(), env, uuid.New())
	require.NoError(t, err)
	require.Empty(t, eng.calls)
}
