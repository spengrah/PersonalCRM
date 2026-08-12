package repository

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
)

// HealthRepository reads the river_job state behind the /health river and sync
// components. It owns no writes — the production methods are pure counts and
// max/min-timestamp aggregates the health package maps to component statuses.
type HealthRepository struct {
	queries db.Querier
}

// NewHealthRepository constructs a HealthRepository.
func NewHealthRepository(queries db.Querier) *HealthRepository {
	return &HealthRepository{queries: queries}
}

// CountDiscardedRiverJobs returns the number of jobs that exhausted their
// retries and landed in 'discarded'.
func (r *HealthRepository) CountDiscardedRiverJobs(ctx context.Context) (int64, error) {
	n, err := r.queries.CountDiscardedRiverJobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("count discarded river jobs: %w", err)
	}
	return n, nil
}

// OldestDueRiverJobScheduledAt returns the earliest scheduled_at among jobs
// that are due (scheduled_at <= now) but still 'available'/'retryable' — the
// worker-stall signal. Returns nil when no job is due (the MIN aggregate is
// NULL over zero rows). sqlc cannot type an untyped aggregate output, so the
// generated query returns interface{}; pgx scans a NULL MIN(timestamptz) into
// that as nil, and a non-NULL one as time.Time — the type assertion below is
// the boundary that restores the documented NULL -> nil *time.Time contract.
func (r *HealthRepository) OldestDueRiverJobScheduledAt(ctx context.Context, now time.Time) (*time.Time, error) {
	v, err := r.queries.OldestDueRiverJobScheduledAt(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("oldest due river job scheduled_at: %w", err)
	}
	ts, ok := v.(time.Time)
	if !ok {
		return nil, nil
	}
	return utcPtr(&ts), nil
}

// LatestCompletedRiverJobByKind returns the newest finalized_at among COMPLETED
// jobs of the given kind — the watchdog-liveness trail. Returns nil when no
// completed job of that kind exists (the MAX aggregate is NULL over zero
// rows). See OldestDueRiverJobScheduledAt for why the type assertion is
// needed: sqlc cannot type an untyped aggregate output.
func (r *HealthRepository) LatestCompletedRiverJobByKind(ctx context.Context, kind string) (*time.Time, error) {
	v, err := r.queries.LatestCompletedRiverJobByKind(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("latest completed river job by kind: %w", err)
	}
	ts, ok := v.(time.Time)
	if !ok {
		return nil, nil
	}
	return utcPtr(&ts), nil
}

// InsertRiverJobForTest plants one river_job with an explicit kind, state,
// scheduled_at, and (nullable) finalized_at. TEST ONLY — the /health river
// integration test uses it to drive the count/age/latest-completed queries
// against known values; production code never inserts river_job directly.
func (r *HealthRepository) InsertRiverJobForTest(ctx context.Context, kind string, state db.RiverJobState, scheduledAt time.Time, finalizedAt *time.Time) error {
	return r.queries.TestInsertRiverJobWithStateForTest(ctx, db.TestInsertRiverJobWithStateForTestParams{
		Kind:        kind,
		State:       state,
		ScheduledAt: scheduledAt,
		FinalizedAt: finalizedAt,
	})
}
