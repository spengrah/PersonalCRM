package repository

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Breach-type constants for sync_staleness_breach.breach_type. Mirror the
// CHECK constraint in migration 063; the service builds candidates with
// these values and the repository round-trips them unchanged.
const (
	BreachTypeHeartbeat = "heartbeat"
	BreachTypeSyncStale = "sync_stale"
	BreachTypePushStale = "push_stale"
	BreachTypeSyncError = "sync_error"
)

// StalenessBreach is the repository-layer view of a sync_staleness_breach
// row. It doubles as the API DTO for GET /api/v1/sync/staleness (the read
// path returns active breaches only, so ResolvedAt is omitted when nil).
type StalenessBreach struct {
	ID               uuid.UUID  `json:"id"`
	Source           string     `json:"source"`
	AccountID        string     `json:"account_id"`
	BreachType       string     `json:"breach_type"`
	StaleSince       time.Time  `json:"stale_since"`
	ThresholdSeconds int64      `json:"threshold_seconds"`
	Details          string     `json:"details"`
	DetectedAt       time.Time  `json:"detected_at"`
	LastObservedAt   time.Time  `json:"last_observed_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

// UpsertOpenBreachParams carries the inputs for opening or refreshing a
// breach. ObservedAt seeds detected_at + last_observed_at on insert and
// advances last_observed_at on the conflict-update path.
type UpsertOpenBreachParams struct {
	Source           string
	AccountID        string
	BreachType       string
	StaleSince       time.Time
	ThresholdSeconds int64
	Details          string
	ObservedAt       time.Time
}

// StalenessRepository handles sync_staleness_breach persistence.
type StalenessRepository struct {
	queries db.Querier
}

// NewStalenessRepository constructs a StalenessRepository.
func NewStalenessRepository(queries db.Querier) *StalenessRepository {
	return &StalenessRepository{queries: queries}
}

func convertDbStalenessBreach(row *db.SyncStalenessBreach) StalenessBreach {
	b := StalenessBreach{
		Source:           row.Source,
		AccountID:        row.AccountID,
		BreachType:       row.BreachType,
		ThresholdSeconds: row.ThresholdSeconds,
		Details:          row.Details,
	}
	if row.ID.Valid {
		b.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.StaleSince.Valid {
		b.StaleSince = row.StaleSince.Time.UTC()
	}
	if row.DetectedAt.Valid {
		b.DetectedAt = row.DetectedAt.Time.UTC()
	}
	if row.LastObservedAt.Valid {
		b.LastObservedAt = row.LastObservedAt.Time.UTC()
	}
	b.ResolvedAt = pgTimestamptzToTimePtr(row.ResolvedAt)
	return b
}

// UpsertOpenBreach opens a breach for (source, account_id, breach_type) or
// refreshes the existing open row. Returns the resulting row: a freshly
// opened breach has DetectedAt == LastObservedAt, which the service uses to
// distinguish "new" from "still breaching".
func (r *StalenessRepository) UpsertOpenBreach(ctx context.Context, params UpsertOpenBreachParams) (StalenessBreach, error) {
	row, err := r.queries.UpsertOpenStalenessBreach(ctx, db.UpsertOpenStalenessBreachParams{
		Source:           params.Source,
		AccountID:        params.AccountID,
		BreachType:       params.BreachType,
		StaleSince:       pgtype.Timestamptz{Time: params.StaleSince, Valid: true},
		ThresholdSeconds: params.ThresholdSeconds,
		Details:          params.Details,
		ObservedAt:       pgtype.Timestamptz{Time: params.ObservedAt, Valid: true},
	})
	if err != nil {
		return StalenessBreach{}, fmt.Errorf("upsert open staleness breach: %w", err)
	}
	return convertDbStalenessBreach(row), nil
}

// ListOpenBreaches returns all currently-open breaches, ordered
// deterministically (detected_at ASC, id ASC). Backs both the watchdog's
// reconcile diff and the read endpoint.
func (r *StalenessRepository) ListOpenBreaches(ctx context.Context) ([]StalenessBreach, error) {
	rows, err := r.queries.ListOpenStalenessBreaches(ctx)
	if err != nil {
		return nil, fmt.Errorf("list open staleness breaches: %w", err)
	}
	out := make([]StalenessBreach, len(rows))
	for i, row := range rows {
		out[i] = convertDbStalenessBreach(row)
	}
	return out, nil
}

// ResolveBreach marks one open breach resolved. Returns the number of rows
// affected (0 when the breach was already resolved by a concurrent tick).
func (r *StalenessRepository) ResolveBreach(ctx context.Context, id uuid.UUID, resolvedAt time.Time) (int64, error) {
	n, err := r.queries.ResolveStalenessBreach(ctx, db.ResolveStalenessBreachParams{
		ID:         uuidToPgUUID(id),
		ResolvedAt: pgtype.Timestamptz{Time: resolvedAt, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("resolve staleness breach: %w", err)
	}
	return n, nil
}

// DeleteResolvedBefore prunes resolved breaches whose resolved_at predates
// the cutoff. Open breaches are never touched.
func (r *StalenessRepository) DeleteResolvedBefore(ctx context.Context, cutoff time.Time) error {
	if err := r.queries.DeleteResolvedStalenessBreachesBefore(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true}); err != nil {
		return fmt.Errorf("delete resolved staleness breaches: %w", err)
	}
	return nil
}
