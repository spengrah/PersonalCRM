package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// CadenceMode gates the PR 7-8 rollout for the CadenceUpdater consumer
// (spec §3.9; plan Decision 9). Mirrors the constants in
// config.EventBusCadenceMode*. Kept duplicated here so non-config callers
// don't import config just to name a mode.
const (
	CadenceModeOff     = "off"
	CadenceModeShadow  = "shadow"
	CadenceModeCutover = "cutover"
)

// cadenceShadowRecorder is the subset of *repository.CadenceShadowObservationRepository
// the cadence updater depends on. Single-file unit tests stub it without
// touching the DB. The tx is the worker's own tx — the observation
// commits atomically with the worker's event-processing tx.
type cadenceShadowRecorder interface {
	RecordConsumer(ctx context.Context, tx pgx.Tx, obs repository.CadenceShadowObservation) error
	FindMatchingDirect(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*repository.CadenceShadowObservation, error)
}

// CadenceUpdater is the PR 7 consumer that replays the direct-path
// cadence-column write in-memory and records a shadow observation for
// post-bake divergence comparison. It runs on every interaction.recorded
// event (routed via consumerJobsForKind). In shadow mode it does NOT
// mutate the contact row — the direct-path applyInteractionEffectsFromRow
// remains the authoritative writer (plan Decision 4).
//
// PR 8 cutover will remove the direct-path writes and have this consumer
// mutate contact cadence directly.
type CadenceUpdater struct {
	cadenceShadowRepo cadenceShadowRecorder
	bus               eventBusTx
	mode              string // config.EventBusCadenceMode*
}

// NewCadenceUpdater constructs the consumer. mode is the current
// EVENT_BUS_CADENCE_MODE value; HandleEvent short-circuits with a DEBUG
// log when mode == "off" (plan Decision 9 always-register model).
func NewCadenceUpdater(
	cadenceShadowRepo cadenceShadowRecorder,
	bus eventBusTx,
	mode string,
) *CadenceUpdater {
	return &CadenceUpdater{
		cadenceShadowRepo: cadenceShadowRepo,
		bus:               bus,
		mode:              mode,
	}
}

// HandleEvent is the per-event entry point. Always invoked by the river
// worker — the worker is registered unconditionally regardless of mode.
// In "off" mode the method short-circuits after a DEBUG log. In shadow
// (and cutover, which PR 7 treats as shadow) it:
//
//  1. Unmarshals the V2 payload. Version != 2 or nil PrevCadenceSnapshot
//     → ERROR log + nil return (don't retry; payload-shape bugs are not
//     transient).
//  2. Replays the direction-rule apply flags against payload.PrevCadenceSnapshot.
//  3. Computes next_* values using the same forwardMax helper the direct
//     path uses (plan Decision 4).
//  4. Inserts a writer='consumer' observation via ON CONFLICT DO NOTHING.
//  5. Inline divergence check against FindMatchingDirect — log ERROR on
//     mismatch, DEBUG when direct is absent (normal; direct runs post-
//     commit and may land after consumer).
func (h *CadenceUpdater) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("cadence_updater: nil envelope")
	}
	if tx == nil {
		return errors.New("cadence_updater: nil tx")
	}

	// Off mode: log and return. Jobs still complete successfully; zero
	// DB writes and no observations. Matches PR 5 InteractionRecorder's
	// pattern for mode gating inside HandleEvent (plan Decision 9 fix 2).
	if h.mode == CadenceModeOff {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Msg("cadence_updater: mode=off; skipping shadow observation")
		return nil
	}

	var p events.InteractionRecordedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		// Malformed payload → river retries won't help, but returning
		// an error lets the worker surface the failure once per attempt
		// before exhausting MaxAttempts. Matches PR 5/6 pattern.
		return fmt.Errorf("unmarshal interaction.recorded payload: %w", err)
	}

	// Version + PrevCadenceSnapshot presence is a hard prerequisite for
	// the shadow computation. V1 payloads (PR 5/6 emit before cutover
	// to V2) or V2 payloads with a nil snapshot cannot be matched bit-
	// for-bit against the direct path. V3+ payloads may carry new fields
	// whose semantics this consumer doesn't understand and must also be
	// rejected until a future PR adopts them. Log ERROR and succeed the
	// job — retries won't change the payload shape.
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

	prev := repository.ContactCadenceFields{
		LastContacted:  p.PrevCadenceSnapshot.LastContacted,
		LastOutreachAt: p.PrevCadenceSnapshot.LastOutreachAt,
		LastResponseAt: p.PrevCadenceSnapshot.LastResponseAt,
		ContactBy:      p.PrevCadenceSnapshot.ContactBy,
	}

	// Cadence value at emit time. Our own emit code (contact.go
	// RecordInteractionTx) sets PrevCadenceValue only when the contact
	// had a non-empty cadence at emit time. A nil value therefore
	// unambiguously means "no cadence at emit" — the consumer treats
	// it as "no cadence" without a live re-read. This eliminates the
	// Codex-flagged race where a contact gaining cadence between emit
	// and consume would cause the consumer to compute contact_by from
	// the live cadence and manufacture a false divergence against
	// direct's pre-image. External V2 producers must also populate
	// PrevCadenceValue when cadence is set, else they accept the same
	// "treated as no cadence" semantic.
	var cadenceStr string
	if p.PrevCadenceValue != nil {
		cadenceStr = *p.PrevCadenceValue
	}
	hasCadence := cadenceStr != ""

	isManual := p.Source == repository.InteractionSourceManual

	applyLastContacted, applyLastOutreachAt, applyLastResponseAt, directionAllowsContactBy := repository.CadenceApplyFlagsByDirection(p.Direction)

	// applyContactBy AND's direction-permission with the per-call gate
	// (prev.LastContacted vs occurredAt vs manual vs cadence-set). This
	// matches the direct path: outbound never touches contact_by, and
	// inbound/mutual only advance it when the time gate permits.
	applyContactBy := directionAllowsContactBy && repository.ShouldApplyContactBy(prev.LastContacted, p.OccurredAt, isManual, hasCadence)

	// contact_by derivation — same math as the direct path. Parse failure
	// collapses applyContactBy to false so the observation matches
	// direct's effective behavior (parse errors suppress the write).
	var nextContactBy *time.Time
	if applyContactBy {
		cadenceType, err := cadence.ParseCadence(cadenceStr)
		if err != nil {
			applyContactBy = false
		} else {
			t := cadence.CalculateContactBy(p.OccurredAt, cadenceType)
			nextContactBy = &t
		}
	}

	branch := repository.CadenceShadowBranchForward
	if isManual {
		branch = repository.CadenceShadowBranchUnconditional
	}

	obs := repository.CadenceShadowObservation{
		EventID:             env.ID,
		ContactID:           p.ContactID,
		Source:              p.Source,
		Direction:           p.Direction,
		Branch:              branch,
		OccurredAt:          p.OccurredAt,
		PrevLastContacted:   prev.LastContacted,
		PrevLastOutreachAt:  prev.LastOutreachAt,
		PrevLastResponseAt:  prev.LastResponseAt,
		PrevContactBy:       prev.ContactBy,
		ApplyLastContacted:  applyLastContacted,
		ApplyLastOutreachAt: applyLastOutreachAt,
		ApplyLastResponseAt: applyLastResponseAt,
		ApplyContactBy:      applyContactBy,
	}
	// Compute next_* values per branch. Apply-flag-false columns stay nil
	// — the shadow row encodes "consumer would not touch this column."
	if applyLastContacted {
		t := computeNext(isManual, prev.LastContacted, p.OccurredAt)
		obs.NextLastContacted = &t
	}
	if applyLastOutreachAt {
		t := computeNext(isManual, prev.LastOutreachAt, p.OccurredAt)
		obs.NextLastOutreachAt = &t
	}
	if applyLastResponseAt {
		t := computeNext(isManual, prev.LastResponseAt, p.OccurredAt)
		obs.NextLastResponseAt = &t
	}
	if applyContactBy {
		// contact_by is written UNCONDITIONALLY when the apply-flag is
		// true — mirrors today's UpdateContactResponseFields /
		// UpdateContactMutualFields SQL, which has no forward-only guard
		// on contact_by (only on last_contacted / last_outreach_at /
		// last_response_at). PR 8's plan-spec'd UpdateContactCadenceForward
		// adds a forward guard on contact_by too; PR 7's job is bit-for-
		// bit parity with today's direct path, so we preserve the
		// unconditional write here. See plan Decision 3.
		obs.NextContactBy = nextContactBy
	}

	if err := h.cadenceShadowRepo.RecordConsumer(ctx, tx, obs); err != nil {
		return fmt.Errorf("record consumer cadence observation: %w", err)
	}

	// Inline divergence logger (advisory; post-bake query is authoritative).
	direct, err := h.cadenceShadowRepo.FindMatchingDirect(ctx, tx, env.ID)
	if err != nil {
		// Non-fatal — log and continue. The post-bake query catches this
		// too; inline failure shouldn't strand the consumer's observation.
		logger.Warn().Err(err).
			Str("event_id", env.ID.String()).
			Msg("cadence_updater: inline divergence lookup failed")
		return nil
	}
	if direct == nil {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Msg("cadence_updater: direct-path observation not yet written; post-bake query is authoritative")
		return nil
	}
	if divergesNext(direct, &obs) {
		logger.Error().
			Str("event_id", env.ID.String()).
			Str("contact_id", p.ContactID.String()).
			Str("direction", p.Direction).
			Str("source", p.Source).
			Msg("cadence_updater: cadence divergence detected")
	}
	return nil
}

// computeNext returns the branch-appropriate next value for a single
// cadence column. Manual-source events take the unconditional branch
// (incoming wins unconditionally); automated sources take the forward-
// only branch (max(prev, incoming)). Plan Decision 4 forwardMax parity.
func computeNext(isManual bool, prev *time.Time, incoming time.Time) time.Time {
	if isManual {
		return incoming
	}
	return repository.ForwardMax(prev, incoming)
}

// divergesNext returns true when any of the four cadence columns'
// next_* values differ between the direct and consumer observations.
// Uses IS DISTINCT FROM semantics (nil-nil is equal; nil-value differ).
func divergesNext(direct, consumer *repository.CadenceShadowObservation) bool {
	return direct.Branch != consumer.Branch ||
		!timePtrEq(direct.NextLastContacted, consumer.NextLastContacted) ||
		!timePtrEq(direct.NextLastOutreachAt, consumer.NextLastOutreachAt) ||
		!timePtrEq(direct.NextLastResponseAt, consumer.NextLastResponseAt) ||
		!timePtrEq(direct.NextContactBy, consumer.NextContactBy)
}

// timePtrEq compares two *time.Time for value equality; nil-nil is true.
// Compares via Equal (time-zone aware) to avoid UTC-vs-local mismatches
// under the round-trip through pgtype.
func timePtrEq(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}

// --------------------------------------------------------------------------
// River worker wrapper.
// --------------------------------------------------------------------------

// CadenceUpdaterWorker is the river worker that dispatches queued
// CadenceUpdaterJobArgs to CadenceUpdater.HandleEvent.
type CadenceUpdaterWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
	bus     eventBusTx
	pool    *pgxpool.Pool
	handler *CadenceUpdater
}

// NewCadenceUpdaterWorker wires the worker to the concrete bus, the
// application pgxpool, and the consumer instance.
func NewCadenceUpdaterWorker(bus eventBusTx, pool *pgxpool.Pool, handler *CadenceUpdater) *CadenceUpdaterWorker {
	return &CadenceUpdaterWorker{
		bus:     bus,
		pool:    pool,
		handler: handler,
	}
}

// Work implements river.Worker. Fetches the event envelope by id, opens
// a fresh tx, and invokes HandleEvent. On error river will retry per
// MaxAttempts (5, set at the enqueue site in events.consumerJobsForKind).
func (w *CadenceUpdaterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.handler.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds how long a single worker invocation can run. The
// cadence consumer does one SELECT (fallback cadence) + one INSERT +
// one SELECT (inline divergence logger); well under a second on the Pi.
// 30s is ample headroom for pool saturation.
func (*CadenceUpdaterWorker) Timeout(*river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}

// CadenceModeFromConfig narrows the config string to one of the three
// valid mode names. Unknown values fall back to shadow with a log —
// config.Validate should have rejected them at startup, but defend
// against test-constructed configs anyway.
func CadenceModeFromConfig(mode string) string {
	switch mode {
	case config.EventBusCadenceModeOff:
		return CadenceModeOff
	case config.EventBusCadenceModeShadow:
		return CadenceModeShadow
	case config.EventBusCadenceModeCutover:
		return CadenceModeCutover
	default:
		logger.Warn().
			Str("mode", mode).
			Msg("cadence_updater: unknown mode; defaulting to shadow")
		return CadenceModeShadow
	}
}
