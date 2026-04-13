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

// RematchService dispatches rematch work per contact method type. Handlers
// register at startup; StartRematchForContact spawns a background goroutine
// that runs handlers sequentially for the given methods, serialized per
// contactID by a mutex.
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

// StartRematchForContact filters the given methods to those that have a
// registered handler, then spawns a detached goroutine to run them. Returns
// uuid.Nil when no methods map to a registered handler — this is the normal
// case when the feature is off or only some handler types are wired.
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

	j := &job{
		id:        uuid.New(),
		contactID: contactID,
		methods:   eligible,
		startedAt: accelerated.GetCurrentTime(),
		status:    JobStatusRunning,
	}
	s.jobs.Store(j.id, j)

	go s.run(j)
	return j.id
}

func (s *RematchService) run(j *job) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Str("contactId", j.contactID.String()).
				Str("jobId", j.id.String()).
				Interface("panic", r).
				Msg("rematch: job panicked")
			j.setFailed(fmt.Errorf("handler panic: %v", r))
		}
	}()

	lockIface, _ := s.contactLocks.LoadOrStore(j.contactID, &sync.Mutex{})
	lock := lockIface.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	ctx := s.detachedCtx()

	for _, m := range j.methods {
		handler, ok := s.handlers[m.Type]
		if !ok {
			continue
		}
		n, err := handler.Rematch(ctx, j.contactID, m.Value)
		if err != nil {
			logger.Warn().Err(err).
				Str("contactId", j.contactID.String()).
				Str("type", m.Type).
				Msg("rematch: handler failed")
			j.setFailed(err)
			return
		}
		j.addMatched(n)
	}

	j.setCompleted()
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
