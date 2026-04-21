package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FollowUpShadowObservation models a row in
// event_shadow_followup_observation. One write attempt by one of the
// two shadow paths (direct or consumer) for a given interaction.recorded
// event. EventID is required — the table's FK to event(id) enforces it.
type FollowUpShadowObservation struct {
	ID         uuid.UUID
	EventID    uuid.UUID
	Writer     string
	ContactID  uuid.UUID
	Source     string
	Direction  string
	OccurredAt time.Time

	// Action the writer did (direct) or would do (consumer). One of the
	// FollowUpAction* constants.
	Action string

	// SkipReason is populated on outbound skip actions that fire one of
	// the three guards. Empty string when absent; Record* helpers coerce
	// to NULL for the DB write.
	SkipReason string

	// WouldIdempotencyKey is the local idempotency key the writer would
	// use for a create action in cutover. Nil for non-create actions.
	WouldIdempotencyKey *string

	// WouldDeadline is the deadline date the writer did set (direct) or
	// would set (consumer) for create / refresh actions. Nil for skip /
	// complete actions.
	WouldDeadline *time.Time

	// DirectContactTaskID is set only on direct-path rows that touched a
	// concrete contact_task row. Nil on consumer rows and on direct rows
	// that skipped or hit a remote error.
	DirectContactTaskID *uuid.UUID

	// ConsumerCalledTodoist is the consumer's self-reported Todoist call
	// flag. Must be false in shadow mode; the post-bake assertion checks
	// that this stays false across all consumer rows.
	ConsumerCalledTodoist bool

	ObservedAt time.Time
}

// FollowUpShadowDivergence is a row-shape result of FindDivergences.
// Either the direct or consumer side may be absent (FULL OUTER JOIN) —
// test callers inspect DirectAction / ConsumerAction for presence.
type FollowUpShadowDivergence struct {
	EventID   uuid.UUID
	ContactID uuid.UUID

	DirectAction   *string
	ConsumerAction *string

	DirectSkipReason   *string
	ConsumerSkipReason *string

	DirectWouldIdempotencyKey   *string
	ConsumerWouldIdempotencyKey *string

	DirectWouldDeadline   *time.Time
	ConsumerWouldDeadline *time.Time

	DirectContactTaskID *uuid.UUID

	ConsumerCalledTodoist *bool
}

// Writer, action, and skip-reason constants mirror the CHECK constraints
// on event_shadow_followup_observation (migration 042).
const (
	FollowUpShadowWriterDirect   = "direct"
	FollowUpShadowWriterConsumer = "consumer"

	FollowUpActionCreate   = "create"
	FollowUpActionRefresh  = "refresh"
	FollowUpActionComplete = "complete"
	FollowUpActionSkip     = "skip"

	FollowUpSkipReasonBackdated        = "backdated"
	FollowUpSkipReasonOutOfOrder       = "out_of_order"
	FollowUpSkipReasonDuplicatePending = "duplicate_pending"
)

// FollowUpShadowObservationRepository persists follow-up shadow
// observations. Parallel in shape to CadenceShadowObservationRepository.
type FollowUpShadowObservationRepository struct {
	queries db.Querier
	pool    *pgxpool.Pool
}

// NewFollowUpShadowObservationRepository builds the repo. pool enables
// the direct-path post-commit closure to open a short-lived own-tx when
// tx is nil; passing nil is permitted but RecordDirect with tx==nil
// will then return an error.
func NewFollowUpShadowObservationRepository(queries db.Querier, pool *pgxpool.Pool) *FollowUpShadowObservationRepository {
	return &FollowUpShadowObservationRepository{queries: queries, pool: pool}
}

// RecordDirect inserts an observation stamped writer="direct". Opens a
// short-lived own-tx when tx is nil so the direct-path post-commit
// closure (whose outer tx has already committed) can still write
// without borrowing a caller tx.
func (r *FollowUpShadowObservationRepository) RecordDirect(ctx context.Context, tx pgx.Tx, obs FollowUpShadowObservation) error {
	obs.Writer = FollowUpShadowWriterDirect
	return r.insertObservation(ctx, tx, obs)
}

// RecordConsumer inserts an observation stamped writer="consumer".
// Callers normally pass the worker tx so the observation commits
// atomically with the worker's unit of work.
func (r *FollowUpShadowObservationRepository) RecordConsumer(ctx context.Context, tx pgx.Tx, obs FollowUpShadowObservation) error {
	obs.Writer = FollowUpShadowWriterConsumer
	return r.insertObservation(ctx, tx, obs)
}

func (r *FollowUpShadowObservationRepository) insertObservation(ctx context.Context, tx pgx.Tx, obs FollowUpShadowObservation) error {
	var skipReason pgtype.Text
	if obs.SkipReason != "" {
		skipReason = pgtype.Text{String: obs.SkipReason, Valid: true}
	}
	params := db.InsertFollowUpShadowObservationParams{
		EventID:               uuidToPgUUID(obs.EventID),
		Writer:                obs.Writer,
		ContactID:             uuidToPgUUID(obs.ContactID),
		Source:                obs.Source,
		Direction:             obs.Direction,
		OccurredAt:            pgtype.Timestamptz{Time: obs.OccurredAt, Valid: true},
		Action:                obs.Action,
		SkipReason:            skipReason,
		WouldIdempotencyKey:   stringToPgText(obs.WouldIdempotencyKey),
		WouldDeadline:         timeToPgDate(obs.WouldDeadline),
		DirectContactTaskID:   uuidPtrToPgUUID(obs.DirectContactTaskID),
		ConsumerCalledTodoist: obs.ConsumerCalledTodoist,
	}

	if tx != nil {
		_, err := db.New(tx).InsertFollowUpShadowObservation(ctx, params)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert follow-up shadow observation (tx): %w", err)
		}
		// ON CONFLICT DO NOTHING returns ErrNoRows on duplicate — idempotent success.
		return nil
	}

	if r.pool == nil {
		return errors.New("follow-up shadow observation: nil tx and nil pool")
	}
	ownTx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin follow-up shadow observation tx: %w", err)
	}
	defer func() {
		if rbErr := ownTx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	if _, err := db.New(ownTx).InsertFollowUpShadowObservation(ctx, params); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert follow-up shadow observation: %w", err)
	}
	if err := ownTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit follow-up shadow observation: %w", err)
	}
	return nil
}

// FindMatchingDirect returns the direct-path observation for the given
// event_id, if present. Returns (nil, nil) when absent — the direct-path
// closure may not have landed yet when the consumer runs.
func (r *FollowUpShadowObservationRepository) FindMatchingDirect(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*FollowUpShadowObservation, error) {
	return r.findByWriter(ctx, tx, eventID, FollowUpShadowWriterDirect)
}

// FindMatchingConsumer returns the consumer-path observation for the
// given event_id, if present.
func (r *FollowUpShadowObservationRepository) FindMatchingConsumer(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*FollowUpShadowObservation, error) {
	return r.findByWriter(ctx, tx, eventID, FollowUpShadowWriterConsumer)
}

func (r *FollowUpShadowObservationRepository) findByWriter(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, writer string) (*FollowUpShadowObservation, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	row, err := q.FindFollowUpShadowObservationByEventAndWriter(ctx, db.FindFollowUpShadowObservationByEventAndWriterParams{
		EventID: uuidToPgUUID(eventID),
		Writer:  writer,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find %s follow-up shadow observation: %w", writer, err)
	}
	return convertDbFollowUpShadowObservation(row), nil
}

// CountByWriter returns the total row count for a writer. Integration
// tests prefer CountByContact to avoid cross-test pollution.
func (r *FollowUpShadowObservationRepository) CountByWriter(ctx context.Context, writer string) (int64, error) {
	return r.queries.CountFollowUpShadowObservationsByWriter(ctx, writer)
}

// CountByContact returns the total row count for a contact.
func (r *FollowUpShadowObservationRepository) CountByContact(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.CountFollowUpShadowObservationsByContact(ctx, uuidToPgUUID(contactID))
}

// FindDivergences returns rows from the FULL OUTER JOIN of direct vs
// consumer observations over [from, to). Expected-divergence classes
// (guard 1 backdated, guard 2 out-of-order, external-only consumer
// rows) are filtered at the report layer, not in this query.
func (r *FollowUpShadowObservationRepository) FindDivergences(ctx context.Context, from, to time.Time) ([]FollowUpShadowDivergence, error) {
	rows, err := r.queries.FindFollowUpShadowDivergences(ctx, db.FindFollowUpShadowDivergencesParams{
		ObservedAtFrom: pgtype.Timestamptz{Time: from, Valid: true},
		ObservedAtTo:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("find follow-up shadow divergences: %w", err)
	}
	out := make([]FollowUpShadowDivergence, 0, len(rows))
	for _, row := range rows {
		div := FollowUpShadowDivergence{
			EventID:                     uuid.UUID(row.EventID.Bytes),
			ContactID:                   uuid.UUID(row.ContactID.Bytes),
			DirectAction:                pgTextToStrPtr(row.DirectAction),
			ConsumerAction:              pgTextToStrPtr(row.ConsumerAction),
			DirectSkipReason:            pgTextToStrPtr(row.DirectSkipReason),
			ConsumerSkipReason:          pgTextToStrPtr(row.ConsumerSkipReason),
			DirectWouldIdempotencyKey:   pgTextToStrPtr(row.DirectWouldIdempotencyKey),
			ConsumerWouldIdempotencyKey: pgTextToStrPtr(row.ConsumerWouldIdempotencyKey),
			DirectWouldDeadline:         pgDateToTimePtr(row.DirectWouldDeadline),
			ConsumerWouldDeadline:       pgDateToTimePtr(row.ConsumerWouldDeadline),
		}
		if row.DirectContactTaskID.Valid {
			id := uuid.UUID(row.DirectContactTaskID.Bytes)
			div.DirectContactTaskID = &id
		}
		if row.ConsumerCalledTodoist.Valid {
			b := row.ConsumerCalledTodoist.Bool
			div.ConsumerCalledTodoist = &b
		}
		out = append(out, div)
	}
	return out, nil
}

func convertDbFollowUpShadowObservation(row *db.EventShadowFollowupObservation) *FollowUpShadowObservation {
	obs := &FollowUpShadowObservation{
		Writer:                row.Writer,
		Source:                row.Source,
		Direction:             row.Direction,
		Action:                row.Action,
		ConsumerCalledTodoist: row.ConsumerCalledTodoist,
	}
	if row.ID.Valid {
		obs.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.EventID.Valid {
		obs.EventID = uuid.UUID(row.EventID.Bytes)
	}
	if row.ContactID.Valid {
		obs.ContactID = uuid.UUID(row.ContactID.Bytes)
	}
	if row.OccurredAt.Valid {
		obs.OccurredAt = row.OccurredAt.Time.UTC()
	}
	if row.SkipReason.Valid {
		obs.SkipReason = row.SkipReason.String
	}
	if key := pgTextToStrPtr(row.WouldIdempotencyKey); key != nil {
		obs.WouldIdempotencyKey = key
	}
	obs.WouldDeadline = pgDateToTimePtr(row.WouldDeadline)
	if row.DirectContactTaskID.Valid {
		id := uuid.UUID(row.DirectContactTaskID.Bytes)
		obs.DirectContactTaskID = &id
	}
	if row.ObservedAt.Valid {
		obs.ObservedAt = row.ObservedAt.Time.UTC()
	}
	return obs
}
