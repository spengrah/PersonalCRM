package declare

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryKey hashes a scoped token into a PostgreSQL advisory-lock key.
//
// FNV-1a 64-bit, computed in Go, converted to bigint as the PRESERVED
// two's-complement bit pattern. No collision-resistance claim is made and none
// is needed: the key space is a uniform 64 bits, the callers are cooperative
// test processes, and a collision's worst case is a spurious conflict or a
// needless serialization — never corruption. PostgreSQL's own 32-bit hashtext
// is deliberately NOT used: birthday collisions at 2^32 are plausible at test
// volume. The scoping prefixes ("declare:", "declare-band:...") keep the key
// domain away from any other advisory-lock user.
func advisoryKey(token string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(token))
	return int64(h.Sum64())
}

// lockSession owns a DEDICATED pooled connection and the session advisory locks
// taken on it. Session locks belong to the connection that took them, so every
// lock/unlock in one session must run on the same connection — which is why
// this cannot go through the pool-wide Queries.
//
// Unlock hygiene is binding: close() releases everything held under a FRESH
// short context (never the run's, which may already be dead), and if an unlock
// fails it destroys the underlying connection so the session — and with it the
// lock — dies anyway. A stranded session lock would otherwise serialize that
// namespace for the lifetime of the process.
type lockSession struct {
	conn   *pgxpool.Conn
	repo   *repository.SyntheticSupportRepository
	held   map[int64]bool
	broken bool
	closed bool
}

func newLockSession(ctx context.Context, database *db.Database) (*lockSession, error) {
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("declare: acquire dedicated lock connection: %w", err)
	}
	return &lockSession{
		conn: conn,
		repo: repository.NewSyntheticSupportRepository(db.New(conn)),
		held: map[int64]bool{},
	}, nil
}

// tryLock takes a non-blocking session lock. A key this session already holds
// is reported held without re-acquiring: session locks are COUNTED, so a second
// acquire would need a second release, and the mismatch would leave a lock alive
// on a connection returned to the pool.
func (s *lockSession) tryLock(ctx context.Context, key int64) (bool, error) {
	if s.held[key] {
		return true, nil
	}
	ok, err := s.repo.TryAdvisoryLock(ctx, key)
	if err != nil {
		return false, err
	}
	if ok {
		s.held[key] = true
	}
	return ok, nil
}

// tryLockAll takes every key non-blockingly, in the given order, and releases
// what it got if any one is unavailable — so a refused claim never leaves a
// partial hold behind. Reports false when a key was held elsewhere; the error is
// reserved for a genuine lock/unlock failure.
func (s *lockSession) tryLockAll(ctx context.Context, keys []int64) (bool, error) {
	var taken []int64
	for _, key := range keys {
		ok, err := s.tryLock(ctx, key)
		if err != nil {
			return false, errors.Join(err, s.unlockAll(taken))
		}
		if !ok {
			return false, s.unlockAll(taken)
		}
		taken = append(taken, key)
	}
	return true, nil
}

// lockAll takes blocking session locks for keys, in the given (sorted) order.
// Blocking is correct here: contention means two DIFFERENT namespaces mapped
// into the same numeric band, which is legitimate and rare, and serializing
// them is the point. It is bounded by ctx.
func (s *lockSession) lockAll(ctx context.Context, keys []int64) error {
	for _, key := range keys {
		if s.held[key] {
			continue
		}
		if err := s.repo.AdvisoryLock(ctx, key); err != nil {
			return err
		}
		s.held[key] = true
	}
	return nil
}

// unlockAll releases keys under a fresh short context and returns the FIRST
// failure. Every key is attempted regardless, so one bad release cannot strand
// the others, and any failure marks the session broken so close() destroys the
// connection.
//
// The returned error is not advisory: a caller that goes on to BLOCK on another
// lock after a failed release is holding-and-waiting, which is the deadlock
// shape the release-before-acquire discipline exists to prevent. Callers abort.
func (s *lockSession) unlockAll(keys []int64) error {
	var firstErr error
	for _, key := range keys {
		if err := s.unlock(key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// unlock releases one session lock. It treats BOTH failure modes as failure:
// an error, and a false return — pg_advisory_unlock reports false when the
// session did not hold the lock, which means the caller's model of what it
// holds is wrong and the lock may still be held by someone. Either way the only
// thing that can settle it is the connection, so the session is marked broken
// and the key is dropped from the held set (close() destroys the connection,
// which releases anything still held on it).
func (s *lockSession) unlock(key int64) error {
	if !s.held[key] {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockOpBudget)
	defer cancel()
	released, err := s.unlockOnce(ctx, key)
	delete(s.held, key)
	switch {
	case err != nil:
		s.broken = true
		return fmt.Errorf("declare: release advisory lock %d: %w", key, err)
	case !released:
		s.broken = true
		return fmt.Errorf("declare: advisory lock %d reported not held on release", key)
	}
	return nil
}

// unlockOnce is the single unlock call site, so the injected-failure seam has
// exactly one place to intercept.
func (s *lockSession) unlockOnce(ctx context.Context, key int64) (bool, error) {
	if err := unlockFailpointError(); err != nil {
		return false, err
	}
	return s.repo.AdvisoryUnlock(ctx, key)
}

// close releases every remaining lock and returns the connection to the pool —
// or destroys it when an unlock failed, so the session (and any lock it still
// holds) dies with it. Idempotent: a caller that closed early on an abort path
// leaves the deferred close a no-op.
func (s *lockSession) close() {
	if s.closed {
		return
	}
	s.closed = true
	remaining := make([]int64, 0, len(s.held))
	for key := range s.held {
		remaining = append(remaining, key)
	}
	_ = s.unlockAll(remaining)
	if s.broken {
		ctx, cancel := context.WithTimeout(context.Background(), lockOpBudget)
		_ = s.conn.Conn().Close(ctx)
		cancel()
	}
	s.conn.Release()
}
