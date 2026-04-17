package repository

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Consumer names for the event_consumer_claim table. Each named consumer
// represents an at-most-once processing boundary. PR 8 only introduces
// the cadence_updater consumer; future PRs may add more (e.g. a
// follow-up-manager consumer in PR 9).
const (
	EventConsumerCadenceUpdater = "cadence_updater"
)

// EventConsumerClaimRepository wraps the event_consumer_claim queries
// (migration 040). The table's (event_id, consumer) primary key is
// the dedupe boundary for the inline + queued delivery paths of the
// same interaction.recorded event.
type EventConsumerClaimRepository struct {
	queries db.Querier
}

// NewEventConsumerClaimRepository builds the repository. The DB.Querier
// passed in is expected to be the same one shared across the service
// graph so caller-owned txs can use db.New(tx) to reach these methods.
func NewEventConsumerClaimRepository(queries db.Querier) *EventConsumerClaimRepository {
	return &EventConsumerClaimRepository{queries: queries}
}

// TryClaimTx attempts to claim (event_id, consumer) inside the caller's
// tx. Returns true when THIS caller successfully inserted the claim row
// (i.e. this caller should proceed to mutate state); false when the row
// already existed (i.e. another path — inline or queued — already
// processed this event, so the caller must no-op).
//
// tx must be non-nil: the claim row MUST share the caller's mutation tx
// so the claim and the state change commit atomically. A pre-commit
// crash rolls back both the claim and the mutation together; a
// post-commit re-delivery then sees the surviving claim and no-ops
// correctly.
func (r *EventConsumerClaimRepository) TryClaimTx(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, consumer string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("event_consumer_claim: nil tx")
	}
	if consumer == "" {
		return false, fmt.Errorf("event_consumer_claim: empty consumer name")
	}
	rows, err := db.New(tx).InsertEventConsumerClaim(ctx, db.InsertEventConsumerClaimParams{
		EventID:  uuidToPgUUID(eventID),
		Consumer: consumer,
	})
	if err != nil {
		return false, fmt.Errorf("insert event_consumer_claim: %w", err)
	}
	return rows == 1, nil
}

// ExistsTx returns true if a claim row is present for (event_id, consumer).
// Read-only helper used by tests that want to assert the claim landed
// without attempting to re-insert.
func (r *EventConsumerClaimRepository) ExistsTx(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, consumer string) (bool, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	return q.ExistsEventConsumerClaim(ctx, db.ExistsEventConsumerClaimParams{
		EventID:  uuidToPgUUID(eventID),
		Consumer: consumer,
	})
}
