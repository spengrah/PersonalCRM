package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RelationshipSignal is a stored per-node scalar keyed by
// (subject_node_id, signal_key). signal_key is free text in SP1 (no catalog);
// 'closeness'|'real_cadence_days'|'trend' are the expected keys. A disposable
// projection: no envelope, no deleted_at.
type RelationshipSignal struct {
	SubjectNodeID uuid.UUID `json:"subject_node_id"`
	SignalKey     string    `json:"signal_key"`
	Value         float64   `json:"value"`
	ComputedAt    time.Time `json:"computed_at"`
	AsOf          time.Time `json:"as_of"`
	MethodVersion string    `json:"method_version"`
}

// UpsertRelationshipSignalRequest is the input for storing a signal.
type UpsertRelationshipSignalRequest struct {
	SubjectNodeID uuid.UUID
	SignalKey     string
	Value         float64
	AsOf          time.Time
	MethodVersion string
}

// RelationshipSignalRepository handles relationship-signal persistence.
type RelationshipSignalRepository struct {
	queries db.Querier
}

// NewRelationshipSignalRepository creates a new RelationshipSignalRepository.
func NewRelationshipSignalRepository(queries db.Querier) *RelationshipSignalRepository {
	return &RelationshipSignalRepository{queries: queries}
}

func convertDbRelationshipSignal(dbSignal *db.RelationshipSignal) RelationshipSignal {
	signal := RelationshipSignal{
		SignalKey:     dbSignal.SignalKey,
		Value:         dbSignal.Value,
		MethodVersion: dbSignal.MethodVersion,
	}
	if dbSignal.SubjectNodeID.Valid {
		signal.SubjectNodeID = uuid.UUID(dbSignal.SubjectNodeID.Bytes)
	}
	if dbSignal.ComputedAt.Valid {
		signal.ComputedAt = dbSignal.ComputedAt.Time.UTC()
	}
	if dbSignal.AsOf.Valid {
		signal.AsOf = dbSignal.AsOf.Time.UTC()
	}
	return signal
}

// UpsertRelationshipSignal idempotently stores one (subject_node_id,
// signal_key) signal; a recompute overwrites the value and watermarks.
func (r *RelationshipSignalRepository) UpsertRelationshipSignal(ctx context.Context, req UpsertRelationshipSignalRequest) error {
	return r.queries.UpsertRelationshipSignal(ctx, db.UpsertRelationshipSignalParams{
		SubjectNodeID: uuidToPgUUID(req.SubjectNodeID),
		SignalKey:     req.SignalKey,
		Value:         req.Value,
		AsOf:          timeToPgTimestamptz(&req.AsOf),
		MethodVersion: req.MethodVersion,
	})
}

// GetRelationshipSignal retrieves one signal by its composite key.
func (r *RelationshipSignalRepository) GetRelationshipSignal(ctx context.Context, subjectNodeID uuid.UUID, signalKey string) (*RelationshipSignal, error) {
	dbSignal, err := r.queries.GetRelationshipSignal(ctx, db.GetRelationshipSignalParams{
		SubjectNodeID: uuidToPgUUID(subjectNodeID),
		SignalKey:     signalKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	signal := convertDbRelationshipSignal(dbSignal)
	return &signal, nil
}

// ListSignalsForSubject returns every signal for one node, ordered by key.
func (r *RelationshipSignalRepository) ListSignalsForSubject(ctx context.Context, subjectNodeID uuid.UUID) ([]RelationshipSignal, error) {
	dbSignals, err := r.queries.ListSignalsForSubject(ctx, uuidToPgUUID(subjectNodeID))
	if err != nil {
		return nil, err
	}
	signals := make([]RelationshipSignal, 0, len(dbSignals))
	for _, dbSignal := range dbSignals {
		signals = append(signals, convertDbRelationshipSignal(dbSignal))
	}
	return signals, nil
}

// DeleteSignalsForSubject wipes every signal for one node.
func (r *RelationshipSignalRepository) DeleteSignalsForSubject(ctx context.Context, subjectNodeID uuid.UUID) error {
	return r.queries.DeleteSignalsForSubject(ctx, uuidToPgUUID(subjectNodeID))
}
