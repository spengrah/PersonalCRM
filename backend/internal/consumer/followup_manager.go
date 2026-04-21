package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// FollowUpMode values mirror config.EventBusFollowUpMode*. Duplicated
// here so non-config callers don't import config just to name a mode.
const (
	FollowUpModeOff     = "off"
	FollowUpModeShadow  = "shadow"
	FollowUpModeCutover = "cutover"
)

// contactTaskReader is the subset of ContactTaskRepository the consumer
// depends on for guard 3 (duplicate-pending check).
type contactTaskReader interface {
	FindPendingFollowUpTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) (*repository.ContactTask, error)
}

// interactionResponseReader is the subset of InteractionRepository the
// consumer depends on for guard 2 (out-of-order delivery check).
type interactionResponseReader interface {
	HasResponseAfterTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, outreachAt time.Time) (bool, error)
}

// followUpShadowWriter is the subset of
// FollowUpShadowObservationRepository the consumer depends on. Narrow
// interface so unit tests stub the writer without a DB.
type followUpShadowWriter interface {
	RecordConsumer(ctx context.Context, tx pgx.Tx, obs repository.FollowUpShadowObservation) error
	FindMatchingDirect(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*repository.FollowUpShadowObservation, error)
}

// FollowUpManager is the event-bus consumer for follow-up task
// management. In shadow mode, it observes the action it would take for
// each interaction.recorded envelope and writes a row to
// event_shadow_followup_observation; it never writes to contact_task or
// calls Todoist. In cutover mode (later PR) the same HandleEvent path
// performs the authoritative two-step create.
type FollowUpManager struct {
	mode            string
	taskRepo        contactTaskReader
	interactionRepo interactionResponseReader
	shadowRepo      followUpShadowWriter
	watchdog        config.WatchdogConfig
}

// NewFollowUpManager constructs the consumer. mode must be one of
// FollowUpMode*; unknown values are treated as off so a misconfigured
// consumer never writes shadow rows.
func NewFollowUpManager(
	mode string,
	taskRepo contactTaskReader,
	interactionRepo interactionResponseReader,
	shadowRepo followUpShadowWriter,
	watchdog config.WatchdogConfig,
) *FollowUpManager {
	return &FollowUpManager{
		mode:            mode,
		taskRepo:        taskRepo,
		interactionRepo: interactionRepo,
		shadowRepo:      shadowRepo,
		watchdog:        watchdog,
	}
}

// HandleEvent processes an interaction.recorded envelope. In mode=off
// returns nil immediately. In mode=shadow, evaluates the three guards,
// computes the action + would_* values, and writes the consumer
// observation row. In mode=cutover it would perform the authoritative
// work — that branch returns an error in this shadow implementation
// because cutover code is not yet in place.
func (h *FollowUpManager) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("followup_manager: nil envelope")
	}
	if tx == nil {
		return errors.New("followup_manager: nil tx")
	}

	if h.mode == FollowUpModeOff {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Msg("followup_manager: mode=off; skipping")
		return nil
	}

	if h.mode == FollowUpModeCutover {
		return fmt.Errorf("followup_manager: mode=cutover not implemented; use shadow")
	}

	var p events.InteractionRecordedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return fmt.Errorf("unmarshal interaction.recorded payload: %w", err)
	}
	if p.Version != 2 {
		logger.Error().
			Str("event_id", env.ID.String()).
			Int("version", p.Version).
			Msg("followup_manager: rejecting interaction.recorded payload with Version != 2; external producers must emit V2")
		return nil
	}

	var cadenceStr string
	if p.PrevCadenceValue != nil {
		cadenceStr = *p.PrevCadenceValue
	}

	switch p.Direction {
	case repository.InteractionDirectionOutbound:
		return h.handleOutbound(ctx, tx, env, p, cadenceStr)
	case repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
		return h.handleInboundMutual(ctx, tx, env, p)
	default:
		logger.Debug().
			Str("event_id", env.ID.String()).
			Str("direction", p.Direction).
			Msg("followup_manager: unknown direction; skipping")
		return nil
	}
}

// handleOutbound runs the three skip guards, then records
// create / refresh / skip. In shadow mode this is pure observation;
// cutover would also perform the authoritative writes.
func (h *FollowUpManager) handleOutbound(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, cadenceStr string,
) error {
	// Guard 0 (implicit): no cadence → no follow-up. Matches direct-path
	// behavior (FollowUpService.CreateOrRefreshFollowUp early-returns
	// when contact.Cadence is nil).
	days := watchdogDaysForCadenceStr(cadenceStr, h.watchdog)
	if days == 0 {
		return h.recordSkip(ctx, tx, env, p, "")
	}

	// Guard 1: backdated outbound. Manual interactions are exempt so a
	// user recording a stale outbound still gets a follow-up.
	isManual := p.Source == repository.InteractionSourceManual
	if !isManual {
		cutoff := time.Duration(days) * 24 * time.Hour
		if accelerated.GetCurrentTime().Sub(p.OccurredAt) > cutoff {
			return h.recordSkip(ctx, tx, env, p, repository.FollowUpSkipReasonBackdated)
		}
	}

	// Guard 2: out-of-order delivery. If a later inbound/mutual
	// interaction is already on record, creating a follow-up now would
	// be stale.
	hasResp, err := h.interactionRepo.HasResponseAfterTx(ctx, tx, p.ContactID, p.OccurredAt)
	if err != nil {
		return fmt.Errorf("has-response check: %w", err)
	}
	if hasResp {
		return h.recordSkip(ctx, tx, env, p, repository.FollowUpSkipReasonOutOfOrder)
	}

	// Guard 3: existing pending follow-up → refresh (mirrors direct
	// path's behavior). The duplicate_pending skip reason is declared
	// in the CHECK constraint for forward-compatibility with a future
	// product decision to skip-rather-than-refresh, but is never
	// written in shadow mode.
	pending, err := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("find pending follow-up: %w", err)
	}
	if pending != nil {
		return h.recordRefresh(ctx, tx, env, p, days)
	}

	return h.recordCreate(ctx, tx, env, p, days)
}

// handleInboundMutual records a complete action when a pending
// follow-up exists, otherwise a no-pending skip (skip_reason NULL).
func (h *FollowUpManager) handleInboundMutual(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload,
) error {
	pending, err := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("find pending follow-up for complete: %w", err)
	}
	if pending == nil {
		return h.recordSkip(ctx, tx, env, p, "")
	}
	return h.recordComplete(ctx, tx, env, p)
}

// recordCreate records a create-action observation. The consumer never
// actually calls Todoist or inserts a contact_task row in shadow; this
// method only writes the shadow row.
func (h *FollowUpManager) recordCreate(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, days int,
) error {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)
	idemKey := buildFollowUpIdempotencyKey(p.ContactID, p.OccurredAt)
	obs := repository.FollowUpShadowObservation{
		EventID:             env.ID,
		ContactID:           p.ContactID,
		Source:              p.Source,
		Direction:           p.Direction,
		OccurredAt:          p.OccurredAt,
		Action:              repository.FollowUpActionCreate,
		WouldIdempotencyKey: &idemKey,
		WouldDeadline:       &deadline,
	}
	return h.shadowRepo.RecordConsumer(ctx, tx, obs)
}

// recordRefresh records a refresh-action observation. Does not populate
// would_idempotency_key (refresh reuses the existing task's key).
func (h *FollowUpManager) recordRefresh(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, days int,
) error {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)
	obs := repository.FollowUpShadowObservation{
		EventID:       env.ID,
		ContactID:     p.ContactID,
		Source:        p.Source,
		Direction:     p.Direction,
		OccurredAt:    p.OccurredAt,
		Action:        repository.FollowUpActionRefresh,
		WouldDeadline: &deadline,
	}
	return h.shadowRepo.RecordConsumer(ctx, tx, obs)
}

// recordComplete records a complete-action observation for an
// inbound/mutual event that found a pending follow-up.
func (h *FollowUpManager) recordComplete(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload,
) error {
	obs := repository.FollowUpShadowObservation{
		EventID:    env.ID,
		ContactID:  p.ContactID,
		Source:     p.Source,
		Direction:  p.Direction,
		OccurredAt: p.OccurredAt,
		Action:     repository.FollowUpActionComplete,
	}
	return h.shadowRepo.RecordConsumer(ctx, tx, obs)
}

// recordSkip records a skip-action observation with the given reason.
// An empty reason maps to NULL in the DB (no-cadence / no-pending
// skips are not guard-class).
func (h *FollowUpManager) recordSkip(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, reason string,
) error {
	obs := repository.FollowUpShadowObservation{
		EventID:    env.ID,
		ContactID:  p.ContactID,
		Source:     p.Source,
		Direction:  p.Direction,
		OccurredAt: p.OccurredAt,
		Action:     repository.FollowUpActionSkip,
		SkipReason: reason,
	}
	return h.shadowRepo.RecordConsumer(ctx, tx, obs)
}

// buildFollowUpIdempotencyKey derives the deterministic local idempotency
// key the cutover consumer would use for step-1 follow-up inserts. Format:
// sha256(contact_id || "|" || occurred_at.RFC3339Nano || "|" || "follow_up")
// → hex. Stable across runs: same inputs → same key.
func buildFollowUpIdempotencyKey(contactID uuid.UUID, occurredAt time.Time) string {
	h := sha256.New()
	h.Write([]byte(contactID.String()))
	h.Write([]byte{'|'})
	h.Write([]byte(occurredAt.UTC().Format(time.RFC3339Nano)))
	h.Write([]byte{'|'})
	h.Write([]byte("follow_up"))
	return hex.EncodeToString(h.Sum(nil))
}

// watchdogDaysForCadenceStr returns the follow-up watchdog window in
// days for a cadence string. Unknown / empty returns 0 (no follow-up).
// Mirrors service.watchdogDaysForCadence but is local to the consumer
// package so the consumer doesn't import service.
func watchdogDaysForCadenceStr(cadenceStr string, cfg config.WatchdogConfig) int {
	switch cadenceStr {
	case "weekly":
		return cfg.WeeklyDays
	case "biweekly":
		return cfg.BiweeklyDays
	case "monthly":
		return cfg.MonthlyDays
	case "quarterly":
		return cfg.QuarterlyDays
	case "biannual":
		return cfg.BiannualDays
	case "annual":
		return cfg.AnnualDays
	default:
		return 0
	}
}

// --------------------------------------------------------------------------
// River worker wrapper.
// --------------------------------------------------------------------------

// FollowUpManagerWorker is the river worker that dispatches queued
// FollowUpManagerJobArgs to FollowUpManager.HandleEvent.
type FollowUpManagerWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
	bus     eventBusTx
	pool    *pgxpool.Pool
	handler *FollowUpManager
}

// NewFollowUpManagerWorker constructs the river worker.
func NewFollowUpManagerWorker(bus eventBusTx, pool *pgxpool.Pool, handler *FollowUpManager) *FollowUpManagerWorker {
	return &FollowUpManagerWorker{
		bus:     bus,
		pool:    pool,
		handler: handler,
	}
}

// Work fetches the event and dispatches HandleEvent inside a fresh tx.
func (w *FollowUpManagerWorker) Work(ctx context.Context, j *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.handler.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds the per-job runtime. Shadow work is quick (a read + a
// single insert); 30s is generous headroom matching CadenceUpdater.
func (*FollowUpManagerWorker) Timeout(*river.Job[consumerjobs.FollowUpManagerJobArgs]) time.Duration {
	return 30 * time.Second
}

// FollowUpModeFromConfig narrows the config string to one of the three
// valid mode names. Unknown values fall back to off with an ERROR log.
func FollowUpModeFromConfig(mode string) string {
	switch mode {
	case config.EventBusFollowUpModeOff:
		return FollowUpModeOff
	case config.EventBusFollowUpModeShadow:
		return FollowUpModeShadow
	case config.EventBusFollowUpModeCutover:
		return FollowUpModeCutover
	default:
		logger.Error().
			Str("mode", mode).
			Msg("followup_manager: unknown mode; defaulting to off")
		return FollowUpModeOff
	}
}
