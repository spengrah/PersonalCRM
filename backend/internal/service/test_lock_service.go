package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrLockWaitTimeout is returned when a lock could not be acquired within
// the caller's wait budget.
var ErrLockWaitTimeout = errors.New("lock wait timeout")

// TestLockService is an in-process named-mutex arbiter for the E2E suite
// (test-only routes, CRM_ENV=testing). Playwright workers are separate OS
// processes that already share this backend, so arbitrating here gives
// single-arbiter mutual exclusion with none of the classic filesystem-lock
// races (stale-break TOCTOU, ownership-blind release). Isolated E2E lanes
// run their own backend, so lock names never collide across lanes.
//
// Leases expire after a TTL so a SIGKILLed holder (a crashed Playwright
// worker) cannot deadlock the suite: live holders renew periodically, dead
// ones stop and their lease lapses. All state dies with the process, which
// is the correct lifetime for a test-run mutex.
type TestLockService struct {
	now func() time.Time

	mu     sync.Mutex
	held   map[string]*testLease // lock name -> current lease
	leases map[string]string     // lease id -> lock name
}

type testLease struct {
	id        string
	expiresAt time.Time
}

// NewTestLockService creates the arbiter. The clock is injected so tests
// control expiry; production wiring passes accelerated.GetCurrentTime.
func NewTestLockService(now func() time.Time) *TestLockService {
	return &TestLockService{
		now:    now,
		held:   make(map[string]*testLease),
		leases: make(map[string]string),
	}
}

// Acquire blocks until the named lock is free (or its holder's lease has
// expired) and returns a new lease id, or fails with ErrLockWaitTimeout
// once wait has elapsed. Expiry and takeover happen atomically under one
// mutex, so concurrent waiters racing an expired lease admit exactly one.
func (s *TestLockService) Acquire(ctx context.Context, name string, ttl, wait time.Duration) (string, error) {
	deadline := s.now().Add(wait)
	for {
		if lease, ok := s.tryAcquire(name, ttl); ok {
			return lease, nil
		}
		if !s.now().Before(deadline) {
			return "", fmt.Errorf("acquire %q: %w", name, ErrLockWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *TestLockService) tryAcquire(name string, ttl time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.held[name]; ok && cur.expiresAt.After(s.now()) {
		return "", false
	} else if ok {
		delete(s.leases, cur.id)
	}
	lease := &testLease{id: uuid.NewString(), expiresAt: s.now().Add(ttl)}
	s.held[name] = lease
	s.leases[lease.id] = name
	return lease.id, true
}

// Renew extends a live lease's expiry. An unknown or lapsed lease is an
// error: the holder must treat it as having lost the lock.
func (s *TestLockService) Renew(lease string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.leases[lease]
	if !ok {
		return fmt.Errorf("renew: unknown or expired lease")
	}
	cur := s.held[name]
	if cur == nil || cur.id != lease || !cur.expiresAt.After(s.now()) {
		return fmt.Errorf("renew: unknown or expired lease")
	}
	cur.expiresAt = s.now().Add(ttl)
	return nil
}

// Release frees the lock held by the lease. Releasing an unknown lease is a
// no-op: the lease may have lapsed and been taken over, in which case the
// lock is no longer this holder's to free.
func (s *TestLockService) Release(lease string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.leases[lease]
	if !ok {
		return
	}
	delete(s.leases, lease)
	if cur := s.held[name]; cur != nil && cur.id == lease {
		delete(s.held, name)
	}
}
