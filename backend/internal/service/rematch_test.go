package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// stubHandler records invocations and returns a configurable matched count / err.
type stubHandler struct {
	typ     string
	matched int
	err     error

	mu     sync.Mutex
	calls  []stubCall
	before func()
	after  func()
	sleep  time.Duration
}

type stubCall struct {
	contactID uuid.UUID
	value     string
}

func (s *stubHandler) IdentifierType() string { return s.typ }

func (s *stubHandler) Rematch(ctx context.Context, contactID uuid.UUID, value string) (int, error) {
	if s.before != nil {
		s.before()
	}
	if s.sleep > 0 {
		time.Sleep(s.sleep)
	}
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{contactID: contactID, value: value})
	s.mu.Unlock()
	if s.after != nil {
		s.after()
	}
	if s.err != nil {
		return 0, s.err
	}
	return s.matched, nil
}

func (s *stubHandler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func waitForJob(t *testing.T, svc *RematchService, jobID uuid.UUID) JobProgress {
	t.Helper()
	// Poll up to 2s in 5ms steps.
	const maxAttempts = 400
	for range maxAttempts {
		j, err := svc.GetJob(jobID)
		if err == nil && j.Status != JobStatusRunning {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("rematch job %s did not finish in time", jobID)
	return JobProgress{}
}

func TestStartRematchForContact_NoMatchingHandler(t *testing.T) {
	svc := NewRematchService()
	jobID := svc.StartRematchForContact(uuid.New(), []Method{{Type: "email", Value: "a@b.c"}})
	if jobID != uuid.Nil {
		t.Fatalf("expected uuid.Nil when no handlers registered, got %s", jobID)
	}
}

// TestEligibleMethods_FiltersToRegisteredHandlers pins the publisher
// contract: ContactService / EnrichmentService / RescanRematch call
// EligibleMethods BEFORE minting a jobID, so unhandled method types
// produce uuid.Nil in the API response (matches pre-cutover
// StartRematchForContact semantics).
func TestEligibleMethods_FiltersToRegisteredHandlers(t *testing.T) {
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email"})
	svc.Register(&stubHandler{typ: "telegram"})

	input := []Method{
		{Type: "email", Value: "a@b.c"},
		{Type: "phone", Value: "+15551212"},
		{Type: "telegram", Value: "alice"},
		{Type: "discord", Value: "alice#1"},
	}
	// spec: IMP-019[0]
	got := svc.EligibleMethods(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 eligible methods, got %d: %+v", len(got), got)
	}
	// Order preserved from input slice.
	if got[0].Type != "email" || got[1].Type != "telegram" {
		t.Fatalf("unexpected eligible methods: %+v", got)
	}
}

func TestEligibleMethods_EmptyWhenNoHandlers(t *testing.T) {
	svc := NewRematchService()
	got := svc.EligibleMethods([]Method{{Type: "email", Value: "a@b.c"}})
	if len(got) != 0 {
		t.Fatalf("expected 0 eligible methods with no handlers registered, got %d", len(got))
	}
}

func TestEligibleMethods_EmptyInput(t *testing.T) {
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email"})
	got := svc.EligibleMethods(nil)
	if got != nil {
		t.Fatalf("expected nil for empty input, got %+v", got)
	}
}

func TestStartRematchForContact_DispatchesToHandler(t *testing.T) {
	svc := NewRematchService()
	h := &stubHandler{typ: "email", matched: 3}
	svc.Register(h)

	contactID := uuid.New()
	jobID := svc.StartRematchForContact(contactID, []Method{{Type: "email", Value: "a@b.c"}})
	if jobID == uuid.Nil {
		t.Fatal("expected non-nil jobID")
	}

	job := waitForJob(t, svc, jobID)
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 3 {
		t.Fatalf("expected Matched=3, got %d", job.Matched)
	}
	if h.callCount() != 1 {
		t.Fatalf("expected 1 handler call, got %d", h.callCount())
	}
	if h.calls[0].contactID != contactID || h.calls[0].value != "a@b.c" {
		t.Fatalf("unexpected handler args: %+v", h.calls[0])
	}
}

func TestRun_AggregatesCountsAcrossMethods(t *testing.T) {
	svc := NewRematchService()
	emailH := &stubHandler{typ: "email", matched: 2}
	phoneH := &stubHandler{typ: "phone", matched: 4}
	svc.Register(emailH)
	svc.Register(phoneH)

	jobID := uuid.New()
	contactID := uuid.New()
	methods := []Method{
		{Type: "email", Value: "a@b.c"},
		{Type: "phone", Value: "+15551212"},
	}
	if err := svc.Run(context.Background(), jobID, contactID, methods); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 6 {
		t.Fatalf("expected Matched=6, got %d", job.Matched)
	}
}

func TestRun_HandlerErrorFailsJob(t *testing.T) {
	svc := NewRematchService()
	errBoom := errors.New("boom")
	svc.Register(&stubHandler{typ: "email", err: errBoom})

	jobID := uuid.New()
	runErr := svc.Run(context.Background(), jobID, uuid.New(), []Method{{Type: "email", Value: "a@b.c"}})
	if !errors.Is(runErr, errBoom) {
		t.Fatalf("Run should return handler error for river retry; got %v", runErr)
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("expected failed, got %s", job.Status)
	}
	if job.Error == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestRun_PanicPropagatesAsError(t *testing.T) {
	svc := NewRematchService()
	svc.Register(panickyHandler{})
	jobID := uuid.New()
	runErr := svc.Run(context.Background(), jobID, uuid.New(), []Method{{Type: "email", Value: "x"}})
	if runErr == nil {
		t.Fatal("Run should return non-nil error when handler panics (named-return recovery)")
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("expected failed after panic, got %s", job.Status)
	}
	if job.Error == "" {
		t.Fatal("expected non-empty error message after panic")
	}
}

func TestRun_NoMatchingHandler_Completes(t *testing.T) {
	// Run with no registered handler for the method type completes
	// cleanly (handler iteration skips unknown types) → terminal
	// Completed with matched=0. Distinct from
	// TestStartRematchForContact_NoMatchingHandler which exercises the
	// pre-spawn eligibility filter.
	svc := NewRematchService()
	jobID := uuid.New()
	if err := svc.Run(context.Background(), jobID, uuid.New(), []Method{{Type: "email", Value: "x"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 0 {
		t.Fatalf("expected Matched=0, got %d", job.Matched)
	}
}

func TestRun_RehydrateOrLookup_CreatesEntry(t *testing.T) {
	// Call Run on a jobID that was never RegisterPending'd (simulating
	// consumer pickup after a crash that lost the in-memory map).
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email", matched: 1})

	jobID := uuid.New()
	contactID := uuid.New()
	if err := svc.Run(context.Background(), jobID, contactID, []Method{{Type: "email", Value: "a@b.c"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.ContactID != contactID {
		t.Fatalf("expected contactID=%s, got %s", contactID, job.ContactID)
	}
	if job.Matched != 1 {
		t.Fatalf("expected Matched=1, got %d", job.Matched)
	}
}

func TestRun_RehydrateOrLookup_LoadsExistingEntry(t *testing.T) {
	// RegisterPending first, then Run: no duplicate entry, counts
	// accumulate on the same job.
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email", matched: 2})

	jobID := uuid.New()
	contactID := uuid.New()
	methods := []Method{{Type: "email", Value: "a@b.c"}}

	svc.RegisterPending(jobID, contactID, methods)
	snap, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob after RegisterPending: %v", err)
	}
	if snap.Status != JobStatusRunning {
		t.Fatalf("expected Running after RegisterPending, got %s", snap.Status)
	}
	if snap.Matched != 0 {
		t.Fatalf("expected Matched=0 before Run, got %d", snap.Matched)
	}

	if err := svc.Run(context.Background(), jobID, contactID, methods); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob after Run: %v", err)
	}
	if final.Status != JobStatusCompleted {
		t.Fatalf("expected Completed, got %s", final.Status)
	}
	if final.Matched != 2 {
		t.Fatalf("expected Matched=2, got %d", final.Matched)
	}
	// StartedAt must be the RegisterPending-assigned value, not a fresh one.
	if !final.StartedAt.Equal(snap.StartedAt) {
		t.Fatalf("StartedAt should match RegisterPending snapshot; got %v vs %v",
			final.StartedAt, snap.StartedAt)
	}
}

func TestRun_RetryAfterFailure_ResetsState(t *testing.T) {
	// First Run fails; second Run with same jobID succeeds. Assert
	// the second attempt's state replaces (not accumulates on) the
	// first attempt's partial state.
	svc := NewRematchService()
	errBoom := errors.New("boom")
	failing := &stubHandler{typ: "email", err: errBoom}
	svc.Register(failing)

	jobID := uuid.New()
	contactID := uuid.New()
	methods := []Method{{Type: "email", Value: "a@b.c"}}

	if err := svc.Run(context.Background(), jobID, contactID, methods); !errors.Is(err, errBoom) {
		t.Fatalf("first Run should fail with boom, got %v", err)
	}
	firstSnap, _ := svc.GetJob(jobID)
	if firstSnap.Status != JobStatusFailed {
		t.Fatalf("expected Failed after first run, got %s", firstSnap.Status)
	}

	// Swap in a successful handler for the retry and re-run.
	svc.handlers["email"] = []RematchHandler{&stubHandler{typ: "email", matched: 5}}
	if err := svc.Run(context.Background(), jobID, contactID, methods); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	final, _ := svc.GetJob(jobID)
	if final.Status != JobStatusCompleted {
		t.Fatalf("expected Completed after retry, got %s", final.Status)
	}
	if final.Matched != 5 {
		t.Fatalf("expected Matched=5 (only second attempt), got %d", final.Matched)
	}
	if final.Error != "" {
		t.Fatalf("expected cleared Error after retry, got %q", final.Error)
	}
	if final.CompletedAt == nil || firstSnap.CompletedAt == nil {
		t.Fatal("expected non-nil CompletedAt on both snapshots")
	}
	if !final.CompletedAt.After(*firstSnap.CompletedAt) &&
		!final.CompletedAt.Equal(*firstSnap.CompletedAt) {
		// Accelerated clock may produce identical timestamps; accept equal or after.
		t.Fatalf("expected retry CompletedAt >= first; got %v vs %v",
			final.CompletedAt, firstSnap.CompletedAt)
	}
}

func TestRegisterPending_CreatesRunningEntry(t *testing.T) {
	svc := NewRematchService()
	jobID := uuid.New()
	contactID := uuid.New()
	methods := []Method{{Type: "email", Value: "a@b.c"}}

	svc.RegisterPending(jobID, contactID, methods)
	snap, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if snap.Status != JobStatusRunning {
		t.Fatalf("expected Running, got %s", snap.Status)
	}
	if snap.ContactID != contactID {
		t.Fatalf("expected contactID=%s, got %s", contactID, snap.ContactID)
	}
	if len(snap.Methods) != 1 || snap.Methods[0].Value != "a@b.c" {
		t.Fatalf("unexpected methods: %+v", snap.Methods)
	}
}

func TestRegisterPending_Idempotent(t *testing.T) {
	svc := NewRematchService()
	jobID := uuid.New()
	contactID := uuid.New()

	svc.RegisterPending(jobID, contactID, []Method{{Type: "email", Value: "a@b.c"}})
	first, _ := svc.GetJob(jobID)

	// Second call with different methods should be a no-op.
	svc.RegisterPending(jobID, contactID, []Method{{Type: "phone", Value: "+1"}})
	second, _ := svc.GetJob(jobID)

	if second.StartedAt != first.StartedAt {
		t.Fatalf("second RegisterPending bumped StartedAt: %v vs %v", second.StartedAt, first.StartedAt)
	}
	if len(second.Methods) != 1 || second.Methods[0].Value != "a@b.c" {
		t.Fatalf("expected first call's methods preserved, got %+v", second.Methods)
	}
}

func TestRegisterPending_NilJobID_NoOp(t *testing.T) {
	svc := NewRematchService()
	svc.RegisterPending(uuid.Nil, uuid.New(), []Method{{Type: "email", Value: "x"}})
	if _, err := svc.GetJob(uuid.Nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound for nil jobID, got %v", err)
	}
}

type panickyHandler struct{}

func (panickyHandler) IdentifierType() string { return "email" }
func (panickyHandler) Rematch(ctx context.Context, contactID uuid.UUID, value string) (int, error) {
	panic("intentional")
}

func TestPerContactMutex_SerializesConcurrentJobs(t *testing.T) {
	svc := NewRematchService()

	var active int32
	var maxActive int32
	sleep := 40 * time.Millisecond

	h := &stubHandler{
		typ:   "email",
		sleep: sleep,
		before: func() {
			n := atomic.AddInt32(&active, 1)
			for {
				cur := atomic.LoadInt32(&maxActive)
				if n <= cur || atomic.CompareAndSwapInt32(&maxActive, cur, n) {
					break
				}
			}
		},
		after: func() { atomic.AddInt32(&active, -1) },
	}
	svc.Register(h)

	contactID := uuid.New()
	j1 := svc.StartRematchForContact(contactID, []Method{{Type: "email", Value: "a"}})
	j2 := svc.StartRematchForContact(contactID, []Method{{Type: "email", Value: "b"}})

	waitForJob(t, svc, j1)
	waitForJob(t, svc, j2)

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("expected at most 1 concurrent handler call for same contact, got %d", got)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	svc := NewRematchService()
	if _, err := svc.GetJob(uuid.New()); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}
}

func TestPruneTerminalJobs_EvictsOldTerminalButKeepsFresh(t *testing.T) {
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email"})

	// Dispatch a job and let it complete.
	oldID := svc.StartRematchForContact(uuid.New(), []Method{{Type: "email", Value: "x@y.z"}})
	waitForJob(t, svc, oldID)

	// Backdate completedAt so the prune considers it expired. Use a fixed
	// epoch-anchored past time — well beyond jobRetention regardless of the
	// current clock.
	v, _ := svc.jobs.Load(oldID)
	oldJob := v.(*job)
	oldJob.mu.Lock()
	stale := time.Unix(0, 0)
	oldJob.completedAt = &stale
	oldJob.mu.Unlock()

	// A fresh dispatch should prune the stale job but keep the new one.
	freshID := svc.StartRematchForContact(uuid.New(), []Method{{Type: "email", Value: "new@y.z"}})
	waitForJob(t, svc, freshID)

	if _, err := svc.GetJob(oldID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected stale terminal job to be pruned, got err=%v", err)
	}
	if _, err := svc.GetJob(freshID); err != nil {
		t.Fatalf("expected fresh job to still be queryable, got err=%v", err)
	}
}

func TestToRematchMethods(t *testing.T) {
	in := []repository.ContactMethod{
		{Type: "email", ValueNormalized: "x@y.z"},
		{Type: "phone", ValueNormalized: "+15551212"},
	}
	out := toRematchMethods(in)
	if len(out) != 2 || out[0].Value != "x@y.z" || out[1].Value != "+15551212" {
		t.Fatalf("unexpected output: %+v", out)
	}
}

// --- fan-out (multiple handlers per type) -----------------------------------

// TestRegister_AppendsMultipleHandlersForSameType pins that Register appends
// rather than overwrites, so a type can fan out to several handlers (calendar +
// gmail both rematch "email").
func TestRegister_AppendsMultipleHandlersForSameType(t *testing.T) {
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email"})
	svc.Register(&stubHandler{typ: "email"})
	if got := len(svc.handlers["email"]); got != 2 {
		t.Fatalf("expected 2 handlers registered for email, got %d", got)
	}
}

// TestRun_FanOut_BothEmailHandlersFire pins that Run dispatches to every
// handler registered for a method type and sums their matched counts.
func TestRun_FanOut_BothEmailHandlersFire(t *testing.T) {
	svc := NewRematchService()
	h1 := &stubHandler{typ: "email", matched: 2}
	h2 := &stubHandler{typ: "email", matched: 3}
	svc.Register(h1)
	svc.Register(h2)

	jobID := uuid.New()
	contactID := uuid.New()
	if err := svc.Run(context.Background(), jobID, contactID, []Method{{Type: "email", Value: "a@b.c"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h1.callCount() != 1 {
		t.Fatalf("expected handler 1 to fire once, got %d", h1.callCount())
	}
	if h2.callCount() != 1 {
		t.Fatalf("expected handler 2 to fire once, got %d", h2.callCount())
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 5 {
		t.Fatalf("expected Matched=5 (2+3 summed across handlers), got %d", job.Matched)
	}
}

// TestRun_CalendarOnly_StillFires is the behavior-preserving invariant: with a
// single handler registered for "email" (today's prod state — calendar only),
// the refactor fires it exactly once and completes, identical to pre-refactor.
func TestRun_CalendarOnly_StillFires(t *testing.T) {
	svc := NewRematchService()
	calendarLike := &stubHandler{typ: "email", matched: 7}
	svc.Register(calendarLike)

	jobID := uuid.New()
	contactID := uuid.New()
	if err := svc.Run(context.Background(), jobID, contactID, []Method{{Type: "email", Value: "a@b.c"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calendarLike.callCount() != 1 {
		t.Fatalf("expected the sole email handler to fire once, got %d", calendarLike.callCount())
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 7 {
		t.Fatalf("expected Matched=7, got %d", job.Matched)
	}
}

// TestRun_FanOut_NonEmailTypesUnaffected pins that fan-out is per-type: two
// email handlers both fire, the telegram handler fires once, and counts sum
// correctly across types.
func TestRun_FanOut_NonEmailTypesUnaffected(t *testing.T) {
	svc := NewRematchService()
	email1 := &stubHandler{typ: "email", matched: 1}
	email2 := &stubHandler{typ: "email", matched: 1}
	telegram := &stubHandler{typ: "telegram", matched: 4}
	svc.Register(email1)
	svc.Register(email2)
	svc.Register(telegram)

	jobID := uuid.New()
	contactID := uuid.New()
	methods := []Method{
		{Type: "email", Value: "a@b.c"},
		{Type: "telegram", Value: "alice"},
	}
	if err := svc.Run(context.Background(), jobID, contactID, methods); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if email1.callCount() != 1 || email2.callCount() != 1 {
		t.Fatalf("expected both email handlers to fire once each, got %d and %d",
			email1.callCount(), email2.callCount())
	}
	if telegram.callCount() != 1 {
		t.Fatalf("expected telegram handler to fire once, got %d", telegram.callCount())
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Matched != 6 {
		t.Fatalf("expected Matched=6 (1+1 email + 4 telegram), got %d", job.Matched)
	}
}

// TestRun_FanOut_FirstHandlerErrorFailsJob pins fail-fast across the fan-out:
// the first handler's error fails the job and the second handler does NOT run.
func TestRun_FanOut_FirstHandlerErrorFailsJob(t *testing.T) {
	svc := NewRematchService()
	errBoom := errors.New("boom")
	failing := &stubHandler{typ: "email", err: errBoom}
	second := &stubHandler{typ: "email", matched: 5}
	svc.Register(failing)
	svc.Register(second)

	jobID := uuid.New()
	runErr := svc.Run(context.Background(), jobID, uuid.New(), []Method{{Type: "email", Value: "a@b.c"}})
	if !errors.Is(runErr, errBoom) {
		t.Fatalf("expected Run to return the first handler's error, got %v", runErr)
	}
	if failing.callCount() != 1 {
		t.Fatalf("expected failing handler to fire once, got %d", failing.callCount())
	}
	if second.callCount() != 0 {
		t.Fatalf("expected second handler NOT to fire after fail-fast, got %d calls", second.callCount())
	}
	job, err := svc.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("expected failed, got %s", job.Status)
	}
}

// TestEligibleMethods_PresenceViaSliceLen pins that the eligibility presence
// check is len(handlers[type]) > 0 — with multiple email handlers registered,
// email is eligible and an unhandled type (phone) is not.
func TestEligibleMethods_PresenceViaSliceLen(t *testing.T) {
	svc := NewRematchService()
	svc.Register(&stubHandler{typ: "email"})
	svc.Register(&stubHandler{typ: "email"})

	got := svc.EligibleMethods([]Method{
		{Type: "email", Value: "a@b.c"},
		{Type: "phone", Value: "+15551212"},
	})
	if len(got) != 1 || got[0].Type != "email" {
		t.Fatalf("expected only email eligible, got %+v", got)
	}
}
