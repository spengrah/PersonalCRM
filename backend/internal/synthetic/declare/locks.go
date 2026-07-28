package declare

import (
	"context"
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

// tryLock takes a non-blocking session lock.
func (s *lockSession) tryLock(ctx context.Context, key int64) (bool, error) {
	ok, err := s.repo.TryAdvisoryLock(ctx, key)
	if err != nil {
		return false, err
	}
	if ok {
		s.held[key] = true
	}
	return ok, nil
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

// unlockAll releases keys under a fresh short context. Failures mark the
// session broken so close() destroys the connection.
func (s *lockSession) unlockAll(keys []int64) {
	for _, key := range keys {
		s.unlock(key)
	}
}

func (s *lockSession) unlock(key int64) {
	if !s.held[key] {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockOpBudget)
	defer cancel()
	if _, err := s.repo.AdvisoryUnlock(ctx, key); err != nil {
		// The lock may or may not still be held; the connection is the only
		// thing that can settle it, so retire the connection instead of
		// pretending the unlock succeeded.
		s.broken = true
	}
	delete(s.held, key)
}

// close releases every remaining lock and returns the connection to the pool —
// or destroys it when an unlock failed, so the session (and any lock it still
// holds) dies with it.
func (s *lockSession) close() {
	remaining := make([]int64, 0, len(s.held))
	for key := range s.held {
		remaining = append(remaining, key)
	}
	s.unlockAll(remaining)
	if s.broken {
		ctx, cancel := context.WithTimeout(context.Background(), lockOpBudget)
		_ = s.conn.Conn().Close(ctx)
		cancel()
	}
	s.conn.Release()
}
