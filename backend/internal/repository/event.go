package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// EventRepository persists raw events for the event-bus pipeline (spec §3.1).
// Append-only: InsertEvent is the only write path.
type EventRepository struct {
	queries db.Querier
}

// NewEventRepository builds an EventRepository over a sqlc Querier. Pass
// database.Queries at startup.
func NewEventRepository(queries db.Querier) *EventRepository {
	return &EventRepository{queries: queries}
}

// convertDbEvent maps a sqlc-generated db.Event into the events.Envelope
// shape used by the rest of the system. Caller guarantees row != nil.
func convertDbEvent(row *db.Event) *events.Envelope {
	env := &events.Envelope{
		Source:  row.Source,
		Kind:    events.Kind(row.Kind),
		Payload: row.Payload, // []byte from sqlc — json.RawMessage compatible
	}
	if row.ID.Valid {
		env.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.SourceID.Valid {
		env.SourceID = row.SourceID.String
	}
	if row.ObservedAt.Valid {
		env.ObservedAt = row.ObservedAt.Time.UTC()
	}
	return env
}

// InsertEvent inserts an event within the caller's transaction. On
// (source, source_id) conflict (when source_id is non-empty), returns
// db.ErrDuplicate — the Bus treats this as idempotent no-op.
//
// On success, env.ID is populated with the DB-generated UUID (or the
// caller-provided UUID if env.ID was non-zero on entry).
func (r *EventRepository) InsertEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if tx == nil {
		return errors.New("insert event: nil tx")
	}
	if env == nil {
		return errors.New("insert event: nil envelope")
	}

	// Build params. source_id → NULL when empty string. id → NULL (let DB
	// default via gen_random_uuid) when env.ID is uuid.Nil.
	var idParam pgtype.UUID
	if env.ID != uuid.Nil {
		idParam = pgtype.UUID{Bytes: env.ID, Valid: true}
	}
	var sourceIDParam pgtype.Text
	if env.SourceID != "" {
		sourceIDParam = pgtype.Text{String: env.SourceID, Valid: true}
	}

	params := db.InsertEventParams{
		ID:         idParam,
		Source:     env.Source,
		SourceID:   sourceIDParam,
		Kind:       string(env.Kind),
		Payload:    []byte(env.Payload),
		ObservedAt: pgtype.Timestamptz{Time: env.ObservedAt, Valid: true},
	}

	// Build a tx-scoped Querier so the INSERT runs inside the caller's tx.
	txQueries := db.New(tx)
	row, err := txQueries.InsertEvent(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING returned zero rows → publisher dedup hit.
			return db.ErrDuplicate
		}
		return err
	}

	inserted := convertDbEvent(row)
	env.ID = inserted.ID
	return nil
}

// GetEvent fetches a single event by id. Returns db.ErrNotFound if absent.
func (r *EventRepository) GetEvent(ctx context.Context, id uuid.UUID) (*events.Envelope, error) {
	row, err := r.queries.GetEvent(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbEvent(row), nil
}

// CountBySource returns the number of events for a source. Used by
// integration tests to assert ingest outcomes (and could power ops
// dashboards later). The event log is append-only — no soft-delete
// filter needed.
func (r *EventRepository) CountBySource(ctx context.Context, source string) (int64, error) {
	return r.queries.CountEventsBySource(ctx, source)
}

// FindEventBySource returns the event matching (source, source_id). Returns
// db.ErrNotFound if absent OR if sourceID is empty. Used by batch ingestion
// pre-filtering and tests.
//
// NULL source_id events are never returned by this method (they aren't
// deduped and so can't be looked up by a stable key). Callers looking for
// a specific NULL-source_id event must use GetEvent by id.
func (r *EventRepository) FindEventBySource(ctx context.Context, source, sourceID string) (*events.Envelope, error) {
	if sourceID == "" {
		return nil, db.ErrNotFound
	}
	row, err := r.queries.FindEventBySource(ctx, db.FindEventBySourceParams{
		Source:   source,
		SourceID: pgtype.Text{String: sourceID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbEvent(row), nil
}
