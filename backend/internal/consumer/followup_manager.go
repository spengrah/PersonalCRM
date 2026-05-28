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
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// followUpUniqueLiveIndex is the Postgres name of the partial unique
// index over live follow-up rows (see migration 041). A 23505 raised
// against this specific index means another concurrent writer has
// already inserted a pending follow-up for the same contact — recover
// by re-reading the existing row and routing to refresh.
const followUpUniqueLiveIndex = "idx_contact_task_followup_unique_live"

// isFollowUpLiveUniqueViolation returns true when err is a 23505 raised
// against idx_contact_task_followup_unique_live. Scoped narrowly so
// unrelated unique-violation surfaces (e.g. idempotency-key collisions
// from a crash-retried worker, or row-level checks) still propagate.
func isFollowUpLiveUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == followUpUniqueLiveIndex
}

// FollowUpMode values mirror config.EventBusFollowUpMode*. Duplicated
// here so non-config callers don't import config just to name a mode.
//
// ModeOff is the emergency-override for disabling the consumer
// entirely; it is gated behind EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF in
// config.Validate and is retained so rollback from the cutover series
// can silence the consumer without a code change.
const (
	FollowUpModeOff     = "off"
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

// followUpContactReader reads contact rows inside the caller's tx. Used
// by the cutover path to resolve cadence + full name for the Todoist
// task shape, and to synthesize the payload in ApplyInteraction direct
// invocations.
type followUpContactReader interface {
	GetContactTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.Contact, error)
}

// followUpTaskWriter collects the contact_task mutation surface the
// cutover consumer needs. Tx-threaded throughout so the consumer's
// writes commit atomically with its claim.
type followUpTaskWriter interface {
	CreateContactTaskTx(ctx context.Context, tx pgx.Tx, req repository.CreateContactTaskRequest, idempotencyKey *string) (*repository.ContactTask, error)
	UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state repository.ContactTaskState) (*repository.ContactTask, error)
	UpdateContactTaskMetadataTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error)
	UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) (*repository.ContactTask, error)
	SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error
	GetContactTaskByIdempotencyKeyTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, key string) (*repository.ContactTask, error)
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

// Decision is the observer payload emitted at each terminal branch of
// FollowUpManager.HandleEvent / ApplyInteraction. The hook has two
// consumers today:
//
//  1. Unit + integration tests install a DecisionObserver closure to
//     assert on the manager's classification (action, skip reason,
//     would-be deadline, idempotency key, contact_task id) without a
//     DB round trip. Replaces the shadow-observation-table assertions
//     retired with migration 044.
//  2. Operators / future metrics can wire the observer to emit
//     Prometheus counters or a structured audit log without having to
//     modify the manager's branches.
//
// Production wiring leaves the observer nil — the hot path pays only
// a nil check per branch (`emit` is the only call site).
//
// Observer calls are emitted AT the decision point, inside the
// caller's tx. For the refresh/close paths that schedule a post-commit
// Todoist call, a SECOND observer call (with CalledTodoist=true) is
// emitted after the post-commit closure finishes its remote call; the
// closure is best-effort so the second call may never fire if the
// closure never runs (e.g. skipped for empty externalID).
type Decision struct {
	// Action is one of FollowUpActionCreate / Refresh / Complete / Skip.
	Action string
	// SkipReason is set on outbound skip branches that fire one of the
	// three guards (backdated, out_of_order, duplicate_pending). Empty
	// string for non-skip actions and for non-guard-class skips.
	SkipReason string
	// WouldDeadline is the deadline the manager WOULD set for create /
	// refresh actions. Nil for skip / complete and for the post-Todoist
	// second call on refresh (which only reports CalledTodoist).
	WouldDeadline *time.Time
	// WouldIdempotencyKey is the local idempotency key the manager
	// WOULD use for a create branch. Non-nil only on create branches.
	WouldIdempotencyKey *string
	// ContactTaskID is the target contact_task.id for refresh /
	// complete branches, and on create after the row is inserted.
	ContactTaskID *uuid.UUID
	// CalledTodoist is true only on the second observer call for
	// refresh / close branches, reported from the post-commit closure
	// after the Todoist sync API call returns successfully. false on
	// the in-tx first call and on branches that do not schedule a
	// post-commit call.
	CalledTodoist bool
}

// DecisionObserver receives Decision payloads at every terminal branch.
// Production wiring leaves this nil; unit tests install a closure that
// appends to a slice to assert on classification coverage.
type DecisionObserver func(Decision)

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
	// onDecision is an optional test-only hook emitted at every terminal
	// branch. Production wiring leaves it nil for zero cost. See Decision
	// docs above for semantics.
	onDecision DecisionObserver
}

// NewFollowUpManager constructs the consumer. mode must be one of
// FollowUpMode*; unknown values are treated as off so a misconfigured
// consumer never writes. The cutover-only dependencies (claims,
// contacts, taskWriter, riverInserter, settings, clientFactory,
// frontendURL) may be nil in off-mode tests — their use is gated on
// mode == cutover.
func NewFollowUpManager(
	mode string,
	claims eventClaimer,
	contacts followUpContactReader,
	taskRepo contactTaskReader,
	taskWriter followUpTaskWriter,
	interactionRepo interactionResponseReader,
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
		riverInserter:   riverInserter,
		pool:            pool,
		settings:        settings,
		clientFactory:   clientFactory,
		frontendURL:     frontendURL,
		watchdog:        watchdog,
	}
}

// SetDecisionObserver installs a test-only observer. Must be called
// BEFORE the manager is used concurrently; the field is read without
// synchronization on the hot path. Prod wiring must NOT call this.
func (h *FollowUpManager) SetDecisionObserver(obs DecisionObserver) {
	h.onDecision = obs
}

// emit invokes the observer if one is installed. Zero-cost nil check on
// the hot path.
func (h *FollowUpManager) emit(d Decision) {
	if h.onDecision != nil {
		h.onDecision(d)
	}
}

// validateCutoverDeps returns a wrapped error when a required cutover
// dependency is nil. Called at the top of every cutover entry point so
// a misconfigured wiring fails closed with a descriptive error instead
// of panicking inside a create / refresh / complete branch.
func (h *FollowUpManager) validateCutoverDeps() error {
	if h.claims == nil {
		return errors.New("followup_manager: cutover requires a claim repository")
	}
	if h.contacts == nil {
		return errors.New("followup_manager: cutover requires a contact reader")
	}
	if h.taskRepo == nil {
		return errors.New("followup_manager: cutover requires a contact-task reader")
	}
	if h.taskWriter == nil {
		return errors.New("followup_manager: cutover requires a contact-task writer")
	}
	if h.interactionRepo == nil {
		return errors.New("followup_manager: cutover requires an interaction reader")
	}
	if h.riverInserter == nil {
		return errors.New("followup_manager: cutover requires a river inserter")
	}
	if h.settings == nil {
		return errors.New("followup_manager: cutover requires a todoist settings func")
	}
	if h.clientFactory == nil {
		return errors.New("followup_manager: cutover requires a todoist client factory")
	}
	return nil
}

// HandleEvent processes an interaction.recorded envelope. In mode=off
// returns nil immediately. In mode=cutover it claims the event via
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
	if err := h.validateCutoverDeps(); err != nil {
		return nil, err
	}

	var p events.InteractionRecordedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return nil, fmt.Errorf("unmarshal interaction.recorded payload: %w", err)
	}
	if p.Version != 2 && p.Version != 3 {
		logger.Error().
			Str("event_id", env.ID.String()).
			Int("version", p.Version).
			Msg("followup_manager: rejecting interaction.recorded payload with Version not in {2, 3}")
		return nil, nil
	}

	var cadenceStr string
	if p.PrevCadenceValue != nil {
		cadenceStr = *p.PrevCadenceValue
	}

	// Durable dedupe across inline + queued delivery. Whoever wins the
	// claim runs the write; the loser returns nil. Claim + write commit
	// atomically in the caller's tx.
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

	switch p.Direction {
	case repository.InteractionDirectionOutbound:
		return h.handleOutbound(ctx, tx, env, p, cadenceStr)
	case repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
		return h.handleInboundMutual(ctx, tx, p)
	default:
		logger.Debug().
			Str("event_id", envIDString(env)).
			Str("direction", p.Direction).
			Msg("followup_manager: unknown direction; skipping")
		return nil, nil
	}
}

// envIDString returns env.ID as a string or an empty marker when env is
// nil. ApplyInteraction and the other direct-invoke callers hand us a
// nil envelope by design (no event published), so log fields must
// tolerate that rather than dereferencing env.ID.
func envIDString(env *events.Envelope) string {
	if env == nil {
		return "(none)"
	}
	return env.ID.String()
}

// ApplyInteraction is the direct-invoke entry point for
// ContactService's non-bus RecordInteraction wrapper (Todoist
// completion path) + PromoteInteractionToMutual / ExtendInteraction.
// No event is published, so no claim is taken. Synthesizes the payload
// from the contact row (inside the caller's tx) and dispatches to the
// same create / refresh / complete branches the event-driven path
// uses.
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
	if err := h.validateCutoverDeps(); err != nil {
		return nil, err
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
		return h.applyOutbound(ctx, tx, nil, contact, payload, cadenceStr)
	case repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
		return h.applyInboundMutual(ctx, tx, payload)
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
	// SuppressFollowUp short-circuit (V3+). Set true by Todoist provider
	// when the completed task is kind=send. Polarity: zero=do-not-suppress
	// preserves the legacy behavior, so V1/V2 payloads (where the field
	// decodes to false) still spawn a follow-up.
	if p.SuppressFollowUp {
		h.emit(Decision{Action: repository.FollowUpActionSkip, SkipReason: repository.FollowUpSkipReasonSuppressed})
		return nil, nil
	}

	// Guard 0 (implicit): no cadence → no follow-up. Matches direct-path
	// behavior (early-return when contact.Cadence is nil).
	days := watchdogDaysForCadenceStr(cadenceStr, h.watchdog)
	if days == 0 {
		h.emit(Decision{Action: repository.FollowUpActionSkip})
		return nil, nil
	}

	// Guard 1: backdated outbound. Manual interactions are exempt so a
	// user recording a stale outbound still gets a follow-up.
	isManual := p.Source == repository.InteractionSourceManual
	if !isManual {
		cutoff := time.Duration(days) * 24 * time.Hour
		if accelerated.GetCurrentTime().Sub(p.OccurredAt) > cutoff {
			h.emit(Decision{Action: repository.FollowUpActionSkip, SkipReason: repository.FollowUpSkipReasonBackdated})
			return nil, nil
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
		h.emit(Decision{Action: repository.FollowUpActionSkip, SkipReason: repository.FollowUpSkipReasonOutOfOrder})
		return nil, nil
	}

	contact, err := h.contacts.GetContactTx(ctx, tx, p.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact for outbound: %w", err)
	}
	return h.applyOutbound(ctx, tx, env, contact, p, cadenceStr)
}

// applyOutbound dispatches guard 3 (find pending) and then create or
// refresh. env may be nil on the ApplyInteraction path.
func (h *FollowUpManager) applyOutbound(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	contact *repository.Contact, p events.InteractionRecordedPayload, cadenceStr string,
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
		return h.applyRefresh(ctx, tx, pending, p, days)
	}
	return h.applyCreate(ctx, tx, env, contact, p, days)
}

// applyCreate inserts a pending_remote_create row + enqueues the
// TodoistFollowUpCreateJob.
func (h *FollowUpManager) applyCreate(
	ctx context.Context, tx pgx.Tx, env *events.Envelope,
	contact *repository.Contact, p events.InteractionRecordedPayload, days int,
) (func(context.Context), error) {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)
	idemKey := buildFollowUpIdempotencyKey(p.ContactID, p.OccurredAt)

	// Guard against a prior step-1 insert that raced us (crash-retry of
	// a queued worker, or duplicate inline call). If the idempotency-key
	// row already exists, we have nothing to do: the caller lost the
	// race and the winner's step-2 worker will finalize.
	if existing, err := h.taskWriter.GetContactTaskByIdempotencyKeyTx(ctx, tx, p.ContactID, idemKey); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check idempotency key: %w", err)
		}
	} else if existing != nil {
		logger.Debug().
			Str("event_id", envIDString(env)).
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
			// best-effort degradation. A log-only skip is sufficient
			// here: the skip is operationally observable via the WARN
			// log.
			logger.Warn().
				Str("event_id", envIDString(env)).
				Str("contact_id", p.ContactID.String()).
				Msg("followup_manager: todoist not configured; skipping create")
			return nil, nil
		}
		return nil, fmt.Errorf("get todoist settings for create: %w", err)
	}
	deadlineStr := deadline.Format(todoist.DateFormat)
	contactLink := fmt.Sprintf("%s/contacts/%s", h.frontendURL, p.ContactID.String())
	content := fmt.Sprintf("Follow up: [%s](%s) (awaiting reply)", contact.FullName, contactLink)
	markerJSON, err := contacttask.EncodeMarker(contacttask.CRMMarker{
		ContactID: p.ContactID.String(),
		Kind:      contacttask.KindReachOut,
		Lifecycle: contacttask.LifecycleFollowUpLoop,
		Instance:  settings.IntegrationInstanceID,
	})
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

	// Wrap the insert in a savepoint so a concurrent-writer collision on
	// idx_contact_task_followup_unique_live can be recovered without
	// aborting the outer interaction tx. Two outbound events for the
	// same contact can both pass guard 3 (FindPendingFollowUpTx) and
	// then race the insert — the loser's 23505 must be treated as "the
	// other writer created the pending follow-up" and re-dispatched as a
	// refresh, not bubbled up as an error.
	sp, spErr := tx.Begin(ctx)
	if spErr != nil {
		return nil, fmt.Errorf("open savepoint for follow-up insert: %w", spErr)
	}
	newTask, err := h.taskWriter.CreateContactTaskTx(ctx, sp, repository.CreateContactTaskRequest{
		ContactID:      p.ContactID,
		Provider:       todoist.SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
		ExternalTaskID: "",
		State:          string(repository.ContactTaskStatePendingRemoteCreate),
		Metadata:       metadata,
	}, &idemKey)
	if err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return nil, fmt.Errorf("rollback follow-up savepoint: %w", rbErr)
		}
		if isFollowUpLiveUniqueViolation(err) {
			// Another tx inserted the pending follow-up for this contact
			// between our guard-3 read and our insert. Re-read the winner's
			// row inside the outer tx and route to the refresh branch so
			// the deadline advances to this event's occurred_at.
			existing, findErr := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
			if findErr != nil && !errors.Is(findErr, db.ErrNotFound) {
				return nil, fmt.Errorf("re-read pending follow-up after unique-violation: %w", findErr)
			}
			if existing == nil {
				// Winner's row no longer live (e.g. already completed). Nothing
				// to refresh; skip silently without failing the outer tx.
				logger.Debug().
					Str("event_id", envIDString(env)).
					Str("contact_id", p.ContactID.String()).
					Msg("followup_manager: unique-violation but no live row on re-read; skipping")
				return nil, nil
			}
			logger.Debug().
				Str("event_id", envIDString(env)).
				Str("contact_id", p.ContactID.String()).
				Str("contact_task_id", existing.ID.String()).
				Msg("followup_manager: concurrent insert detected; routing to refresh")
			return h.applyRefresh(ctx, tx, existing, p, days)
		}
		return nil, fmt.Errorf("insert pending_remote_create row: %w", err)
	}
	if commitErr := sp.Commit(ctx); commitErr != nil {
		return nil, fmt.Errorf("commit follow-up savepoint: %w", commitErr)
	}

	if _, err := h.riverInserter.InsertTx(ctx, tx, consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: newTask.ID}, &river.InsertOpts{MaxAttempts: 10}); err != nil {
		return nil, fmt.Errorf("enqueue todoist followup create job: %w", err)
	}

	taskID := newTask.ID
	h.emit(Decision{
		Action:              repository.FollowUpActionCreate,
		WouldDeadline:       &deadline,
		WouldIdempotencyKey: &idemKey,
		ContactTaskID:       &taskID,
	})
	return nil, nil
}

// applyRefresh updates the local metadata deadline in tx and returns a
// post-commit closure that calls Todoist item_update + on failure
// enqueues TodoistFollowUpRefreshJob for river-managed retry.
func (h *FollowUpManager) applyRefresh(
	ctx context.Context, tx pgx.Tx,
	pending *repository.ContactTask, p events.InteractionRecordedPayload, days int,
) (func(context.Context), error) {
	deadline := cadence.Today(p.OccurredAt).AddDate(0, 0, days)

	// Update local metadata in tx.
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

	taskID := pending.ID
	h.emit(Decision{
		Action:        repository.FollowUpActionRefresh,
		WouldDeadline: &deadline,
		ContactTaskID: &taskID,
	})

	// Capture externalID so the post-commit closure can issue item_update.
	// If externalID is empty the row is still pending_remote_create — no
	// remote task yet, so no item_update is needed. The create worker's
	// metadata build reads due_date from the same local row.
	externalID := pending.ExternalTaskID
	return h.buildRefreshPostCommit(externalID, taskID, deadline), nil
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
		// Second observer call: remote call succeeded. WouldDeadline is
		// deliberately nil — the first observer call (inside the tx)
		// already reported the deadline; this one is purely a "Todoist
		// side of the world caught up" signal.
		id := contactTaskID
		h.emit(Decision{
			Action:        repository.FollowUpActionRefresh,
			ContactTaskID: &id,
			CalledTodoist: true,
		})
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

// handleInboundMutual handles inbound/mutual direction. Finds any
// pending follow-up; if present, transitions state to 'completed' (in
// tx) and — under single-owner semantics — enqueues
// TodoistFollowUpCloseJob only when the row was fully finalized
// (state='managed' AND external_task_id != ”). When the row is still
// pending_remote_create the create worker handles the
// close-while-pending race itself (create-then-close).
func (h *FollowUpManager) handleInboundMutual(
	ctx context.Context, tx pgx.Tx, p events.InteractionRecordedPayload,
) (func(context.Context), error) {
	return h.applyInboundMutual(ctx, tx, p)
}

// applyInboundMutual shares the inbound/mutual logic between
// HandleEvent and ApplyInteraction.
func (h *FollowUpManager) applyInboundMutual(
	ctx context.Context, tx pgx.Tx, p events.InteractionRecordedPayload,
) (func(context.Context), error) {
	pending, err := h.taskRepo.FindPendingFollowUpTx(ctx, tx, p.ContactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("find pending follow-up for complete: %w", err)
	}
	if pending == nil {
		h.emit(Decision{Action: repository.FollowUpActionSkip})
		return nil, nil
	}

	// Transition to completed in tx. Use the RETURNING row so the
	// close-enqueue decision reads post-update state. The create worker
	// can finalize (pending_remote_create → managed + external task id)
	// between our guard-3 read and this update; reading the pre-update
	// snapshot strands the close when that race fires.
	updated, err := h.taskWriter.UpdateContactTaskStateTx(ctx, tx, pending.ID, repository.ContactTaskStateCompleted)
	if err != nil {
		return nil, fmt.Errorf("mark follow-up completed: %w", err)
	}

	// Single-owner close-job enqueue: only enqueue when the row carries a
	// Todoist external_task_id (i.e. the create worker finished before we
	// completed). An empty external_task_id means the row is still
	// pending_remote_create remotely; the create worker handles the
	// close-while-pending race itself (create-then-close), so enqueuing
	// here would produce a duplicate close.
	if updated.ExternalTaskID != "" {
		if _, err := h.riverInserter.InsertTx(ctx, tx, consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: updated.ID}, &river.InsertOpts{MaxAttempts: 10}); err != nil {
			return nil, fmt.Errorf("enqueue todoist followup close job: %w", err)
		}
	}

	taskID := pending.ID
	h.emit(Decision{
		Action:        repository.FollowUpActionComplete,
		ContactTaskID: &taskID,
	})
	return nil, nil
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

// FollowUpModeFromConfig narrows the config string to one of the two
// valid mode names. Unknown values fall back to off with an ERROR log.
func FollowUpModeFromConfig(mode string) string {
	switch mode {
	case config.EventBusFollowUpModeOff:
		return FollowUpModeOff
	case config.EventBusFollowUpModeCutover:
		return FollowUpModeCutover
	default:
		logger.Error().
			Str("mode", mode).
			Msg("followup_manager: unknown mode; defaulting to off")
		return FollowUpModeOff
	}
}
