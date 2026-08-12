package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	env.ID = row.ID
	if row.SourceID != nil {
		env.SourceID = *row.SourceID
	}
	env.ObservedAt = row.ObservedAt.UTC()
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
	var idParam *uuid.UUID
	if env.ID != uuid.Nil {
		idParam = &env.ID
	}
	var sourceIDParam *string
	if env.SourceID != "" {
		sourceIDParam = &env.SourceID
	}

	params := db.InsertEventParams{
		ID:         idParam,
		Source:     env.Source,
		SourceID:   sourceIDParam,
		Kind:       string(env.Kind),
		Payload:    []byte(env.Payload),
		ObservedAt: env.ObservedAt,
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
	row, err := r.queries.GetEvent(ctx, id)
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
		SourceID: &sourceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbEvent(row), nil
}

// HardDeleteEventsBySourceAndSourceIDPrefix removes event rows whose
// (source, source_id) match the given source and a LIKE-prefix on
// source_id. Test-only helper for integration tests that publish
// per-run event envelopes — a soft-delete is not possible because the
// event log is append-only and the (source, source_id) partial unique
// must be cleared between test runs. Production code MUST NOT call this.
func (r *EventRepository) HardDeleteEventsBySourceAndSourceIDPrefix(ctx context.Context, source, sourceIDPrefix string) error {
	return r.queries.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, db.HardDeleteEventsBySourceAndSourceIDPrefixParams{
		Source:         source,
		SourceIDPrefix: &sourceIDPrefix,
	})
}

// CountRematchDispatcherJobs returns the number of river_job rows of
// kind "rematch_dispatcher" whose args JSON matches the given
// (contactID, rematchJobID) tuple. Test-only helper for rematch dedup
// integration tests — avoids inlining raw SQL into Go test code.
//
// The underlying sqlc query lives in queries/event.sql because
// river_job is interrogated alongside the event-bus tables and has no
// dedicated sqlc module.
func (r *EventRepository) CountRematchDispatcherJobs(ctx context.Context, contactID, rematchJobID uuid.UUID) (int64, error) {
	return r.queries.CountRematchDispatcherJobsByContactAndJob(ctx, db.CountRematchDispatcherJobsByContactAndJobParams{
		ContactID:    contactID.String(),
		RematchJobID: rematchJobID.String(),
	})
}

// CountRematchDispatcherJobsByContact returns the number of
// rematch_dispatcher river_job rows enqueued for a contact (any
// rematch_job_id). Test-only helper for the address-book reconcile
// integration test — proves the matched-row auto-propagate published
// contact_methods.added without needing the jobID.
func (r *EventRepository) CountRematchDispatcherJobsByContact(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.CountRematchDispatcherJobsByContact(ctx, contactID.String())
}
