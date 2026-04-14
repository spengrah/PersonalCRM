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

// ShadowObservation models a row in the event_shadow_observation table —
// one write attempt by one of the two shadow-mode paths (direct or
// consumer). See .ai/spec/event-bus-foundation.md §3.8 and
// .ai/log/plan/event-bus-foundation-pr5-interaction-recorder-shadow.md
// Design Decision 1.
//
// Writer and Kind form a two-axis classification:
//
//   - Writer="direct"  + Kind="direct_record"  → ContactService.RecordInteraction
//     fresh-write path. This is the row that DOES have a consumer peer.
//   - Writer="direct"  + Kind="direct_extend"  → ExtendInteraction. No peer.
//   - Writer="direct"  + Kind="direct_promote" → PromoteInteractionToMutual. No peer.
//   - Writer="consumer"+ Kind=<envelope.Kind>  → consumer path. Replay=true if
//     FindBySourceRef found the direct-path row and the consumer early-returned;
//     Replay=false if the consumer was the sole writer (an expected-degradation
//     mode in shadow — see plan Risk 3).
type ShadowObservation struct {
	ID            uuid.UUID
	EventID       *uuid.UUID
	Writer        string
	Kind          string
	Source        string
	SourceRef     *string
	ContactID     uuid.UUID
	Direction     string
	OccurredAt    time.Time
	InteractionID *uuid.UUID
	Replay        bool
	ObservedAt    time.Time
}

// ShadowDivergence is a row-shape result of the FindShadowDivergences*
// queries. Any non-empty result set indicates a drift between the two
// write paths during the bake window. See plan Decision 14 Part B.
//
// SourceRef is nil for manual-kind divergences (they join on occurred_at
// only). DirectDirection / ConsumerDirection / DirectOccurredAt /
// ConsumerOccurredAt are nil when a row is present on only one side of
// the FULL OUTER JOIN.
type ShadowDivergence struct {
	Source             string
	SourceRef          *string // nil for manual kind
	ContactID          uuid.UUID
	DirectDirection    *string
	ConsumerDirection  *string
	DirectOccurredAt   *time.Time
	ConsumerOccurredAt *time.Time
}

// Writer / Kind constants. Writers are hard-coded in the CHECK
// constraint (migration 038). Kind values are convention (DB column is
// free text).
const (
	ShadowWriterDirect   = "direct"
	ShadowWriterConsumer = "consumer"

	// Direct-path synthetic kinds (no corresponding event.Kind).
	ShadowKindDirectRecord  = "direct_record"
	ShadowKindDirectExtend  = "direct_extend"
	ShadowKindDirectPromote = "direct_promote"
)

// ShadowObservationRepository persists shadow-mode observations. Methods
// accept an optional pgx.Tx so the caller can thread the insert into an
// existing transaction (consumer path — must commit with the interaction
// insert + interaction.recorded event row per spec §3.4.1) or open a
// short-lived tx on the configured pool (direct path — today's per-query
// implicit-tx semantics).
type ShadowObservationRepository struct {
	queries db.Querier
	pool    *pgxpool.Pool
}

// NewShadowObservationRepository builds the repository. Pass
// database.Queries + database.Pool — the pool is required so the
// tx-less branch (direct-path observer) can open its own connection.
func NewShadowObservationRepository(queries db.Querier, pool *pgxpool.Pool) *ShadowObservationRepository {
	return &ShadowObservationRepository{queries: queries, pool: pool}
}

// RecordDirectWrite inserts an observation row stamped Writer="direct".
// If tx is nil the repository opens a short-lived tx on the pool —
// direct-path call sites don't hold an explicit tx today (plan Decision 11).
// Returns the inserted row for caller convenience; most callers ignore it.
func (r *ShadowObservationRepository) RecordDirectWrite(ctx context.Context, tx pgx.Tx, obs ShadowObservation) (*ShadowObservation, error) {
	obs.Writer = ShadowWriterDirect
	return r.insertObservation(ctx, tx, obs)
}

// RecordConsumerWrite inserts an observation row stamped Writer="consumer",
// Replay=false — the consumer was the sole writer (either direct failed or
// the publisher emitted without a paired direct call).
func (r *ShadowObservationRepository) RecordConsumerWrite(ctx context.Context, tx pgx.Tx, obs ShadowObservation) (*ShadowObservation, error) {
	obs.Writer = ShadowWriterConsumer
	obs.Replay = false
	return r.insertObservation(ctx, tx, obs)
}

// RecordConsumerReplay inserts an observation row for the case where the
// consumer saw an existing interaction (via FindBySourceRef / FindInWindow)
// and early-returned. Replay=true, InteractionID = the existing row's id.
// This is the expected state 100% of the time in shadow mode (direct wrote
// first, consumer observes).
func (r *ShadowObservationRepository) RecordConsumerReplay(ctx context.Context, tx pgx.Tx, obs ShadowObservation) (*ShadowObservation, error) {
	obs.Writer = ShadowWriterConsumer
	obs.Replay = true
	return r.insertObservation(ctx, tx, obs)
}

// insertObservation performs the actual INSERT. Handles the tx=nil branch
// (open a short-lived tx on the pool).
func (r *ShadowObservationRepository) insertObservation(ctx context.Context, tx pgx.Tx, obs ShadowObservation) (*ShadowObservation, error) {
	params := db.InsertEventShadowObservationParams{
		EventID:       uuidPtrToPgUUID(obs.EventID),
		Writer:        obs.Writer,
		Kind:          obs.Kind,
		Source:        obs.Source,
		SourceRef:     stringToPgText(obs.SourceRef),
		ContactID:     uuidToPgUUID(obs.ContactID),
		Direction:     obs.Direction,
		OccurredAt:    pgtype.Timestamptz{Time: obs.OccurredAt, Valid: true},
		InteractionID: uuidPtrToPgUUID(obs.InteractionID),
		Replay:        obs.Replay,
	}

	if tx != nil {
		row, err := db.New(tx).InsertEventShadowObservation(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("insert shadow observation (tx): %w", err)
		}
		return convertDbShadowObservation(row), nil
	}

	if r.pool == nil {
		return nil, errors.New("shadow observation: nil tx and nil pool")
	}
	ownTx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin shadow observation tx: %w", err)
	}
	defer func() {
		if rbErr := ownTx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			// Best-effort rollback; shadow observation is non-authoritative.
			_ = rbErr
		}
	}()

	row, err := db.New(ownTx).InsertEventShadowObservation(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("insert shadow observation: %w", err)
	}
	if err := ownTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit shadow observation: %w", err)
	}
	return convertDbShadowObservation(row), nil
}

// FindMatchingDirectWrite looks up the direct-path peer of a consumer
// observation row. Dispatches on source_ref presence:
//
//   - source_ref non-nil: match on (source, source_ref, contact_id).
//   - source_ref nil:     match on (source, contact_id, occurred_at-second).
//
// Returns (nil, nil) when no peer exists (consumer wrote first, or direct
// failed) — the caller uses that as the "no peer yet, don't log divergence"
// signal. Never returns db.ErrNotFound — the hit/miss is part of the
// normal flow.
func (r *ShadowObservationRepository) FindMatchingDirectWrite(ctx context.Context, tx pgx.Tx, obs ShadowObservation) (*ShadowObservation, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}

	if obs.SourceRef != nil {
		row, err := q.FindMatchingDirectWriteBySourceRef(ctx, db.FindMatchingDirectWriteBySourceRefParams{
			Source:    obs.Source,
			SourceRef: stringToPgText(obs.SourceRef),
			ContactID: uuidToPgUUID(obs.ContactID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			return nil, fmt.Errorf("find matching direct (ref): %w", err)
		}
		return convertDbShadowObservation(row), nil
	}

	row, err := q.FindMatchingDirectWriteByManual(ctx, db.FindMatchingDirectWriteByManualParams{
		Source:     obs.Source,
		ContactID:  uuidToPgUUID(obs.ContactID),
		OccurredAt: pgtype.Timestamptz{Time: obs.OccurredAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find matching direct (manual): %w", err)
	}
	return convertDbShadowObservation(row), nil
}

// CountByWriter returns the total number of rows for the given writer. Used
// by integration tests.
func (r *ShadowObservationRepository) CountByWriter(ctx context.Context, writer string) (int64, error) {
	return r.queries.CountShadowObservationsByWriter(ctx, writer)
}

// FindDivergences runs both the ref-bearing and manual divergence queries
// bounded by [from, to) and unions their rows. A non-empty result means one
// or more observations drifted between direct and consumer during the
// window. Plan Decision 14 Part B.
func (r *ShadowObservationRepository) FindDivergences(ctx context.Context, from, to time.Time) ([]ShadowDivergence, error) {
	params := db.FindShadowDivergencesRefBearingParams{
		ObservedAtFrom: pgtype.Timestamptz{Time: from, Valid: true},
		ObservedAtTo:   pgtype.Timestamptz{Time: to, Valid: true},
	}
	ref, err := r.queries.FindShadowDivergencesRefBearing(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("find shadow divergences (ref): %w", err)
	}
	manualParams := db.FindShadowDivergencesManualParams{
		ObservedAtFrom: pgtype.Timestamptz{Time: from, Valid: true},
		ObservedAtTo:   pgtype.Timestamptz{Time: to, Valid: true},
	}
	manual, err := r.queries.FindShadowDivergencesManual(ctx, manualParams)
	if err != nil {
		return nil, fmt.Errorf("find shadow divergences (manual): %w", err)
	}

	out := make([]ShadowDivergence, 0, len(ref)+len(manual))
	for _, row := range ref {
		out = append(out, ShadowDivergence{
			Source:             row.Source,
			SourceRef:          pgTextToStrPtr(row.SourceRef),
			ContactID:          uuid.UUID(row.ContactID.Bytes),
			DirectDirection:    pgTextToStrPtr(row.DirectDirection),
			ConsumerDirection:  pgTextToStrPtr(row.ConsumerDirection),
			DirectOccurredAt:   pgTimestamptzToTimePtr(row.DirectTs),
			ConsumerOccurredAt: pgTimestamptzToTimePtr(row.ConsumerTs),
		})
	}
	for _, row := range manual {
		out = append(out, ShadowDivergence{
			Source:             row.Source,
			SourceRef:          nil,
			ContactID:          uuid.UUID(row.ContactID.Bytes),
			DirectDirection:    pgTextToStrPtr(row.DirectDirection),
			ConsumerDirection:  pgTextToStrPtr(row.ConsumerDirection),
			DirectOccurredAt:   pgTimestamptzToTimePtr(row.DirectTs),
			ConsumerOccurredAt: pgTimestamptzToTimePtr(row.ConsumerTs),
		})
	}
	return out, nil
}

// convertDbShadowObservation maps the sqlc-generated row into the repo-
// level ShadowObservation shape.
func convertDbShadowObservation(row *db.EventShadowObservation) *ShadowObservation {
	obs := &ShadowObservation{
		Writer:    row.Writer,
		Kind:      row.Kind,
		Source:    row.Source,
		Direction: row.Direction,
		Replay:    row.Replay,
	}
	if row.ID.Valid {
		obs.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.EventID.Valid {
		id := uuid.UUID(row.EventID.Bytes)
		obs.EventID = &id
	}
	if row.ContactID.Valid {
		obs.ContactID = uuid.UUID(row.ContactID.Bytes)
	}
	if row.SourceRef.Valid {
		s := row.SourceRef.String
		obs.SourceRef = &s
	}
	if row.OccurredAt.Valid {
		obs.OccurredAt = row.OccurredAt.Time.UTC()
	}
	if row.InteractionID.Valid {
		id := uuid.UUID(row.InteractionID.Bytes)
		obs.InteractionID = &id
	}
	if row.ObservedAt.Valid {
		obs.ObservedAt = row.ObservedAt.Time.UTC()
	}
	return obs
}

// uuidPtrToPgUUID converts a *uuid.UUID to pgtype.UUID, treating nil as
// invalid (NULL). Used for nullable FK / ref columns
// (event_id, interaction_id).
func uuidPtrToPgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{Valid: false}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// pgTextToStrPtr converts a pgtype.Text to *string; invalid/NULL → nil.
func pgTextToStrPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// pgTimestamptzToTimePtr converts a pgtype.Timestamptz to *time.Time;
// invalid/NULL → nil.
func pgTimestamptzToTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
