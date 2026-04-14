// Package service provides business-logic wrappers around repositories.
// IngestService owns the single-tx batch-publish semantics for the HTTP
// ingestion endpoint introduced in PR 4 of the event-bus-foundation spec.
package service

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IngestService persists pre-validated event envelopes inside a single
// transaction. All envelopes in a batch commit together or roll back
// together — spec §3.5 "all-or-nothing on unexpected errors."
//
// Duplicates (ON CONFLICT hits on the (source, source_id) partial unique
// index) are silent no-ops inside Bus.PublishTx and do NOT abort the tx;
// they're counted separately via the env.ID-sentinel pattern (see
// IngestBatch docs).
type IngestService struct {
	database *db.Database
	bus      *events.Bus
}

// NewIngestService builds an IngestService. database.Pool is used to open
// the batch transaction; bus.PublishTx does the per-envelope insert.
func NewIngestService(database *db.Database, bus *events.Bus) *IngestService {
	return &IngestService{database: database, bus: bus}
}

// IngestBatch persists envs in one pgx transaction. Returns (accepted,
// duplicate, err):
//
//   - accepted: number of envelopes whose INSERT produced a fresh row.
//   - duplicate: number of envelopes whose INSERT hit the (source,
//     source_id) unique index and was silently dropped by the ON CONFLICT
//     DO NOTHING clause.
//   - err: any unexpected failure (begin-tx, publish-tx mid-batch, commit).
//     The whole tx rolls back in this case; both counters are zero on
//     error return.
//
// The empty-slice case is handled without opening a transaction (trivial
// optimization).
//
// Duplicate detection relies on Bus.PublishTx returning nil for both
// happy-path inserts and ON CONFLICT DO NOTHING hits. The repository
// populates env.ID only on a real RETURNING row, so env.ID == uuid.Nil is
// the duplicate sentinel. See PR 2 plan Design Decision 2 for the
// documented contract this depends on.
func (s *IngestService) IngestBatch(ctx context.Context, envs []*events.Envelope) (accepted, duplicate int, err error) {
	if len(envs) == 0 {
		return 0, 0, nil
	}

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback on any non-commit return. Safe to call after Commit: pgx
	// returns ErrTxClosed which we ignore. If rollback itself fails and no
	// prior error has been recorded, surface it.
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) && err == nil {
			err = fmt.Errorf("rollback: %w", rollbackErr)
		}
	}()

	for i, env := range envs {
		if pubErr := s.bus.PublishTx(ctx, tx, env); pubErr != nil {
			return 0, 0, fmt.Errorf("publish event index %d: %w", i, pubErr)
		}
		// env.ID is populated iff the INSERT produced a row. On ON CONFLICT
		// DO NOTHING (dup (source, source_id)) the repository returns nil
		// without mutating env.ID, leaving it at uuid.Nil.
		if env.ID == uuid.Nil {
			duplicate++
		} else {
			accepted++
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return 0, 0, fmt.Errorf("commit: %w", commitErr)
	}
	return accepted, duplicate, nil
}
