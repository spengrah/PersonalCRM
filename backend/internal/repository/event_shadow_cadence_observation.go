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

// CadenceShadowObservation models a row in the event_shadow_cadence_observation
// table — one write attempt by one of the two shadow-mode paths (direct or
// consumer) for a given interaction.recorded event. See
// .ai/spec/event-bus-foundation.md §3.4.2 and
// .ai/log/plan/event-bus-foundation-pr7-cadence-updater-shadow.md Design
// Decision 1.
//
// EventID is required (NOT NULL in the table) — every observation is tied
// to a concrete interaction.recorded event. ExtendInteraction /
// PromoteInteractionToMutual / MergeContacts run WITHOUT a paired event
// in PR 7 and therefore produce no observation row (see plan Decision 6).
type CadenceShadowObservation struct {
	ID         uuid.UUID
	EventID    uuid.UUID
	Writer     string
	ContactID  uuid.UUID
	Source     string
	Direction  string
	Branch     string
	OccurredAt time.Time

	// Pre-image — the four cadence values before the (hypothetical) write.
	PrevLastContacted  *time.Time
	PrevLastOutreachAt *time.Time
	PrevLastResponseAt *time.Time
	PrevContactBy      *time.Time

	// Post-image — what the consumer WOULD write. Nil for an apply-flag-false
	// column (direction rule didn't select it).
	NextLastContacted  *time.Time
	NextLastOutreachAt *time.Time
	NextLastResponseAt *time.Time
	NextContactBy      *time.Time

	ApplyLastContacted  bool
	ApplyLastOutreachAt bool
	ApplyLastResponseAt bool
	ApplyContactBy      bool

	ObservedAt time.Time
}

// CadenceShadowDivergence is a row-shape result of FindDivergences. Either
// the direct or consumer side may be absent (FULL OUTER JOIN) — test
// callers inspect DirectBranch / ConsumerBranch for presence.
type CadenceShadowDivergence struct {
	EventID   uuid.UUID
	ContactID uuid.UUID

	DirectBranch   *string
	ConsumerBranch *string

	DirectNextLastContacted    *time.Time
	ConsumerNextLastContacted  *time.Time
	DirectNextLastOutreachAt   *time.Time
	ConsumerNextLastOutreachAt *time.Time
	DirectNextLastResponseAt   *time.Time
	ConsumerNextLastResponseAt *time.Time
	DirectNextContactBy        *time.Time
	ConsumerNextContactBy      *time.Time
}

// Writer / branch constants — mirror the CHECK constraints on the table.
const (
	CadenceShadowWriterDirect   = "direct"
	CadenceShadowWriterConsumer = "consumer"

	CadenceShadowBranchForward       = "forward"
	CadenceShadowBranchUnconditional = "unconditional"
)

// CadenceShadowDrainFn is the per-call shape of the direct-path cadence
// shadow observation drain. Deferred by ContactService.RecordInteractionTx
// and invoked by the event-bus caller (InteractionRecorder) AFTER the
// interaction.recorded event is published — so the caller can bind
// eventID to the freshly-assigned event row id (plan Decision 6). Both
// direct and consumer observations must share the same event_id for
// the post-bake FULL OUTER JOIN in FindDivergences to resolve.
type CadenceShadowDrainFn func(ctx context.Context, eventID uuid.UUID)

// CadenceShadowObservationRepository persists cadence shadow observations.
// Parallel in shape to ShadowObservationRepository (PR 5) — a two-table,
// two-repo split keeps the interaction-row divergence query (038) and the
// cadence divergence query (039) cleanly separated.
type CadenceShadowObservationRepository struct {
	queries db.Querier
	pool    *pgxpool.Pool
}

// NewCadenceShadowObservationRepository constructs the repository. pool is
// required so the tx-less branch (direct-path post-commit closure) can
// open its own connection. Passing nil is allowed — RecordDirect with
// tx==nil then returns an error.
func NewCadenceShadowObservationRepository(queries db.Querier, pool *pgxpool.Pool) *CadenceShadowObservationRepository {
	return &CadenceShadowObservationRepository{queries: queries, pool: pool}
}

// RecordDirect inserts an observation stamped writer="direct". When tx is
// nil the repo opens a short-lived own-tx on the configured pool — this
// is the direct-path shadow observer's expected call shape: the outer
// authoritative tx has already committed, and the observation runs on
// its own best-effort tx. ON CONFLICT (event_id, writer) DO NOTHING is
// applied by the underlying sqlc query, so river retries / double-invokes
// cannot produce duplicate rows.
func (r *CadenceShadowObservationRepository) RecordDirect(ctx context.Context, tx pgx.Tx, obs CadenceShadowObservation) error {
	obs.Writer = CadenceShadowWriterDirect
	return r.insertObservation(ctx, tx, obs)
}

// RecordConsumer inserts an observation stamped writer="consumer". Callers
// normally pass the worker tx (not nil) so the observation commits
// atomically with the worker's event-processing tx — no orphan rows on
// retry. ON CONFLICT (event_id, writer) DO NOTHING in the SQL gives
// at-most-one-row-per-writer semantics.
func (r *CadenceShadowObservationRepository) RecordConsumer(ctx context.Context, tx pgx.Tx, obs CadenceShadowObservation) error {
	obs.Writer = CadenceShadowWriterConsumer
	return r.insertObservation(ctx, tx, obs)
}

// insertObservation performs the INSERT ... ON CONFLICT DO NOTHING.
// Handles the tx=nil branch by opening a short-lived tx on the pool.
// Returns nil on both fresh insert and conflict (same-event retry).
func (r *CadenceShadowObservationRepository) insertObservation(ctx context.Context, tx pgx.Tx, obs CadenceShadowObservation) error {
	params := db.InsertCadenceShadowObservationParams{
		EventID:             uuidToPgUUID(obs.EventID),
		Writer:              obs.Writer,
		ContactID:           uuidToPgUUID(obs.ContactID),
		Source:              obs.Source,
		Direction:           obs.Direction,
		Branch:              obs.Branch,
		OccurredAt:          pgtype.Timestamptz{Time: obs.OccurredAt, Valid: true},
		PrevLastContacted:   timeToPgTimestamptz(obs.PrevLastContacted),
		PrevLastOutreachAt:  timeToPgTimestamptz(obs.PrevLastOutreachAt),
		PrevLastResponseAt:  timeToPgTimestamptz(obs.PrevLastResponseAt),
		PrevContactBy:       timeToPgDate(obs.PrevContactBy),
		NextLastContacted:   timeToPgTimestamptz(obs.NextLastContacted),
		NextLastOutreachAt:  timeToPgTimestamptz(obs.NextLastOutreachAt),
		NextLastResponseAt:  timeToPgTimestamptz(obs.NextLastResponseAt),
		NextContactBy:       timeToPgDate(obs.NextContactBy),
		ApplyLastContacted:  obs.ApplyLastContacted,
		ApplyLastOutreachAt: obs.ApplyLastOutreachAt,
		ApplyLastResponseAt: obs.ApplyLastResponseAt,
		ApplyContactBy:      obs.ApplyContactBy,
	}

	if tx != nil {
		_, err := db.New(tx).InsertCadenceShadowObservation(ctx, params)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert cadence shadow observation (tx): %w", err)
		}
		// ON CONFLICT DO NOTHING returns ErrNoRows on duplicate — treat as
		// idempotent success per plan Decision 4.
		return nil
	}

	if r.pool == nil {
		return errors.New("cadence shadow observation: nil tx and nil pool")
	}
	ownTx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cadence shadow observation tx: %w", err)
	}
	defer func() {
		if rbErr := ownTx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			// Best-effort rollback; shadow observation is non-authoritative.
			_ = rbErr
		}
	}()

	if _, err := db.New(ownTx).InsertCadenceShadowObservation(ctx, params); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert cadence shadow observation: %w", err)
	}
	if err := ownTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cadence shadow observation: %w", err)
	}
	return nil
}

// FindMatchingDirect returns the direct-path observation for the given
// event_id, if present. Used by the consumer's inline divergence logger
// to compare its `next_*` values against direct's. Returns (nil, nil)
// when the direct row hasn't been written yet — a normal condition in
// PR 7 because the direct path writes its row from a post-commit closure
// that may not have landed by the time the consumer runs (see plan
// Decision 4 ordering note).
func (r *CadenceShadowObservationRepository) FindMatchingDirect(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*CadenceShadowObservation, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	row, err := q.FindCadenceShadowObservationByEventAndWriter(ctx, db.FindCadenceShadowObservationByEventAndWriterParams{
		EventID: uuidToPgUUID(eventID),
		Writer:  CadenceShadowWriterDirect,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find matching direct cadence observation: %w", err)
	}
	return convertDbCadenceShadowObservation(row), nil
}

// FindMatchingConsumer returns the consumer-path observation for the
// given event_id. Useful for integration tests that insert a consumer
// row directly and then re-read it to assert field values.
func (r *CadenceShadowObservationRepository) FindMatchingConsumer(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*CadenceShadowObservation, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	row, err := q.FindCadenceShadowObservationByEventAndWriter(ctx, db.FindCadenceShadowObservationByEventAndWriterParams{
		EventID: uuidToPgUUID(eventID),
		Writer:  CadenceShadowWriterConsumer,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find matching consumer cadence observation: %w", err)
	}
	return convertDbCadenceShadowObservation(row), nil
}

// CountByWriter returns the total number of rows for the given writer.
// Used by integration tests and the bake-window evidence collection.
func (r *CadenceShadowObservationRepository) CountByWriter(ctx context.Context, writer string) (int64, error) {
	return r.queries.CountCadenceShadowObservationsByWriter(ctx, writer)
}

// CountByContact returns the total number of rows scoped to the given
// contact. Integration tests on the shared test DB use this instead of
// CountByWriter so the assertion doesn't race against rows written by
// concurrently-running tests on other contacts.
func (r *CadenceShadowObservationRepository) CountByContact(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.CountCadenceShadowObservationsByContact(ctx, uuidToPgUUID(contactID))
}

// RecordAtTime is a test-only variant of RecordDirect / RecordConsumer
// that lets the caller pin observed_at explicitly. Production code must
// continue to use RecordDirect / RecordConsumer (which let the DB
// default observed_at to NOW()) — the DEFAULT timestamp is load-bearing
// for the grace-window filter in FindDivergences. This wrapper exists
// so integration tests can simulate "direct landed 4s ago / consumer
// landed 6s ago" without actually waiting wall-clock seconds.
func (r *CadenceShadowObservationRepository) RecordAtTime(ctx context.Context, obs CadenceShadowObservation, observedAt time.Time) error {
	params := db.InsertCadenceShadowObservationAtTimeParams{
		EventID:             uuidToPgUUID(obs.EventID),
		Writer:              obs.Writer,
		ContactID:           uuidToPgUUID(obs.ContactID),
		Source:              obs.Source,
		Direction:           obs.Direction,
		Branch:              obs.Branch,
		OccurredAt:          pgtype.Timestamptz{Time: obs.OccurredAt, Valid: true},
		ApplyLastContacted:  obs.ApplyLastContacted,
		ApplyLastOutreachAt: obs.ApplyLastOutreachAt,
		ApplyLastResponseAt: obs.ApplyLastResponseAt,
		ApplyContactBy:      obs.ApplyContactBy,
		ObservedAt:          pgtype.Timestamptz{Time: observedAt, Valid: true},
	}
	if _, err := r.queries.InsertCadenceShadowObservationAtTime(ctx, params); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert cadence shadow observation at time: %w", err)
	}
	return nil
}

// FindDivergences returns rows from the FULL OUTER JOIN of direct vs
// consumer observations over [from, to). A non-empty result indicates
// drift (direct-missing, consumer-missing, or next_* disagreement) —
// before raising an alert, callers apply the race-class filters from
// plan Decision 4 (skip rows with contact.deleted_at IS NOT NULL at
// bake end, skip rows observed in the last 5 seconds to let the direct
// post-commit closure land).
func (r *CadenceShadowObservationRepository) FindDivergences(ctx context.Context, from, to time.Time) ([]CadenceShadowDivergence, error) {
	rows, err := r.queries.FindCadenceShadowDivergences(ctx, db.FindCadenceShadowDivergencesParams{
		ObservedAtFrom: pgtype.Timestamptz{Time: from, Valid: true},
		ObservedAtTo:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("find cadence shadow divergences: %w", err)
	}
	out := make([]CadenceShadowDivergence, 0, len(rows))
	for _, row := range rows {
		out = append(out, CadenceShadowDivergence{
			EventID:                    uuid.UUID(row.EventID.Bytes),
			ContactID:                  uuid.UUID(row.ContactID.Bytes),
			DirectBranch:               pgTextToStrPtr(row.DirectBranch),
			ConsumerBranch:             pgTextToStrPtr(row.ConsumerBranch),
			DirectNextLastContacted:    pgTimestamptzToTimePtr(row.DirectNextLastContacted),
			ConsumerNextLastContacted:  pgTimestamptzToTimePtr(row.ConsumerNextLastContacted),
			DirectNextLastOutreachAt:   pgTimestamptzToTimePtr(row.DirectNextLastOutreachAt),
			ConsumerNextLastOutreachAt: pgTimestamptzToTimePtr(row.ConsumerNextLastOutreachAt),
			DirectNextLastResponseAt:   pgTimestamptzToTimePtr(row.DirectNextLastResponseAt),
			ConsumerNextLastResponseAt: pgTimestamptzToTimePtr(row.ConsumerNextLastResponseAt),
			DirectNextContactBy:        pgDateToTimePtr(row.DirectNextContactBy),
			ConsumerNextContactBy:      pgDateToTimePtr(row.ConsumerNextContactBy),
		})
	}
	return out, nil
}

// convertDbCadenceShadowObservation maps the sqlc-generated row into the
// repo-level CadenceShadowObservation shape.
func convertDbCadenceShadowObservation(row *db.EventShadowCadenceObservation) *CadenceShadowObservation {
	obs := &CadenceShadowObservation{
		Writer:              row.Writer,
		Source:              row.Source,
		Direction:           row.Direction,
		Branch:              row.Branch,
		ApplyLastContacted:  row.ApplyLastContacted,
		ApplyLastOutreachAt: row.ApplyLastOutreachAt,
		ApplyLastResponseAt: row.ApplyLastResponseAt,
		ApplyContactBy:      row.ApplyContactBy,
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
	obs.PrevLastContacted = pgTimestamptzToTimePtr(row.PrevLastContacted)
	obs.PrevLastOutreachAt = pgTimestamptzToTimePtr(row.PrevLastOutreachAt)
	obs.PrevLastResponseAt = pgTimestamptzToTimePtr(row.PrevLastResponseAt)
	obs.PrevContactBy = pgDateToTimePtr(row.PrevContactBy)
	obs.NextLastContacted = pgTimestamptzToTimePtr(row.NextLastContacted)
	obs.NextLastOutreachAt = pgTimestamptzToTimePtr(row.NextLastOutreachAt)
	obs.NextLastResponseAt = pgTimestamptzToTimePtr(row.NextLastResponseAt)
	obs.NextContactBy = pgDateToTimePtr(row.NextContactBy)
	if row.ObservedAt.Valid {
		obs.ObservedAt = row.ObservedAt.Time.UTC()
	}
	return obs
}
