package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// CadenceMode values must mirror config.EventBusCadenceMode*. Kept
// duplicated here so non-config callers don't import config just to
// name a mode. Cross-reference: see config.EventBusCadenceMode* for
// the startup-gate + unsafe-override semantics.
const (
	CadenceModeOff     = "off"
	CadenceModeShadow  = "shadow"
	CadenceModeCutover = "cutover"
)

// eventClaimer is the subset of *repository.EventConsumerClaimRepository
// the consumer depends on. Keeping it as an interface lets unit tests
// stub the claim path without a DB.
type eventClaimer interface {
	TryClaimTx(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, consumer string) (bool, error)
}

// contactCadenceReader reads the single cadence string off a contact row
// inside the caller's tx. Used by BulkApply (to compute contact_by from
// the chosen cadence string after merge) and ApplyInteraction (to
// honor the same rule that CadenceUpdater.HandleEvent uses for
// payload-less invocations). Narrow interface so tests can stub.
type contactCadenceReader interface {
	GetContactTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.Contact, error)
}

// CadenceUpdater is the PR 8 cutover consumer — the sole writer of
// contact.last_contacted, contact.last_outreach_at,
// contact.last_response_at, and contact.contact_by.
//
// Entry points (all funnel into applyTx):
//   - HandleEvent:       envelope-driven (from InteractionRecorder inline
//     call + queued river worker). Dedupes on
//     event_consumer_claim (event_id, consumer='cadence_updater').
//   - ApplyInteraction:  direct-invoke for ExtendInteraction +
//     PromoteInteractionToMutual (no event, no claim).
//   - BulkApply:         direct-invoke for MergeContacts. Forward-max
//     semantics across individual fields.
//   - ApplyContactByOverride: direct-invoke for user cadence edits
//     (can clear or backdate contact_by unconditionally).
type CadenceUpdater struct {
	claims    eventClaimer
	contacts  contactCadenceReader
	queries   db.Querier
	mode      string // config.EventBusCadenceMode*
	unsafeOff bool   // mirrors config.EventBus.UnsafeAllowOffMode for self-reported diagnostics
}

// NewCadenceUpdater constructs the consumer. `queries` must be a
// generic *db.Queries backed by the application's primary pool; the
// tx-scoped path (applyTx) uses `db.New(tx)` directly. `mode` must be
// one of CadenceMode*; unknown values are treated as off so callers
// never accidentally write through a misconfigured consumer.
func NewCadenceUpdater(
	claims eventClaimer,
	contacts contactCadenceReader,
	queries db.Querier,
	mode string,
	unsafeOff bool,
) *CadenceUpdater {
	return &CadenceUpdater{
		claims:    claims,
		contacts:  contacts,
		queries:   queries,
		mode:      mode,
		unsafeOff: unsafeOff,
	}
}

// cadenceWriteRequest is the internal shape passed to applyTx. Each
// cadence column has an explicit apply flag + the value to write when
// the flag is true. Branch selects forward-only vs unconditional SQL.
type cadenceWriteRequest struct {
	ContactID uuid.UUID
	Branch    string // CadenceShadowBranchForward or CadenceShadowBranchUnconditional

	ApplyLastContacted  bool
	LastContacted       *time.Time
	ApplyLastOutreachAt bool
	LastOutreachAt      *time.Time
	ApplyLastResponseAt bool
	LastResponseAt      *time.Time
	ApplyContactBy      bool
	ContactBy           *time.Time // nil means "clear contact_by" on the unconditional branch
}

// --------------------------------------------------------------------------
// Public entry points.
// --------------------------------------------------------------------------

// HandleEvent processes an interaction.recorded envelope. It first claims
// the event (durable dedupe across inline + queued delivery), then
// applies the direction-conditional cadence write. Mode=off short-
// circuits before the claim; the job completes successfully so river
// doesn't retry.
func (h *CadenceUpdater) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("cadence_updater: nil envelope")
	}
	if tx == nil {
		return errors.New("cadence_updater: nil tx")
	}

	if h.mode == CadenceModeOff {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Bool("unsafe_allow_off", h.unsafeOff).
			Msg("cadence_updater: mode=off; skipping cadence write")
		return nil
	}

	var p events.InteractionRecordedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return fmt.Errorf("unmarshal interaction.recorded payload: %w", err)
	}
	if p.Version != 2 {
		logger.Error().
			Str("event_id", env.ID.String()).
			Int("version", p.Version).
			Msg("cadence_updater: rejecting interaction.recorded payload with Version != 2; external producers must emit V2")
		return nil
	}
	if p.PrevCadenceSnapshot == nil {
		logger.Error().
			Str("event_id", env.ID.String()).
			Msg("cadence_updater: V2 payload missing PrevCadenceSnapshot; skipping")
		return nil
	}

	// Durable dedupe. Whoever wins the claim (inline recorder call or
	// queued worker) runs the write; the loser returns nil. Claim +
	// write commit atomically in the caller's tx — a pre-commit crash
	// rolls back both together, so a post-commit re-delivery is safe.
	claimed, err := h.claims.TryClaimTx(ctx, tx, env.ID, repository.EventConsumerCadenceUpdater)
	if err != nil {
		return fmt.Errorf("claim event for cadence_updater: %w", err)
	}
	if !claimed {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Msg("cadence_updater: event already claimed by another path; no-op")
		return nil
	}

	prev := repository.ContactCadenceFields{
		LastContacted:  p.PrevCadenceSnapshot.LastContacted,
		LastOutreachAt: p.PrevCadenceSnapshot.LastOutreachAt,
		LastResponseAt: p.PrevCadenceSnapshot.LastResponseAt,
		ContactBy:      p.PrevCadenceSnapshot.ContactBy,
	}
	var cadenceStr string
	if p.PrevCadenceValue != nil {
		cadenceStr = *p.PrevCadenceValue
	}

	req := h.buildInteractionWrite(p.ContactID, p.Direction, p.Source, p.OccurredAt, prev, cadenceStr)
	return h.applyTx(ctx, tx, req)
}

// ApplyInteraction is the direct-invoke entry point for
// ExtendInteraction + PromoteInteractionToMutual. Reads the current
// contact row (for prev-cadence + cadence string) inside the caller's
// tx, then dispatches through applyTx. No claim is taken because these
// paths do NOT emit interaction.recorded and therefore cannot collide
// with a queued worker. The request type is defined in repository
// (alongside RecordInteractionRequest) so ContactService can reference
// it without importing this package.
func (h *CadenceUpdater) ApplyInteraction(ctx context.Context, tx pgx.Tx, req repository.ApplyInteractionRequest) error {
	if tx == nil {
		return errors.New("cadence_updater: nil tx")
	}
	if h.mode == CadenceModeOff {
		return nil
	}
	contact, err := h.contacts.GetContactTx(ctx, tx, req.ContactID)
	if err != nil {
		return fmt.Errorf("get contact for apply interaction: %w", err)
	}
	prev := repository.ContactCadenceFieldsFromContact(contact)
	var cadenceStr string
	if contact.Cadence != nil {
		cadenceStr = *contact.Cadence
	}
	writeReq := h.buildInteractionWrite(req.ContactID, req.Direction, req.Source, req.OccurredAt, prev, cadenceStr)
	return h.applyTx(ctx, tx, writeReq)
}

// BulkApply is the direct-invoke entry point for MergeContacts. The
// caller computes per-field merged cadence values (typically forward-
// max of source + target pre-merge snapshots) and hands them in. This
// method dispatches them through applyTx on the forward-only branch so
// MergeContacts NEVER moves cadence state backward.
//
// MergeContacts must update the target contact's chosen cadence string
// BEFORE calling BulkApply (plan Decision 7) so contact_by recomputation
// picks up the correct cadence value.
func (h *CadenceUpdater) BulkApply(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, fields repository.ContactCadenceFields) error {
	if tx == nil {
		return errors.New("cadence_updater: nil tx")
	}
	if h.mode == CadenceModeOff {
		return nil
	}
	req := cadenceWriteRequest{
		ContactID:           contactID,
		Branch:              repository.CadenceShadowBranchForward,
		ApplyLastContacted:  fields.LastContacted != nil,
		LastContacted:       fields.LastContacted,
		ApplyLastOutreachAt: fields.LastOutreachAt != nil,
		LastOutreachAt:      fields.LastOutreachAt,
		ApplyLastResponseAt: fields.LastResponseAt != nil,
		LastResponseAt:      fields.LastResponseAt,
		ApplyContactBy:      fields.ContactBy != nil,
		ContactBy:           fields.ContactBy,
	}
	return h.applyTx(ctx, tx, req)
}

// ApplyContactByOverride is the direct-invoke entry point for user-driven
// cadence preference edits (cadence-set, cadence-change, cadence-clear).
// Takes the unconditional branch so operators and users can legitimately
// move contact_by backward or clear it when cadence is removed. Never
// touches last_contacted, last_outreach_at, or last_response_at.
func (h *CadenceUpdater) ApplyContactByOverride(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, contactBy *time.Time) error {
	if tx == nil {
		return errors.New("cadence_updater: nil tx")
	}
	if h.mode == CadenceModeOff {
		return nil
	}
	req := cadenceWriteRequest{
		ContactID:      contactID,
		Branch:         repository.CadenceShadowBranchUnconditional,
		ApplyContactBy: true,
		ContactBy:      contactBy,
	}
	return h.applyTx(ctx, tx, req)
}

// --------------------------------------------------------------------------
// Shared write engine.
// --------------------------------------------------------------------------

// buildInteractionWrite builds the cadenceWriteRequest for an
// interaction-driven path (HandleEvent + ApplyInteraction). Reproduces
// the direction-rule apply flags and the contact_by derivation the
// PR 6 direct path used (plan Decision 3 + §3.4.2), so the post-cutover
// write matches the pre-cutover behavior bit-for-bit.
func (h *CadenceUpdater) buildInteractionWrite(
	contactID uuid.UUID, direction, source string, occurredAt time.Time,
	prev repository.ContactCadenceFields, cadenceStr string,
) cadenceWriteRequest {
	hasCadence := cadenceStr != ""
	isManual := source == repository.InteractionSourceManual

	applyLastContacted, applyLastOutreachAt, applyLastResponseAt, directionAllowsContactBy := repository.CadenceApplyFlagsByDirection(direction)
	applyContactBy := directionAllowsContactBy && repository.ShouldApplyContactBy(prev.LastContacted, occurredAt, isManual, hasCadence)

	branch := repository.CadenceShadowBranchForward
	if isManual {
		branch = repository.CadenceShadowBranchUnconditional
	}

	req := cadenceWriteRequest{
		ContactID:           contactID,
		Branch:              branch,
		ApplyLastContacted:  applyLastContacted,
		ApplyLastOutreachAt: applyLastOutreachAt,
		ApplyLastResponseAt: applyLastResponseAt,
		ApplyContactBy:      applyContactBy,
	}
	if applyLastContacted {
		t := occurredAt
		req.LastContacted = &t
	}
	if applyLastOutreachAt {
		t := occurredAt
		req.LastOutreachAt = &t
	}
	if applyLastResponseAt {
		t := occurredAt
		req.LastResponseAt = &t
	}
	if applyContactBy {
		cadenceType, err := cadence.ParseCadence(cadenceStr)
		if err != nil {
			req.ApplyContactBy = false
		} else {
			t := cadence.CalculateContactBy(occurredAt, cadenceType)
			req.ContactBy = &t
		}
	}
	return req
}

// applyTx is the shared write engine. Routes to UpdateContactCadenceForward
// or UpdateContactCadenceUnconditional based on req.Branch. A no-op
// request (all four apply flags false) short-circuits to avoid issuing
// an UPDATE that would bump updated_at for nothing.
func (h *CadenceUpdater) applyTx(ctx context.Context, tx pgx.Tx, req cadenceWriteRequest) error {
	if !req.ApplyLastContacted && !req.ApplyLastOutreachAt && !req.ApplyLastResponseAt && !req.ApplyContactBy {
		return nil
	}
	q := db.New(tx)
	switch req.Branch {
	case repository.CadenceShadowBranchForward:
		return q.UpdateContactCadenceForward(ctx, db.UpdateContactCadenceForwardParams{
			ApplyLastContacted:  req.ApplyLastContacted,
			LastContacted:       timePtrToPgTimestamptz(req.LastContacted),
			ApplyLastOutreachAt: req.ApplyLastOutreachAt,
			LastOutreachAt:      timePtrToPgTimestamptz(req.LastOutreachAt),
			ApplyLastResponseAt: req.ApplyLastResponseAt,
			LastResponseAt:      timePtrToPgTimestamptz(req.LastResponseAt),
			ApplyContactBy:      req.ApplyContactBy,
			ContactBy:           timePtrToPgDate(req.ContactBy),
			ID:                  uuidToPgUUID(req.ContactID),
		})
	case repository.CadenceShadowBranchUnconditional:
		return q.UpdateContactCadenceUnconditional(ctx, db.UpdateContactCadenceUnconditionalParams{
			ApplyLastContacted:  req.ApplyLastContacted,
			LastContacted:       timePtrToPgTimestamptz(req.LastContacted),
			ApplyLastOutreachAt: req.ApplyLastOutreachAt,
			LastOutreachAt:      timePtrToPgTimestamptz(req.LastOutreachAt),
			ApplyLastResponseAt: req.ApplyLastResponseAt,
			LastResponseAt:      timePtrToPgTimestamptz(req.LastResponseAt),
			ApplyContactBy:      req.ApplyContactBy,
			ContactBy:           timePtrToPgDate(req.ContactBy),
			ID:                  uuidToPgUUID(req.ContactID),
		})
	default:
		return fmt.Errorf("cadence_updater: unknown branch %q", req.Branch)
	}
}

// --------------------------------------------------------------------------
// pgtype helpers. Small local duplicates of repository/conversions.go — the
// consumer package must not depend on the repository's private helpers and
// these paths are hot enough that an inline conversion beats a new
// repository-level wrapper.
// --------------------------------------------------------------------------

func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func timePtrToPgTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func timePtrToPgDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{Valid: false}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

// --------------------------------------------------------------------------
// River worker wrapper.
// --------------------------------------------------------------------------

// CadenceUpdaterWorker is the river worker that dispatches queued
// CadenceUpdaterJobArgs to CadenceUpdater.HandleEvent. In cutover mode
// the inline recorder path claims the event first, so this worker is
// almost always a durable no-op — the claim already exists.
type CadenceUpdaterWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
	bus     eventBusTx
	pool    *pgxpool.Pool
	handler *CadenceUpdater
}

func NewCadenceUpdaterWorker(bus eventBusTx, pool *pgxpool.Pool, handler *CadenceUpdater) *CadenceUpdaterWorker {
	return &CadenceUpdaterWorker{
		bus:     bus,
		pool:    pool,
		handler: handler,
	}
}

func (w *CadenceUpdaterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.handler.HandleEvent(ctx, tx, env)
	})
}

func (*CadenceUpdaterWorker) Timeout(*river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}

// CadenceModeFromConfig narrows the config string to one of the three
// valid mode names. Unknown values fall back to off with an ERROR log
// — a misconfigured consumer MUST NOT silently write. config.Validate
// rejects unknown values at startup; this fallback defends test-
// constructed configs.
func CadenceModeFromConfig(mode string) string {
	switch mode {
	case config.EventBusCadenceModeOff:
		return CadenceModeOff
	case config.EventBusCadenceModeShadow:
		return CadenceModeShadow
	case config.EventBusCadenceModeCutover:
		return CadenceModeCutover
	default:
		logger.Error().
			Str("mode", mode).
			Msg("cadence_updater: unknown mode; defaulting to off to avoid unsafe writes")
		return CadenceModeOff
	}
}
