package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"personal-crm/backend/internal/todoist"

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

// ErrTodoistUnconfigured is the sentinel the TodoistSettingsFunc returns
// when Todoist is not wired (no account, no settings, missing label).
// The consumer treats it as a non-fatal skip so interaction recording
// doesn't roll back when the user has disabled Todoist integration.
// Matches the direct-path's best-effort degradation behavior — the
// local cadence advance still happens; only the follow-up Todoist task
// is skipped.
var ErrTodoistUnconfigured = errors.New("todoist integration not configured")

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

// followUpContactReader reads contact rows inside the caller's tx. Used
// by the cutover path to resolve cadence + full name for the Todoist
// task shape, and to synthesize the payload in ApplyInteraction direct
// invocations.
type followUpContactReader interface {
	GetContactTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.Contact, error)
}

// followUpTaskWriter collects the contact_task mutation surface the
// cutover consumer needs. Tx-threaded throughout so the consumer's
// writes commit atomically with its claim + shadow-obs writes.
type followUpTaskWriter interface {
	CreateContactTaskTx(ctx context.Context, tx pgx.Tx, req repository.CreateContactTaskRequest, idempotencyKey *string) (*repository.ContactTask, error)
	UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state repository.ContactTaskState) (*repository.ContactTask, error)
	UpdateContactTaskMetadataTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error)
	UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) (*repository.ContactTask, error)
	SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error
	GetContactTaskByIdempotencyKeyTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, kind, key string) (*repository.ContactTask, error)
	GetContactTaskTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ContactTask, error)
}

// RiverInserter mirrors the repository.JobEnqueuer surface the consumer
// needs. Kept as a local alias so test doubles don't need to import the
// repository package graph.
type RiverInserter = repository.JobEnqueuer

// TodoistSettingsFunc resolves Todoist settings + access token for a
// given request. Matches the shape of the deleted FollowUpService's
// settings func so main.go can wire it through OAuthService as before.
type TodoistSettingsFunc func(ctx context.Context) (*todoist.Settings, string, error)

// FollowUpHandler is the subset of FollowUpManager used by
// InteractionRecorder's inline invoke. Returns a post-commit closure
// (may be nil) carrying the refresh path's Todoist item_update call
// out of the caller's tx (core.md rule 153: no external HTTP inside
// a pgx.Tx).
type FollowUpHandler interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (postCommit func(context.Context), err error)
}

// FollowUpManager is the event-bus consumer for follow-up task
// management. In cutover mode it is the sole writer of the
// contact_task.kind='follow_up' lifecycle: three-guard skip logic,
// two-step crash-safe create (pending_remote_create → managed once the
// Todoist item_add succeeds), refresh with post-commit Todoist call +
// river-retry fallback, and complete with a river-retried
// TodoistFollowUpCloseJob.
type FollowUpManager struct {
	mode            string
	claims          eventClaimer
	contacts        followUpContactReader
	taskRepo        contactTaskReader
	taskWriter      followUpTaskWriter
	interactionRepo interactionResponseReader
	shadowRepo      followUpShadowWriter
	riverInserter   RiverInserter
	// pool is used only by the refresh post-commit closure to open a
	// short tx for enqueueing TodoistFollowUpRefreshJob when the inline
	// item_update attempt fails. Nil is legal in unit tests that don't
	// exercise the post-commit retry path.
	pool          *pgxpool.Pool
	settings      TodoistSettingsFunc
	clientFactory todoist.ClientFactory
	frontendURL   string
	watchdog      config.WatchdogConfig
}

// NewFollowUpManager constructs the consumer. mode must be one of
// FollowUpMode*; unknown values are treated as off so a misconfigured
// consumer never writes shadow rows. The cutover-only dependencies
// (claims, contacts, taskWriter, riverInserter, settings,
// clientFactory, frontendURL) may be nil in off/shadow tests — their
// use is gated on mode == cutover.
func NewFollowUpManager(
	mode string,
	claims eventClaimer,
	contacts followUpContactReader,
	taskRepo contactTaskReader,
	taskWriter followUpTaskWriter,
	interactionRepo interactionResponseReader,
	shadowRepo followUpShadowWriter,
	riverInserter RiverInserter,
	pool *pgxpool.Pool,
	settings TodoistSettingsFunc,
	clientFactory todoist.ClientFactory,
	frontendURL string,
	watchdog config.WatchdogConfig,
) *FollowUpManager {
	return &FollowUpManager{
		mode:            mode,
		claims:          claims,
		contacts:        contacts,
		taskRepo:        taskRepo,
		taskWriter:      taskWriter,
		interactionRepo: interactionRepo,
		shadowRepo:      shadowRepo,
		riverInserter:   riverInserter,
		pool:            pool,
		settings:        settings,
		clientFactory:   clientFactory,
		frontendURL:     frontendURL,
		watchdog:        watchdog,
	}
}

// HandleEvent processes an interaction.recorded envelope. In mode=off
// returns nil immediately. In mode=shadow, evaluates the three guards,
// computes the action + would_* values, and writes the consumer
// observation row. In mode=cutover it claims the event via
// event_consumer_claim then performs the authoritative follow-up
// write (create / refresh / complete / skip), enqueuing Todoist
// create/close river jobs as needed.
//
// Returns a post-commit closure on the refresh path (outbound when a
// pending follow-up already exists): the closure calls Todoist
// item_update outside the caller's tx and, on failure, enqueues
// TodoistFollowUpRefreshJob for river-managed retry. All other paths
// return a nil closure.
func (h *FollowUpManager) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (func(context.Context), error) {
	if env == nil {
		return nil, errors.New("followup_manager: nil envelope")
	}
	if tx == nil {
		return nil, errors.New("followup_manager: nil tx")
	}

	if h.mode == FollowUpModeOff {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Msg("followup_manager: mode=off; skipping")
		return nil, nil
	}

	var p events.InteractionRecordedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return nil, fmt.Errorf("unmarshal interaction.recorded payload: %w", err)
	}
	if p.Version != 2 {
		logger.Error().
			Str("event_id", env.ID.String()).
			Int("version", p.Version).
			Msg("followup_manager: rejecting interaction.recorded payload with Version != 2; external producers must emit V2")
		return nil, nil
	}

	var cadenceStr string
	if p.PrevCadenceValue != nil {
		cadenceStr = *p.PrevCadenceValue
	}

	// Durable dedupe across inline + queued delivery in cutover mode.
	// Shadow mode doesn't claim because its writes are best-effort and
	// the observation table itself tolerates duplicates via its unique
	// index. Whoever wins the claim runs the write; the loser returns
	// nil. Claim + write commit atomically in the caller's tx.
	if h.mode == FollowUpModeCutover {
		if h.claims == nil {
			return nil, errors.New("followup_manager: cutover requires a claim repository")
		}
		claimed, err := h.claims.TryClaimTx(ctx, tx, env.ID, repository.EventConsumerFollowUpManager)
		if err != nil {
			return nil, fmt.Errorf("claim event for followup_manager: %w", err)
		}
		if !claimed {
			logger.Debug().
				Str("event_id", env.ID.String()).
				Msg("followup_manager: event already claimed by another path; no-op")
			return nil, nil
		}
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
		return nil, nil
	}
}

// ApplyInteraction is the direct-invoke entry point for
// ContactService's non-bus RecordInteraction wrapper (Todoist
// completion path) + PromoteInteractionToMutual / ExtendInteraction.
// No event is published, so no claim is taken. Synthesizes the payload
// from the contact row (inside the caller's tx) and dispatches to the
// same create / refresh / complete branches the event-driven path
// uses, minus the shadow-observation write (no event_id to key off).
//
// Returns a post-commit closure on the refresh path matching
// HandleEvent's semantics.
func (h *FollowUpManager) ApplyInteraction(ctx context.Context, tx pgx.Tx, req repository.ApplyInteractionRequest) (func(context.Context), error) {
	if tx == nil {
		return nil, errors.New("followup_manager: nil tx")
	}
	if h.mode == FollowUpModeOff {
		return nil, nil
	}
	if h.mode != FollowUpModeCutover {
		// Shadow mode has no direct-invoke surface — the direct path that
		// shadow observes has no events either. Callers on non-cutover
		// branches must hold the direct path themselves until cutover.
		return nil, nil
	}
	if h.contacts == nil {
		return nil, errors.New("followup_manager: apply interaction requires a contact reader")
	}
	contact, err := h.contacts.GetContactTx(ctx, tx, req.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact for apply interaction: %w", err)
	}
	var cadenceStr string
	if contact.Cadence != nil {
		cadenceStr = *contact.Cadence
	}
	payload := events.InteractionRecordedPayload{
		Version:          2,
		ContactID:        req.ContactID,
		Direction:        req.Direction,
		Source:           req.Source,
		OccurredAt:       req.OccurredAt,
		PrevCadenceValue: &cadenceStr,
	}
	switch req.Direction {
	case repository.InteractionDirectionOutbound:
		return h.applyOutbound(ctx, tx, nil, contact, payload, cadenceStr, false)
	case repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
		return h.applyInboundMutual(ctx, tx, nil, payload, false)
	default:
		return nil, nil
	}
}

// handleOutbound is the event-driven entry to the outbound branch. It
// runs the three skip guards and dispatches to applyOutbound.
func (h *FollowUpManager) handleOutbound(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, cadenceStr string,
) (func(context.Context), error) {
	// Guard 0 (implicit): no cadence → no follow-up. Matches direct-path
	// behavior (early-return when contact.Cadence is nil).
	days := watchdogDaysForCadenceStr(cadenceStr, h.watchdog)
	if days == 0 {
		return nil, h.recordSkip(ctx, tx, env, p, "")
	}

	// Guard 1: backdated outbound. Manual interactions are exempt so a
	// user recording a stale outbound still gets a follow-up.
	isManual := p.Source == repository.InteractionSourceManual
	if !isManual {
		cutoff := time.Duration(days) * 24 * time.Hour
		if accelerated.GetCurrentTime().Sub(p.OccurredAt) > cutoff {
			return nil, h.recordSkip(ctx, tx, env, p, repository.FollowUpSkipReasonBackdated)
		}
	}

	// Guard 2: out-of-order delivery. If a later inbound/mutual
	// interaction is already on record, creating a follow-up now would
	// be stale.
	hasResp, err := h.interactionRepo.HasResponseAfterTx(ctx, tx, p.ContactID, p.OccurredAt)
	if err != nil {
		return nil, fmt.Errorf("has-response check: %w", err)
	}
	if hasResp {
		return nil, h.recordSkip(ctx, tx, env, p, repository.FollowUpSkipReasonOutOfOrder)
	}

	// In cutover, the contact reader is needed for the Todoist task
	// shape even in the create branch (full name, contact link); in
	// shadow we don't read the contact.
	var contact *repository.Contact
	if h.mode == FollowUpModeCutover {
		if h.contacts == nil {
			return nil, errors.New("followup_manager: cutover outbound requires a contact reader")
		}
		c, err := h.contacts.GetContactTx(ctx, tx, p.ContactID)
		if err != nil {
			return nil, fmt.Errorf("get contact for outbound: %w", err)
		}
		contact = c
	}

	return h.applyOutbound(ctx, tx, env, contact, p, cadenceStr, true)
}

// applyOutbound dispatches guard 3 (find pending) and then create or
// refresh. env may be nil on the ApplyInteraction path (no shadow
// write). writeShadow controls whether the path writes a consumer
// observation row.
func (h *FollowUpManager) applyOutbound(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	contact *repository.Contact, p events.InteractionRecordedPayload, cadenceStr string, writeShadow bool,
) (func(context.Context), error) {
	days := watchdogDaysForCadenceStr(cadenceStr, h.watchdog)
	if days == 0 {
		// ApplyInteraction short-circuit: direct-invoke callers may get
		// here when the contact has no cadence.
		return nil, nil
	}
	pending, err := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("find pending follow-up: %w", err)
	}
	if pending != nil {
		return h.applyRefresh(ctx, tx, env, pending, p, days, writeShadow)
	}
	return h.applyCreate(ctx, tx, env, contact, p, days, writeShadow)
}

// applyCreate inserts a pending_remote_create row + enqueues the
// TodoistFollowUpCreateJob (cutover) or writes a create shadow
// observation (shadow).
func (h *FollowUpManager) applyCreate(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	contact *repository.Contact, p events.InteractionRecordedPayload, days int, writeShadow bool,
) (func(context.Context), error) {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)
	idemKey := buildFollowUpIdempotencyKey(p.ContactID, p.OccurredAt)

	if h.mode == FollowUpModeShadow {
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
		return nil, h.shadowRepo.RecordConsumer(ctx, tx, obs)
	}

	// Cutover path. Guard against a prior step-1 insert that raced us
	// (crash-retry of a queued worker, or duplicate inline call). If the
	// idempotency-key row already exists, we have nothing to do: the
	// caller lost the race and the winner's step-2 worker will finalize.
	if existing, err := h.taskWriter.GetContactTaskByIdempotencyKeyTx(ctx, tx, p.ContactID, todoist.TaskKindFollowUp, idemKey); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check idempotency key: %w", err)
		}
	} else if existing != nil {
		logger.Debug().
			Str("event_id", env.ID.String()).
			Str("contact_id", p.ContactID.String()).
			Str("contact_task_id", existing.ID.String()).
			Msg("followup_manager: idempotency key already present; skipping insert")
		return nil, nil
	}

	if contact == nil {
		return nil, errors.New("followup_manager: cutover create requires a contact")
	}

	// Build the Todoist item_add shape at step-1 time so step-2 can
	// reconstruct the command from stored metadata without re-reading
	// contact / settings. Settings may change between step-1 and step-2;
	// snapshotting here locks the user-visible task shape to the state
	// at event-record time.
	settings, _, err := h.settings(ctx)
	if err != nil {
		if errors.Is(err, ErrTodoistUnconfigured) {
			// Todoist not configured — skip follow-up creation without
			// rolling back the interaction write. The local cadence
			// advance still happens; the user sees no follow-up task
			// until they wire Todoist, matching the direct-path's
			// best-effort degradation.
			logger.Warn().
				Str("event_id", env.ID.String()).
				Str("contact_id", p.ContactID.String()).
				Msg("followup_manager: todoist not configured; skipping create")
			if writeShadow && env != nil {
				obs := repository.FollowUpShadowObservation{
					EventID:    env.ID,
					ContactID:  p.ContactID,
					Source:     p.Source,
					Direction:  p.Direction,
					OccurredAt: p.OccurredAt,
					Action:     repository.FollowUpActionSkip,
					SkipReason: repository.FollowUpSkipReasonTodoistUnconfigured,
				}
				if shadowErr := h.shadowRepo.RecordConsumer(ctx, tx, obs); shadowErr != nil {
					return nil, fmt.Errorf("record todoist-unconfigured skip observation: %w", shadowErr)
				}
			}
			return nil, nil
		}
		return nil, fmt.Errorf("get todoist settings for create: %w", err)
	}
	deadlineStr := deadline.Format(todoist.DateFormat)
	contactLink := fmt.Sprintf("%s/contacts/%s", h.frontendURL, p.ContactID.String())
	content := fmt.Sprintf("Follow up: [%s](%s) (awaiting reply)", contact.FullName, contactLink)
	marker := map[string]any{
		"crm":        true,
		"contact_id": p.ContactID.String(),
		"kind":       todoist.TaskKindFollowUp,
		"instance":   settings.IntegrationInstanceID,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return nil, fmt.Errorf("marshal marker: %w", err)
	}

	metadata := map[string]any{
		"due_date":                deadlineStr,
		"content":                 content,
		"marker_json":             string(markerJSON),
		"project_id":              settings.ProjectID,
		"label_name":              settings.LabelName,
		"integration_instance_id": settings.IntegrationInstanceID,
	}

	newTask, err := h.taskWriter.CreateContactTaskTx(ctx, tx, repository.CreateContactTaskRequest{
		ContactID:      p.ContactID,
		Provider:       todoist.SourceName,
		Kind:           todoist.TaskKindFollowUp,
		ExternalTaskID: "",
		State:          string(repository.ContactTaskStatePendingRemoteCreate),
		Metadata:       metadata,
	}, &idemKey)
	if err != nil {
		return nil, fmt.Errorf("insert pending_remote_create row: %w", err)
	}

	if _, err := h.riverInserter.InsertTx(ctx, tx, consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: newTask.ID}, &river.InsertOpts{MaxAttempts: 10}); err != nil {
		return nil, fmt.Errorf("enqueue todoist followup create job: %w", err)
	}

	if writeShadow && env != nil {
		obs := repository.FollowUpShadowObservation{
			EventID:             env.ID,
			ContactID:           p.ContactID,
			Source:              p.Source,
			Direction:           p.Direction,
			OccurredAt:          p.OccurredAt,
			Action:              repository.FollowUpActionCreate,
			WouldIdempotencyKey: &idemKey,
			WouldDeadline:       &deadline,
			DirectContactTaskID: &newTask.ID,
		}
		if err := h.shadowRepo.RecordConsumer(ctx, tx, obs); err != nil {
			return nil, fmt.Errorf("record create shadow observation: %w", err)
		}
	}

	return nil, nil
}

// applyRefresh updates the local metadata deadline in tx (cutover) and
// returns a post-commit closure that calls Todoist item_update + on
// failure enqueues TodoistFollowUpRefreshJob for river-managed retry.
// Shadow mode just writes the observation row.
func (h *FollowUpManager) applyRefresh(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	pending *repository.ContactTask, p events.InteractionRecordedPayload, days int, writeShadow bool,
) (func(context.Context), error) {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)

	if h.mode == FollowUpModeShadow {
		obs := repository.FollowUpShadowObservation{
			EventID:             env.ID,
			ContactID:           p.ContactID,
			Source:              p.Source,
			Direction:           p.Direction,
			OccurredAt:          p.OccurredAt,
			Action:              repository.FollowUpActionRefresh,
			WouldDeadline:       &deadline,
			DirectContactTaskID: &pending.ID,
		}
		return nil, h.shadowRepo.RecordConsumer(ctx, tx, obs)
	}

	// Cutover: update local metadata in tx.
	deadlineStr := deadline.Format(todoist.DateFormat)
	metadata := pending.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	} else {
		copied := make(map[string]any, len(metadata))
		for k, v := range metadata {
			copied[k] = v
		}
		metadata = copied
	}
	metadata["due_date"] = deadlineStr
	if _, err := h.taskWriter.UpdateContactTaskMetadataTx(ctx, tx, pending.ID, metadata); err != nil {
		return nil, fmt.Errorf("refresh local metadata: %w", err)
	}

	if writeShadow && env != nil {
		obs := repository.FollowUpShadowObservation{
			EventID:             env.ID,
			ContactID:           p.ContactID,
			Source:              p.Source,
			Direction:           p.Direction,
			OccurredAt:          p.OccurredAt,
			Action:              repository.FollowUpActionRefresh,
			WouldDeadline:       &deadline,
			DirectContactTaskID: &pending.ID,
		}
		if err := h.shadowRepo.RecordConsumer(ctx, tx, obs); err != nil {
			return nil, fmt.Errorf("record refresh shadow observation: %w", err)
		}
	}

	// Capture externalID so the post-commit closure can issue item_update.
	// If externalID is empty the row is still pending_remote_create — no
	// remote task yet, so no item_update is needed. The create worker's
	// metadata build reads due_date from the same local row.
	externalID := pending.ExternalTaskID
	contactTaskID := pending.ID
	return h.buildRefreshPostCommit(externalID, contactTaskID, deadline), nil
}

// buildRefreshPostCommit returns a closure that calls Todoist
// item_update after the outer tx commits and, on failure, enqueues
// TodoistFollowUpRefreshJob for river-managed retry. externalID may be
// empty, in which case the closure is a no-op (create worker will pick
// up the refreshed metadata on finalize).
func (h *FollowUpManager) buildRefreshPostCommit(externalID string, contactTaskID uuid.UUID, newDeadline time.Time) func(context.Context) {
	if externalID == "" {
		return nil
	}
	return func(postCtx context.Context) {
		deadlineStr := newDeadline.Format(todoist.DateFormat)
		_, accessToken, err := h.settings(postCtx)
		if err != nil {
			if errors.Is(err, ErrTodoistUnconfigured) {
				// Todoist disabled — local deadline is authoritative;
				// a retry would never succeed. Skip silently.
				logger.Debug().
					Str("contact_task_id", contactTaskID.String()).
					Msg("followup_manager: refresh skipped — todoist not configured")
				return
			}
			logger.Warn().Err(err).
				Str("contact_task_id", contactTaskID.String()).
				Msg("followup_manager: refresh post-commit settings lookup failed; enqueuing retry")
			h.enqueueRefreshRetry(postCtx, contactTaskID, newDeadline)
			return
		}
		client := h.clientFactory(accessToken)
		updateCmd := todoist.NewItemUpdateCommand(externalID, map[string]any{
			"deadline": map[string]string{"date": deadlineStr},
		})
		if _, err := client.Sync(postCtx, "*", []string{}, []todoist.SyncCommand{updateCmd}); err != nil {
			logger.Warn().Err(err).
				Str("contact_task_id", contactTaskID.String()).
				Str("external_task_id", externalID).
				Msg("followup_manager: refresh item_update failed post-commit; enqueuing retry")
			h.enqueueRefreshRetry(postCtx, contactTaskID, newDeadline)
			return
		}
	}
}

// enqueueRefreshRetry opens a fresh tx on the pool and enqueues a
// TodoistFollowUpRefreshJob for river-managed retry. Best-effort: if
// enqueue itself fails the only recovery is the user's next outbound
// triggering a fresh flow for the same contact.
func (h *FollowUpManager) enqueueRefreshRetry(ctx context.Context, contactTaskID uuid.UUID, newDeadline time.Time) {
	if h.riverInserter == nil || h.pool == nil {
		logger.Error().
			Str("contact_task_id", contactTaskID.String()).
			Msg("followup_manager: refresh retry enqueue skipped — inserter or pool unwired")
		return
	}
	args := consumerjobs.TodoistFollowUpRefreshJobArgs{
		ContactTaskID: contactTaskID,
		NewDeadline:   newDeadline,
	}
	err := pgx.BeginTxFunc(ctx, h.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, insertErr := h.riverInserter.InsertTx(ctx, tx, args, &river.InsertOpts{MaxAttempts: 10})
		return insertErr
	})
	if err != nil {
		logger.Error().Err(err).
			Str("contact_task_id", contactTaskID.String()).
			Msg("followup_manager: failed to enqueue refresh retry job")
	}
}

// handleInboundMutual handles inbound/mutual direction under the
// cutover rules. Finds any pending follow-up; if present, transitions
// state to 'completed' (in tx) and — under single-owner semantics —
// enqueues TodoistFollowUpCloseJob only when the row was fully
// finalized (state='managed' AND external_task_id != ”). When the
// row is still pending_remote_create the create worker handles the
// close-while-pending race itself (create-then-close).
func (h *FollowUpManager) handleInboundMutual(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload,
) (func(context.Context), error) {
	return h.applyInboundMutual(ctx, tx, env, p, true)
}

// applyInboundMutual shares the inbound/mutual logic between
// HandleEvent and ApplyInteraction. writeShadow=false on direct-invoke.
func (h *FollowUpManager) applyInboundMutual(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	p events.InteractionRecordedPayload, writeShadow bool,
) (func(context.Context), error) {
	pending, err := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("find pending follow-up for complete: %w", err)
	}
	if pending == nil {
		if writeShadow && env != nil {
			return nil, h.recordSkip(ctx, tx, env, p, "")
		}
		return nil, nil
	}

	if h.mode == FollowUpModeShadow {
		obs := repository.FollowUpShadowObservation{
			EventID:             env.ID,
			ContactID:           p.ContactID,
			Source:              p.Source,
			Direction:           p.Direction,
			OccurredAt:          p.OccurredAt,
			Action:              repository.FollowUpActionComplete,
			DirectContactTaskID: &pending.ID,
		}
		return nil, h.shadowRepo.RecordConsumer(ctx, tx, obs)
	}

	// Cutover: transition to completed in tx.
	if _, err := h.taskWriter.UpdateContactTaskStateTx(ctx, tx, pending.ID, repository.ContactTaskStateCompleted); err != nil {
		return nil, fmt.Errorf("mark follow-up completed: %w", err)
	}

	// Single-owner close-job enqueue: only enqueue when the row was
	// fully finalized — i.e. state='managed' AND external_task_id
	// populated. When state was pending_remote_create the create worker
	// handles the race (create-then-close) itself, so enqueuing here
	// would produce a duplicate close.
	if pending.State == repository.ContactTaskStateManaged && pending.ExternalTaskID != "" {
		if _, err := h.riverInserter.InsertTx(ctx, tx, consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: pending.ID}, &river.InsertOpts{MaxAttempts: 10}); err != nil {
			return nil, fmt.Errorf("enqueue todoist followup close job: %w", err)
		}
	}

	if writeShadow && env != nil {
		obs := repository.FollowUpShadowObservation{
			EventID:             env.ID,
			ContactID:           p.ContactID,
			Source:              p.Source,
			Direction:           p.Direction,
			OccurredAt:          p.OccurredAt,
			Action:              repository.FollowUpActionComplete,
			DirectContactTaskID: &pending.ID,
		}
		if err := h.shadowRepo.RecordConsumer(ctx, tx, obs); err != nil {
			return nil, fmt.Errorf("record complete shadow observation: %w", err)
		}
	}
	return nil, nil
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
// key the cutover consumer uses for step-1 follow-up inserts. Format:
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
// After the tx commits, invokes the returned post-commit closure (non-
// nil on the refresh path).
func (w *FollowUpManagerWorker) Work(ctx context.Context, j *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	var postCommit func(context.Context)
	err = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		pc, handleErr := w.handler.HandleEvent(ctx, tx, env)
		if handleErr != nil {
			return handleErr
		}
		postCommit = pc
		return nil
	})
	if err != nil {
		return err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return nil
}

// Timeout bounds the per-job runtime.
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
