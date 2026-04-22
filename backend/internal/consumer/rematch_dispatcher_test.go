package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubRunner records Run invocations and returns a configurable error.
type stubRunner struct {
	mu    sync.Mutex
	calls []stubRunCall
	err   error
}

type stubRunCall struct {
	jobID     uuid.UUID
	contactID uuid.UUID
	methods   []service.Method
}

func (s *stubRunner) Run(_ context.Context, jobID, contactID uuid.UUID, methods []service.Method) error {
	s.mu.Lock()
	s.calls = append(s.calls, stubRunCall{jobID: jobID, contactID: contactID, methods: append([]service.Method(nil), methods...)})
	s.mu.Unlock()
	return s.err
}

func (s *stubRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// validEnvelope builds a routable contact_methods.added envelope for tests.
func validEnvelope(t *testing.T, contactID, jobID uuid.UUID, methods []events.ContactMethodRef) *events.Envelope {
	t.Helper()
	payload, err := events.Marshal(events.KindContactMethodsAdded, events.ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    contactID,
		Methods:      methods,
		RematchJobID: jobID,
	})
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     "manual",
		SourceID:   jobID.String(),
		Kind:       events.KindContactMethodsAdded,
		Payload:    payload,
		ObservedAt: time.Now(),
	}
}

func TestRematchDispatcher_HandleEvent_DispatchesToRunner(t *testing.T) {
	runner := &stubRunner{}
	d := NewRematchDispatcher(runner)

	contactID := uuid.New()
	jobID := uuid.New()
	methods := []events.ContactMethodRef{
		{Type: "email", Value: "a@b.c"},
		{Type: "phone", Value: "+15551212"},
	}
	env := validEnvelope(t, contactID, jobID, methods)

	require.NoError(t, d.HandleEvent(context.Background(), env))
	require.Equal(t, 1, runner.callCount())
	call := runner.calls[0]
	require.Equal(t, jobID, call.jobID)
	require.Equal(t, contactID, call.contactID)
	require.Len(t, call.methods, 2)
	require.Equal(t, service.Method{Type: "email", Value: "a@b.c"}, call.methods[0])
	require.Equal(t, service.Method{Type: "phone", Value: "+15551212"}, call.methods[1])
}

func TestRematchDispatcher_HandleEvent_NilEnvelope_Errors(t *testing.T) {
	d := NewRematchDispatcher(&stubRunner{})
	err := d.HandleEvent(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

func TestRematchDispatcher_HandleEvent_WrongKind_Errors(t *testing.T) {
	d := NewRematchDispatcher(&stubRunner{})
	env := &events.Envelope{
		ID:   uuid.New(),
		Kind: events.KindInteractionRecorded,
	}
	err := d.HandleEvent(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected kind")
}

func TestRematchDispatcher_HandleEvent_MalformedPayload_Errors(t *testing.T) {
	d := NewRematchDispatcher(&stubRunner{})
	env := &events.Envelope{
		ID:      uuid.New(),
		Kind:    events.KindContactMethodsAdded,
		Payload: json.RawMessage(`{not json`),
	}
	err := d.HandleEvent(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal contact_methods.added")
}

func TestRematchDispatcher_HandleEvent_EmptyRematchJobID_Errors(t *testing.T) {
	d := NewRematchDispatcher(&stubRunner{})
	env := validEnvelope(t, uuid.New(), uuid.Nil, []events.ContactMethodRef{{Type: "email", Value: "x"}})
	err := d.HandleEvent(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty rematch_job_id")
}

func TestRematchDispatcher_HandleEvent_EmptyContactID_Errors(t *testing.T) {
	d := NewRematchDispatcher(&stubRunner{})
	env := validEnvelope(t, uuid.Nil, uuid.New(), []events.ContactMethodRef{{Type: "email", Value: "x"}})
	err := d.HandleEvent(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty contact_id")
}

func TestRematchDispatcher_HandleEvent_EmptyMethods_NoOp(t *testing.T) {
	runner := &stubRunner{}
	d := NewRematchDispatcher(runner)
	env := validEnvelope(t, uuid.New(), uuid.New(), nil)
	require.NoError(t, d.HandleEvent(context.Background(), env))
	require.Zero(t, runner.callCount(), "empty methods must not invoke runner")
}

func TestRematchDispatcher_HandleEvent_RunnerError_Propagates(t *testing.T) {
	sentinel := errors.New("handler exploded")
	runner := &stubRunner{err: sentinel}
	d := NewRematchDispatcher(runner)

	env := validEnvelope(t, uuid.New(), uuid.New(), []events.ContactMethodRef{{Type: "email", Value: "a"}})
	err := d.HandleEvent(context.Background(), env)
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel, "dispatcher must surface runner error so river retries")
	require.Contains(t, err.Error(), "rematch run")
}
