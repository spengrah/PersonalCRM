package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a mutable time source safe for concurrent readers.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newLockFixture() (*TestLockService, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return NewTestLockService(clock.now), clock
}

func TestTestLock_MutualExclusionAndRelease(t *testing.T) {
	t.Parallel()
	svc, _ := newLockFixture()
	ctx := context.Background()

	lease, err := svc.Acquire(ctx, "mac-host", time.Minute, 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := svc.Acquire(ctx, "mac-host", time.Minute, 0); !errors.Is(err, ErrLockWaitTimeout) {
		t.Fatalf("second acquire while held: want ErrLockWaitTimeout, got %v", err)
	}

	// A different name is independent.
	if _, err := svc.Acquire(ctx, "other", time.Minute, 0); err != nil {
		t.Fatalf("independent name: %v", err)
	}

	svc.Release(lease)
	if _, err := svc.Acquire(ctx, "mac-host", time.Minute, 0); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}

	// Releasing an already-released lease is a no-op.
	svc.Release(lease)
}

func TestTestLock_ExpiredLeaseIsTakenOverExactlyOnce(t *testing.T) {
	t.Parallel()
	svc, clock := newLockFixture()
	ctx := context.Background()

	stale, err := svc.Acquire(ctx, "mac-host", 30*time.Second, 0)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	clock.advance(31 * time.Second)

	// The regression the filesystem designs kept failing: N waiters racing
	// one expired lease must admit exactly one. wait=0 makes each waiter a
	// single takeover attempt (the fake clock never advances mid-test).
	const waiters = 10
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Acquire(ctx, "mac-host", time.Minute, 0); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("takeover winners: want exactly 1, got %d", got)
	}

	// The lapsed holder's lease is dead: renew fails, release is a no-op
	// that does not free the new holder's lock.
	if err := svc.Renew(stale, time.Minute); err == nil {
		t.Fatal("renew of lapsed lease should fail")
	}
	svc.Release(stale)
	if _, err := svc.Acquire(ctx, "mac-host", time.Minute, 0); !errors.Is(err, ErrLockWaitTimeout) {
		t.Fatalf("lock should still be held by the takeover winner, got %v", err)
	}
}

func TestTestLock_RenewExtendsExpiry(t *testing.T) {
	t.Parallel()
	svc, clock := newLockFixture()
	ctx := context.Background()

	lease, err := svc.Acquire(ctx, "mac-host", 30*time.Second, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	clock.advance(20 * time.Second)
	if err := svc.Renew(lease, 30*time.Second); err != nil {
		t.Fatalf("renew: %v", err)
	}
	clock.advance(20 * time.Second)

	// 40s elapsed since acquire, but only 20s since renew: still held.
	if _, err := svc.Acquire(ctx, "mac-host", time.Minute, 0); !errors.Is(err, ErrLockWaitTimeout) {
		t.Fatalf("lock should still be held after renew, got %v", err)
	}
}

func TestTestLock_AcquireWaitsForRelease(t *testing.T) {
	t.Parallel()
	svc, _ := newLockFixture()
	ctx := context.Background()

	lease, err := svc.Acquire(ctx, "mac-host", time.Minute, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Acquire(ctx, "mac-host", time.Minute, 5*time.Second)
		done <- err
	}()

	// Give the waiter a moment to start polling, then free the lock.
	time.Sleep(120 * time.Millisecond)
	svc.Release(lease)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter should acquire after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire after release")
	}
}
