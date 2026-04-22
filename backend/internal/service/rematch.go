package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Method is a single (type, normalized value) pair that a rematch handler can
// dispatch against — e.g. ("email", "alice@example.com").
type Method struct {
	Type  string
	Value string
}

// RematchHandler is implemented per identifier type. When a contact method of
// that type is added, the handler is invoked once per identifier to link any
// pre-existing external records (calendar events, telegram messages, ...) to
// the CRM contact. The returned count is how many records were newly linked.
type RematchHandler interface {
	IdentifierType() string
	Rematch(ctx context.Context, contactID uuid.UUID, value string) (int, error)
}

// JobStatus represents the lifecycle state of a rematch job.
type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

// ErrJobNotFound is returned by GetJob when the job ID is unknown.
var ErrJobNotFound = errors.New("rematch job not found")

// JobProgress is the externally-visible snapshot of a rematch job.
type JobProgress struct {
	ID          uuid.UUID
	ContactID   uuid.UUID
	Methods     []Method
	Status      JobStatus
	Matched     int
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
}

// RematchRegistry is the narrow contract consumed by ContactService /
// EnrichmentService / rescan handlers for registering an in-memory job
// entry synchronously after events.Bus.PublishTx. Keeps publishers
// decoupled from the full *RematchService surface — they just need
// "make the job visible to GET /rematch/jobs/:id now" so the frontend's
// synchronous `rematch_job_id` poll loop never 404s.
type RematchRegistry interface {
	RegisterPending(jobID, contactID uuid.UUID, methods []Method)
}

// job is the internal mutable representation protected by its own mutex.
type job struct {
	id        uuid.UUID
	contactID uuid.UUID
	methods   []Method
	startedAt time.Time

	mu          sync.RWMutex
	status      JobStatus
	matched     int
	completedAt *time.Time
	err         string
}

func (j *job) snapshot() JobProgress {
	j.mu.RLock()
	defer j.mu.RUnlock()
	methodsCopy := make([]Method, len(j.methods))
	copy(methodsCopy, j.methods)
	var completed *time.Time
	if j.completedAt != nil {
		t := *j.completedAt
		completed = &t
	}
	return JobProgress{
		ID:          j.id,
		ContactID:   j.contactID,
		Methods:     methodsCopy,
		Status:      j.status,
		Matched:     j.matched,
		StartedAt:   j.startedAt,
		CompletedAt: completed,
		Error:       j.err,
	}
}

func (j *job) addMatched(n int) {
	j.mu.Lock()
	j.matched += n
	j.mu.Unlock()
}

func (j *job) setCompleted() {
	j.mu.Lock()
	j.status = JobStatusCompleted
	t := accelerated.GetCurrentTime()
	j.completedAt = &t
	j.mu.Unlock()
}

func (j *job) setFailed(err error) {
	j.mu.Lock()
	j.status = JobStatusFailed
	t := accelerated.GetCurrentTime()
	j.completedAt = &t
	if err != nil {
		j.err = err.Error()
	}
	j.mu.Unlock()
}

// resetForRun clears the mutable run-state fields before a fresh
// attempt. Called from rehydrateOrLookup so a River retry after a crash
// or failure starts clean instead of accumulating matched counts on top
// of the prior attempt or keeping the prior attempt's error / completedAt.
func (j *job) resetForRun() {
	j.mu.Lock()
	j.status = JobStatusRunning
	j.matched = 0
	j.completedAt = nil
	j.err = ""
	j.mu.Unlock()
}

// RematchService dispatches rematch work per contact method type. Handlers
// register at startup. Post-PR-10 (#180) the primary entry point is Run,
// which the RematchDispatcher consumer invokes per contact_methods.added
// event. StartRematchForContact is retained as a test-only helper that
// spawns Run on the detached context; production code publishes events
// and relies on the consumer to dispatch.
type RematchService struct {
	handlers     map[string]RematchHandler
	jobs         sync.Map // uuid.UUID -> *job
	contactLocks sync.Map // uuid.UUID -> *sync.Mutex

	// detachedCtx returns a fresh context for the background goroutine.
	// Overridable in tests.
	detachedCtx func() context.Context
}

// NewRematchService constructs a RematchService with no handlers registered.
// Callers register handlers via Register before use.
func NewRematchService() *RematchService {
	return &RematchService{
		handlers:    make(map[string]RematchHandler),
		detachedCtx: func() context.Context { return context.Background() },
	}
}

// Register adds a handler for a specific identifier type. Intended to be
// called at startup before any jobs run.
func (s *RematchService) Register(h RematchHandler) {
	s.handlers[h.IdentifierType()] = h
}

// jobRetention bounds how long terminal jobs remain queryable via GetJob.
// After a job completes/fails, clients have this long to poll for the final
// state before the entry is evicted on the next RegisterPending /
// StartRematchForContact call. Keeps the in-memory job map bounded without
// requiring a separate reaper.
const jobRetention = 10 * time.Minute

// RegisterPending creates an in-memory job entry keyed by jobID with
// Status=Running so GET /rematch/jobs/:id returns it immediately after
// a publisher publishes a contact_methods.added event — i.e. before the
// async RematchDispatcher consumer picks up. Idempotent on duplicate
// jobID (second call is a no-op) so publisher retries don't clobber
// in-progress run state.
//
// The spec text at §3.4.4 calls this the "pending" state; the existing
// JobStatus enum has only running|completed|failed. We reuse Running
// rather than adding a new terminal state because the frontend contract
// (`useRematchJob` polls until terminal) already treats Running as
// "not done, keep polling" — introducing Pending would break
// `frontend/src/lib/rematch-api.ts`'s three-state shape.
func (s *RematchService) RegisterPending(jobID, contactID uuid.UUID, methods []Method) {
	if jobID == uuid.Nil {
		return
	}
	// Opportunistic prune on registration — keeps the map bounded for
	// long-running processes without a dedicated reaper.
	s.pruneTerminalJobs()
	j := &job{
		id:        jobID,
		contactID: contactID,
		methods:   append([]Method(nil), methods...),
		startedAt: accelerated.GetCurrentTime(),
		status:    JobStatusRunning,
	}
	// LoadOrStore keeps the first writer — second call is a no-op so
	// publisher retries or a race with the consumer's rehydrate don't
	// clobber an in-progress entry.
	s.jobs.LoadOrStore(jobID, j)
}

// StartRematchForContact filters the given methods to those that have a
// registered handler, then spawns a detached goroutine to run them.
// Returns uuid.Nil when no methods map to a registered handler.
//
// Deprecated: production code publishes contact_methods.added via
// events.Bus.PublishTx and relies on RematchDispatcher to invoke Run.
// Kept alive for tests that exercise the in-process spawn path.
func (s *RematchService) StartRematchForContact(contactID uuid.UUID, methods []Method) uuid.UUID {
	eligible := make([]Method, 0, len(methods))
	for _, m := range methods {
		if _, ok := s.handlers[m.Type]; ok {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		return uuid.Nil
	}

	// Opportunistic prune on dispatch — keeps the map bounded for
	// long-running processes without a dedicated reaper goroutine.
	s.pruneTerminalJobs()

	jobID := uuid.New()
	s.RegisterPending(jobID, contactID, eligible)

	// Fire-and-forget; Run records failure on the in-memory entry via
	// setFailed and returns an error for River semantics — for this
	// test-only spawn we discard it since there is no River wrapper.
	go func() {
		_ = s.Run(s.detachedCtx(), jobID, contactID, eligible)
	}()
	return jobID
}

// pruneTerminalJobs evicts completed/failed jobs whose terminal timestamp is
// older than jobRetention. Called opportunistically before each new dispatch.
func (s *RematchService) pruneTerminalJobs() {
	cutoff := accelerated.GetCurrentTime().Add(-jobRetention)
	s.jobs.Range(func(key, value any) bool {
		j := value.(*job)
		j.mu.RLock()
		terminal := j.status != JobStatusRunning
		var completedAt time.Time
		if j.completedAt != nil {
			completedAt = *j.completedAt
		}
		j.mu.RUnlock()
		if terminal && completedAt.Before(cutoff) {
			s.jobs.Delete(key)
		}
		return true
	})
}

// Run executes a rematch job for the given (jobID, contactID, methods)
// tuple under per-contact mutex serialization. Called by
// RematchDispatcher.HandleEvent (production) and via StartRematchForContact
// (tests only).
//
// Rehydrates the in-memory *job entry from parameters when none exists
// (worker retry after crash, or consumer pickup before RegisterPending
// ran) and resets mutable run-state (matched / status / completedAt /
// err) so a retry begins from a clean slate.
//
// The named `err` return is crucial: a recovered panic is assigned to
// err via the deferred recover, so River sees the error and retries the
// job per its MaxAttempts. Without the named return, a bare `return`
// after setFailed would hand back a zero-valued nil error and River
// would ack the job as complete.
func (s *RematchService) Run(ctx context.Context, jobID, contactID uuid.UUID, methods []Method) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Str("contactId", contactID.String()).
				Str("jobId", jobID.String()).
				Interface("panic", r).
				Msg("rematch: job panicked")
			panicErr := fmt.Errorf("handler panic: %v", r)
			if j, ok := s.jobs.Load(jobID); ok {
				j.(*job).setFailed(panicErr)
			}
			// Propagate so River retries per MaxAttempts. A named return
			// is required so this deferred assignment survives `return`.
			err = panicErr
		}
	}()

	j := s.rehydrateOrLookup(jobID, contactID, methods)

	lockIface, _ := s.contactLocks.LoadOrStore(contactID, &sync.Mutex{})
	lock := lockIface.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	for _, m := range j.methods {
		handler, ok := s.handlers[m.Type]
		if !ok {
			continue
		}
		n, handlerErr := handler.Rematch(ctx, contactID, m.Value)
		if handlerErr != nil {
			logger.Warn().Err(handlerErr).
				Str("contactId", contactID.String()).
				Str("type", m.Type).
				Msg("rematch: handler failed")
			j.setFailed(handlerErr)
			return handlerErr
		}
		j.addMatched(n)
	}
	j.setCompleted()
	return nil
}

// rehydrateOrLookup returns the job for jobID, creating a fresh in-memory
// entry when none exists (process crashed between RegisterPending and
// consumer pickup, or a River retry picked up the job on a worker that
// never saw the publisher's registration). Resets mutable run-state on
// every call so a retry begins clean — without this the second attempt's
// matched counts accumulate on top of the first attempt's partial counts
// and the old error/completedAt linger on the entry.
func (s *RematchService) rehydrateOrLookup(jobID, contactID uuid.UUID, methods []Method) *job {
	j := &job{
		id:        jobID,
		contactID: contactID,
		methods:   append([]Method(nil), methods...),
		startedAt: accelerated.GetCurrentTime(),
		status:    JobStatusRunning,
	}
	if actual, loaded := s.jobs.LoadOrStore(jobID, j); loaded {
		j = actual.(*job)
	}
	j.resetForRun()
	return j
}

// GetJob returns a snapshot of the job, or ErrJobNotFound if unknown.
func (s *RematchService) GetJob(id uuid.UUID) (JobProgress, error) {
	v, ok := s.jobs.Load(id)
	if !ok {
		return JobProgress{}, ErrJobNotFound
	}
	return v.(*job).snapshot(), nil
}

// RescanContact triggers a full rematch for every method currently on the
// contact. Resolves the methods via the supplied ContactService so the
// handler layer doesn't need a direct repository dependency.
//
// Deprecated: kept so handler compilation stays green during the PR-10
// step-wise cutover. Step 7 moves the HTTP rescan path onto
// ContactService.RescanRematch (event-bus publish) and deletes this method.
func (s *RematchService) RescanContact(ctx context.Context, contactSvc *ContactService, contactID uuid.UUID) (uuid.UUID, error) {
	contact, err := contactSvc.GetContact(ctx, contactID)
	if err != nil {
		return uuid.Nil, err
	}
	return s.StartRematchForContact(contactID, toRematchMethods(contact.Methods)), nil
}

// diffNewMethods returns methods present in `after` whose (type,
// value_normalized) pair is not in `before`. Order-insensitive.
func diffNewMethods(before, after []repository.ContactMethod) []Method {
	existing := make(map[string]struct{}, len(before))
	for _, m := range before {
		existing[m.Type+"|"+m.ValueNormalized] = struct{}{}
	}
	out := make([]Method, 0, len(after))
	for _, m := range after {
		key := m.Type + "|" + m.ValueNormalized
		if _, ok := existing[key]; ok {
			continue
		}
		out = append(out, Method{Type: m.Type, Value: m.ValueNormalized})
	}
	return out
}

// toRematchMethods converts a slice of ContactMethod to rematch Methods using
// the normalized value.
func toRematchMethods(methods []repository.ContactMethod) []Method {
	out := make([]Method, len(methods))
	for i, m := range methods {
		out[i] = Method{Type: m.Type, Value: m.ValueNormalized}
	}
	return out
}
