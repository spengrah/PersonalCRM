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

func TestStartRematchForContact_AggregatesCountsAcrossMethods(t *testing.T) {
	svc := NewRematchService()
	emailH := &stubHandler{typ: "email", matched: 2}
	phoneH := &stubHandler{typ: "phone", matched: 4}
	svc.Register(emailH)
	svc.Register(phoneH)

	jobID := svc.StartRematchForContact(uuid.New(), []Method{
		{Type: "email", Value: "a@b.c"},
		{Type: "phone", Value: "+15551212"},
	})
	job := waitForJob(t, svc, jobID)
	if job.Status != JobStatusCompleted {
		t.Fatalf("expected completed, got %s", job.Status)
	}
	if job.Matched != 6 {
		t.Fatalf("expected Matched=6, got %d", job.Matched)
	}
}

func TestStartRematchForContact_HandlerErrorFailsJob(t *testing.T) {
	svc := NewRematchService()
	errBoom := errors.New("boom")
	svc.Register(&stubHandler{typ: "email", err: errBoom})

	jobID := svc.StartRematchForContact(uuid.New(), []Method{{Type: "email", Value: "a@b.c"}})
	job := waitForJob(t, svc, jobID)
	if job.Status != JobStatusFailed {
		t.Fatalf("expected failed, got %s", job.Status)
	}
	if job.Error == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestStartRematchForContact_PanicRecovered(t *testing.T) {
	svc := NewRematchService()
	svc.Register(panickyHandler{})
	jobID := svc.StartRematchForContact(uuid.New(), []Method{{Type: "email", Value: "x"}})
	job := waitForJob(t, svc, jobID)
	if job.Status != JobStatusFailed {
		t.Fatalf("expected failed after panic, got %s", job.Status)
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

func TestDiffNewMethods_NormalizedKey(t *testing.T) {
	before := []repository.ContactMethod{
		{Type: "email", ValueNormalized: "alice@example.com"},
	}
	after := []repository.ContactMethod{
		{Type: "email", ValueNormalized: "alice@example.com"}, // unchanged
		{Type: "email", ValueNormalized: "bob@example.com"},   // new
		{Type: "phone", ValueNormalized: "+15551212"},         // new
	}
	diff := diffNewMethods(before, after)
	if len(diff) != 2 {
		t.Fatalf("expected 2 new methods, got %d: %+v", len(diff), diff)
	}
	wantValues := map[string]bool{"bob@example.com": true, "+15551212": true}
	for _, m := range diff {
		if !wantValues[m.Value] {
			t.Fatalf("unexpected method %+v", m)
		}
	}
}

func TestDiffNewMethods_EmptyInputs(t *testing.T) {
	if got := diffNewMethods(nil, nil); len(got) != 0 {
		t.Fatalf("expected empty diff, got %+v", got)
	}
	if got := diffNewMethods(nil, []repository.ContactMethod{{Type: "email", ValueNormalized: "a@b"}}); len(got) != 1 {
		t.Fatalf("expected 1 new, got %d", len(got))
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
