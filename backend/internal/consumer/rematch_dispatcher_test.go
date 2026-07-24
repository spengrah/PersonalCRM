package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// envelopeBus is an eventBusTx stub whose GetEvent returns a preconfigured
// envelope, used to drive RematchDispatcherWorker.Work in unit tests.
type envelopeBus struct {
	env *events.Envelope
	err error
}

func (b *envelopeBus) PublishTx(context.Context, pgx.Tx, *events.Envelope) error { return nil }

func (b *envelopeBus) GetEvent(context.Context, uuid.UUID) (*events.Envelope, error) {
	return b.env, b.err
}

func newWorkerForRunner(t *testing.T, runnerErr error) (*RematchDispatcherWorker, *river.Job[consumerjobs.RematchDispatcherJobArgs]) {
	t.Helper()
	env := validEnvelope(t, uuid.New(), uuid.New(), []events.ContactMethodRef{{Type: "email", Value: "a@b.c"}})
	worker := NewRematchDispatcherWorker(
		&envelopeBus{env: env},
		nil, // pool is unused by Work
		NewRematchDispatcher(&stubRunner{err: runnerErr}),
	)
	job := &river.Job[consumerjobs.RematchDispatcherJobArgs]{
		Args: consumerjobs.RematchDispatcherJobArgs{EventID: env.ID},
	}
	return worker, job
}

// TestRematchDispatcherWorker_Work_BudgetExhausted_Snoozes proves that when the
// rematch runner reports budget exhaustion (the continue-later sentinel), Work
// reschedules the job via JobSnooze instead of returning a terminal error that
// would burn the job's MaxAttempts and discard the backfill. The sentinel is
// wrapped exactly as production wraps it — per-account context (%w) inside the
// errors.Join that gchatRematchBase.rematch builds — so this also proves the
// sentinel survives the full wrapping (Join + %w) back up to Work.
func TestRematchDispatcherWorker_Work_BudgetExhausted_Snoozes(t *testing.T) {
	runnerErr := errors.Join(fmt.Errorf("account acct-1: %w", google.ErrRematchBudgetExhausted))
	worker, job := newWorkerForRunner(t, runnerErr)

	err := worker.Work(context.Background(), job)

	var snooze *river.JobSnoozeError
	require.ErrorAs(t, err, &snooze, "budget exhaustion must reschedule via JobSnooze, not discard")
	require.Positive(t, snooze.Duration, "snooze must use a non-zero backoff")
	require.NotErrorIs(t, err, google.ErrRematchBudgetExhausted,
		"the returned reschedule must not carry the sentinel as a terminal error")
}

// TestRematchDispatcherWorker_Work_GenuineError_Propagates proves a non-budget
// failure still propagates as a terminal error so river retries and eventually
// discards it per MaxAttempts — budget-exhaustion handling must not swallow
// real failures. The error is wrapped as production wraps it (per-account %w
// inside errors.Join).
func TestRematchDispatcherWorker_Work_GenuineError_Propagates(t *testing.T) {
	sentinel := errors.New("gchat api exploded")
	runnerErr := errors.Join(fmt.Errorf("account acct-1: %w", sentinel))
	worker, job := newWorkerForRunner(t, runnerErr)

	err := worker.Work(context.Background(), job)

	require.ErrorIs(t, err, sentinel, "genuine errors must propagate so river retries/discards normally")
	var snooze *river.JobSnoozeError
	require.NotErrorAs(t, err, &snooze, "genuine errors must not be rescheduled as a snooze")
}

// TestRematchDispatcherWorker_Work_MixedError_Snoozes locks the documented
// self-resolving tradeoff for the multi-account fan-out: when one account
// budget-exhausts and another returns a genuine error, rematch returns an
// errors.Join of both, and Work snoozes (budget-exhaustion is present). Once the
// budget stops exhausting on a later run, only the genuine error remains and it
// discards normally. Guarding this here means a refactor that reordered the
// checks (e.g. inspecting for a genuine error first) fails loudly instead of
// silently discarding backfills mid-budget.
func TestRematchDispatcherWorker_Work_MixedError_Snoozes(t *testing.T) {
	runnerErr := errors.Join(
		fmt.Errorf("account acct-1: %w", google.ErrRematchBudgetExhausted),
		fmt.Errorf("account acct-2: %w", errors.New("oauth token revoked")),
	)
	worker, job := newWorkerForRunner(t, runnerErr)

	err := worker.Work(context.Background(), job)

	var snooze *river.JobSnoozeError
	require.ErrorAs(t, err, &snooze,
		"budget exhaustion co-occurring with a genuine error still snoozes (self-resolving tradeoff)")
}

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
		ObservedAt: accelerated.GetCurrentTime(),
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
