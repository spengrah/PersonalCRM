package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InteractionObservation is what the observation step reads: one contact's
// part in a source object as a sync, ingest or manual path records it.
// Existing is the row the observation lands on when it extends, promotes or
// re-links one; nil when it creates a row.
type InteractionObservation struct {
	ContactID  uuid.UUID
	Source     string
	SourceRef  *string
	Direction  string
	OccurredAt time.Time
	Existing   *Interaction
}

// ObserveInteractionTx is the observation step (arc seam S3): the one place an
// observation of a correctable source becomes an interaction row that carries
// its interaction node. Every create, extend, promote-to-mutual and re-link
// runs it inside the write's transaction, before any cadence or follow-up
// effect and before any interaction.recorded publish. It returns the
// observation's interaction node, or nil for a message-burst source.
//
// The node is keyed by the per-source identity rule that migration 087 defines
// once in SQL (resolve_interaction_node); this step holds no key derivation. A
// row that already has a node keeps it. A live row that has none (a row
// stored before migration 087's backfill) gets the node assigned here, once.
//
// Before it returns, the step takes the correction lock on the node, so the
// write and its verdict read (ObservationVerdictTx) are serialized with any
// correction of that node: a concurrent correction either commits first or
// waits for this write's transaction.
func (r *InteractionRepository) ObserveInteractionTx(ctx context.Context, tx pgx.Tx, obs InteractionObservation) (*uuid.UUID, error) {
	q := db.New(tx)
	nodeID, err := observeInteractionNode(ctx, q, obs)
	if err != nil || nodeID == nil {
		return nil, err
	}
	if err := acquireCorrectionLocks(ctx, q, []uuid.UUID{*nodeID}); err != nil {
		return nil, err
	}
	return nodeID, nil
}

// observeInteractionNode resolves the observation's interaction node, assigning
// it to an existing row that has none; nil for a message-burst source.
func observeInteractionNode(ctx context.Context, q *db.Queries, obs InteractionObservation) (*uuid.UUID, error) {
	source, sourceRef := obs.Source, obs.SourceRef
	if obs.Existing != nil {
		if obs.Existing.InteractionNodeID != nil {
			return obs.Existing.InteractionNodeID, nil
		}
		source, sourceRef = obs.Existing.Source, obs.Existing.SourceRef
	}
	nodeID, err := resolveInteractionNode(ctx, q, source, sourceRef)
	if err != nil {
		return nil, err
	}
	if obs.Existing != nil && nodeID != nil {
		if err := q.AssignInteractionNode(ctx, db.AssignInteractionNodeParams{
			ID:                obs.Existing.ID,
			InteractionNodeID: nodeID,
		}); err != nil {
			return nil, fmt.Errorf("assign interaction node: %w", err)
		}
	}
	return nodeID, nil
}

// resolveInteractionNode finds or creates the interaction node of an
// observation; nil for a message-burst source, which has none.
func resolveInteractionNode(ctx context.Context, q db.Querier, source string, sourceRef *string) (*uuid.UUID, error) {
	nodeID, err := q.ResolveInteractionNode(ctx, db.ResolveInteractionNodeParams{Source: source, SourceRef: sourceRef})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve interaction node: %w", err)
	}
	return &nodeID, nil
}
