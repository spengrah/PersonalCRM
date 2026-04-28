package todoist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// SourceName is the source identifier for Todoist sync
	SourceName = "todoist"
	// DefaultSyncInterval is the default sync interval (5 minutes)
	DefaultSyncInterval = 5 * time.Minute
)

// Settings keys stored in sync state metadata
const (
	MetadataKeyProjectID           = "project_id"
	MetadataKeyLabelID             = "label_id"
	MetadataKeyLabelName           = "label_name"
	MetadataKeyProjectName         = "project_name"
	MetadataKeyIntegrationInstance = "integration_instance_id"
	MetadataKeyUserTimezone        = "user_timezone"
)

// Contact task metadata keys
const (
	// MetadataKeyPendingTempID stores the temp ID used when creating a Todoist task,
	// before the real ID is returned from the API.
	MetadataKeyPendingTempID = "pending_temp_id"
	// MetadataKeySyncedDeadline stores the deadline (YYYY-MM-DD) that was last synced
	// to Todoist. Used to detect when contact_by changes in the CRM and the Todoist
	// task needs to be updated.
	MetadataKeySyncedDeadline = "synced_deadline"
	// MetadataKeySyncedLastContacted stores the last_contacted timestamp (RFC3339) at the
	// time the task was created or last synced. Used to detect when a contact is marked
	// as contacted from a non-Todoist source (e.g., calendar sync), even when contact_by
	// doesn't change (same cadence period).
	MetadataKeySyncedLastContacted = "synced_last_contacted"
	// MetadataKeySyncedLastOutreachAt stores the last_outreach_at timestamp (RFC3339) at
	// the time the task was created or last synced. Used to detect when a contact is
	// reached out to from a non-Todoist source (e.g., Telegram), so the cadence task
	// (outreach reminder) can be closed.
	MetadataKeySyncedLastOutreachAt = "synced_last_outreach_at"
)

// DateFormat is the date format used for Todoist deadlines and synced_deadline metadata (YYYY-MM-DD)
const DateFormat = "2006-01-02"

// eventPublisher is the subset of *events.Bus used by the provider. Defined
// consumer-side so tests can stub without importing the bus.
type eventPublisher interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// oauthTokenProvider is the subset of *OAuthService the provider uses at
// runtime. Defined as an interface so tests can stub access-token lookup
// without constructing a real OAuth account.
type oauthTokenProvider interface {
	GetAccessToken(ctx context.Context, accountID string) (string, error)
	HasAnyAccount(ctx context.Context) bool
}

// contactTaskWriter is the subset of *repository.ContactTaskRepository the
// provider uses. Narrow interface so integration tests can wrap the real
// repo with faulty-method injection.
type contactTaskWriter interface {
	GetContactTaskByExternalID(ctx context.Context, provider, externalID string) (*repository.ContactTask, error)
	GetContactTaskByContactCadenceDue(ctx context.Context, contactID uuid.UUID, provider string) (*repository.ContactTask, error)
	GetContactTaskByPendingTempID(ctx context.Context, provider, tempID string) (*repository.ContactTask, error)
	GetContactTask(ctx context.Context, id uuid.UUID) (*repository.ContactTask, error)
	CreateContactTask(ctx context.Context, req repository.CreateContactTaskRequest) (*repository.ContactTask, error)
	DeleteContactTask(ctx context.Context, id uuid.UUID) error
	UpdateContactTaskState(ctx context.Context, id uuid.UUID, state repository.ContactTaskState) (*repository.ContactTask, error)
	UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state repository.ContactTaskState) (*repository.ContactTask, error)
	UpdateContactTaskMetadata(ctx context.Context, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error)
	UpdateContactTaskMetadataTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error)
	UpdateContactTaskExternalID(ctx context.Context, id uuid.UUID, externalID string) (*repository.ContactTask, error)
	UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalID string) (*repository.ContactTask, error)
	FindPendingFollowUp(ctx context.Context, contactID uuid.UUID) (*repository.ContactTask, error)
	ListContactTasksByContact(ctx context.Context, contactID uuid.UUID) ([]repository.ContactTask, error)
}

// contactWriter is the subset of *repository.ContactRepository the provider
// uses.
type contactWriter interface {
	GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error)
	ListContactsWithContactBy(ctx context.Context, limit int32) ([]repository.Contact, error)
}

// cadenceOverrider is the subset of *consumer.CadenceUpdater the provider
// uses to route contact_by writes through the sole writer. Defined
// consumer-side so the todoist package does not import the consumer
// package directly.
type cadenceOverrider interface {
	ApplyContactByOverride(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, contactBy *time.Time) error
}

// CadenceSyncProvider implements SyncProvider for Todoist cadence tasks
type CadenceSyncProvider struct {
	oauthService    oauthTokenProvider
	contactTaskRepo contactTaskWriter
	contactRepo     contactWriter
	cadenceUpdater  cadenceOverrider
	syncRepo        *repository.SyncRepository
	// bus, pool, and clientFactory are required for the tx-atomic handlers
	// and for HTTP-failure injection in tests. Nil values are caught by a
	// single defensive nil-guard at the top of Sync; production wiring in
	// main.go passes non-nil values.
	bus           eventPublisher
	pool          *pgxpool.Pool
	clientFactory ClientFactory
	frontendURL   string
}

// NewCadenceSyncProvider creates a new Todoist cadence sync provider
func NewCadenceSyncProvider(
	oauthService oauthTokenProvider,
	contactTaskRepo contactTaskWriter,
	contactRepo contactWriter,
	syncRepo *repository.SyncRepository,
	cfg *config.Config,
	bus eventPublisher,
	cadenceUpdater cadenceOverrider,
	pool *pgxpool.Pool,
	clientFactory ClientFactory,
) *CadenceSyncProvider {
	return &CadenceSyncProvider{
		oauthService:    oauthService,
		contactTaskRepo: contactTaskRepo,
		contactRepo:     contactRepo,
		cadenceUpdater:  cadenceUpdater,
		syncRepo:        syncRepo,
		bus:             bus,
		pool:            pool,
		clientFactory:   clientFactory,
		frontendURL:     cfg.CORS.FrontendURL,
	}
}

// Config returns the provider's configuration
func (p *CadenceSyncProvider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 SourceName,
		DisplayName:          "Todoist",
		Strategy:             repository.SyncStrategyFetchAll,
		SupportsMultiAccount: false, // v1 supports single account only
		SupportsDiscovery:    false, // No task discovery from Todoist in v1
		DefaultInterval:      DefaultSyncInterval,
	}
}

// Sync performs the Todoist sync for cadence tasks
func (p *CadenceSyncProvider) Sync(
	ctx context.Context,
	state *repository.SyncState,
	contacts []repository.Contact,
) (*sync.SyncResult, error) {
	if p.bus == nil || p.pool == nil || p.clientFactory == nil || p.cadenceUpdater == nil {
		return nil, fmt.Errorf("todoist: bus + pool + clientFactory + cadenceUpdater must be non-nil")
	}

	// Get account ID from state
	if state.AccountID == nil {
		return nil, fmt.Errorf("account ID required for Todoist sync")
	}
	accountID := *state.AccountID

	logger.Info().
		Str("source", SourceName).
		Str("account", accountID).
		Msg("starting Todoist cadence sync")

	// Validate settings are configured
	settings := getSettingsFromMetadata(state.Metadata)
	if settings.ProjectID == "" || settings.LabelID == "" {
		return nil, fmt.Errorf("todoist settings not configured: missing project_id or label_id")
	}

	// Get access token
	accessToken, err := p.oauthService.GetAccessToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// Create sync client
	client := p.clientFactory(accessToken)

	result := &sync.SyncResult{}

	// Determine sync token
	syncToken := "*" // Full sync
	if state.SyncCursor != nil && *state.SyncCursor != "" {
		syncToken = *state.SyncCursor
	}

	// Perform sync
	syncResp, err := client.Sync(ctx, syncToken, []string{"items", "labels", "projects", "user"}, nil)
	if err != nil {
		return result, p.handleSyncError(err)
	}

	// Update project name from sync response (in case of project rename)
	if len(syncResp.Projects) > 0 {
		for _, project := range syncResp.Projects {
			if project.ID == settings.ProjectID && !project.IsDeleted {
				if project.Name != settings.ProjectName {
					settings.ProjectName = project.Name
					// Update metadata with new project name
					if err := p.updateSyncStateMetadata(ctx, state.ID, settings); err != nil {
						logger.Warn().Err(err).Msg("failed to update project name in metadata")
					}
				}
				break
			}
		}
	}

	// Update label name from sync response (in case of label rename)
	if len(syncResp.Labels) > 0 {
		for _, label := range syncResp.Labels {
			if label.ID == settings.LabelID && !label.IsDeleted {
				if label.Name != settings.LabelName {
					settings.LabelName = label.Name
					// Update metadata with new label name
					if err := p.updateSyncStateMetadata(ctx, state.ID, settings); err != nil {
						logger.Warn().Err(err).Msg("failed to update label name in metadata")
					}
				}
				break
			}
		}
	}

	// Update user timezone from sync response
	if syncResp.User != nil && syncResp.User.TzInfo != nil && syncResp.User.TzInfo.Timezone != "" {
		settings.UserTimezone = syncResp.User.TzInfo.Timezone
		// Update metadata with timezone
		if err := p.updateSyncStateMetadata(ctx, state.ID, settings); err != nil {
			logger.Warn().Err(err).Msg("failed to update timezone in metadata")
		}
	}

	// Process synced items.
	processedTasks, commands, itemsRecoveryFailed, processErr := p.processItems(ctx, syncResp.Items, settings, accountID)
	result.ItemsProcessed = processedTasks

	// Execute any accumulated commands BEFORE checking processErr. If
	// processItems aborted mid-batch, the commands accumulated up to that
	// point are all cleanup commands (ItemClose / ItemDelete) for already-
	// persisted local state transitions — executing them prevents orphaning
	// Todoist tasks whose local rows are already in terminal states. See the
	// comment in processItems's abort path for why this is safe.
	mappingRolledBack := false
	if len(commands) > 0 {
		for _, batch := range BatchCommands(commands, 100) {
			cmdResp, err := client.Sync(ctx, syncResp.SyncToken, []string{}, batch)
			if err != nil {
				logger.Warn().Err(err).Int("commands", len(batch)).Msg("failed to execute Todoist commands")
			} else if cmdResp != nil {
				if p.processTempIDMappings(ctx, cmdResp.TempIDMap) {
					mappingRolledBack = true
				}
			}
		}
	}

	// On a fatal error from processItems, return WITHOUT setting
	// result.NewCursor so the next sync replays the batch from the pre-batch
	// cursor.
	if processErr != nil {
		return result, processErr
	}

	// Store new sync token
	result.NewCursor = syncResp.SyncToken

	// If this was a full sync, perform an immediate incremental sync
	// to ensure we have the latest updates
	if syncResp.FullSync && syncToken == "*" {
		logger.Debug().Msg("performing follow-up incremental sync after full sync")
		followUpResp, err := client.Sync(ctx, syncResp.SyncToken, []string{"items"}, nil)
		if err == nil {
			result.NewCursor = followUpResp.SyncToken
		}
	}

	// Reconcile: ensure all contacts with cadence have tasks.
	// deferSkipDrift suppresses the skip-drift recovery branch when:
	//   - processTempIDMappings had a per-task tx rollback (mapping
	//     ambiguous — real remote task may exist); OR
	//   - tryRecoverPendingTempID attempted recovery but the tx rolled
	//     back (local row stale while real remote item was delivered this
	//     tick — skip-drift would emit a duplicate item_add).
	deferSkipDrift := mappingRolledBack || itemsRecoveryFailed
	reconcileCommands := p.reconcileContactTasks(ctx, client, settings, accountID, deferSkipDrift)
	if len(reconcileCommands) > 0 {
		for _, batch := range BatchCommands(reconcileCommands, 100) {
			cmdResp, err := client.Sync(ctx, result.NewCursor, []string{}, batch)
			if err != nil {
				logger.Warn().Err(err).Int("commands", len(batch)).Msg("failed to execute reconciliation commands")
			} else if cmdResp != nil {
				// Rollback here is picked up next tick by design — no further
				// reconcile pass within this Sync invocation.
				_ = p.processTempIDMappings(ctx, cmdResp.TempIDMap)
			}
		}
	}

	logger.Info().
		Str("source", SourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Int("commands", len(commands)+len(reconcileCommands)).
		Msg("Todoist cadence sync completed")

	return result, nil
}

// ValidateCredentials checks if the Todoist credentials are valid
func (p *CadenceSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	if accountID == nil {
		// Check if any account exists
		if !p.oauthService.HasAnyAccount(ctx) {
			return fmt.Errorf("no Todoist account connected")
		}
		return nil
	}

	// Validate specific account
	_, err := p.oauthService.GetAccessToken(ctx, *accountID)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	return nil
}

// processItemResult is the structured return from processItem and its
// handlers.
//
// Fields:
//   - Processed: true if the item was owned by CRM and handled (not skipped).
//   - Commands:  Todoist commands to enqueue in the batch.
//   - Err:       non-nil for fatal errors; processItems aborts the batch
//     without advancing the cursor.
//   - RecoveryFailed: tryRecoverPendingTempID matched a CRM marker for
//     this item but its atomic tx (external_id swap + pending_temp_id
//     clear) rolled back, leaving the local row stale. Sync uses this to
//     force deferSkipDrift=true for reconcile in the same invocation —
//     otherwise reconcile's skip-drift branch would emit a duplicate
//     item_add against the real remote task that already exists.
type processItemResult struct {
	Processed      bool
	Commands       []SyncCommand
	Err            error
	RecoveryFailed bool
}

// processItems iterates over the items returned by a Todoist sync,
// dispatching each through processItem. On a fatal processItem error
// (r.Err != nil) the batch aborts and the Sync caller preserves the
// pre-batch cursor so the next tick replays. Each successful item commits
// atomically via its own tx, so replays short-circuit at the state !=
// 'managed' early-return in processItem and never double-advance.
//
// recoveryFailed is set when any item's tryRecoverPendingTempID attempted
// recovery but the atomic tx rolled back. Sync threads this into
// reconcileContactTasks as deferSkipDrift=true so the skip-drift branch
// does not fire against the stale local row this tick — the real Todoist
// task already exists server-side.
func (p *CadenceSyncProvider) processItems(
	ctx context.Context,
	items []SyncItem,
	settings Settings,
	accountID string,
) (processed int, commands []SyncCommand, recoveryFailed bool, err error) {
	for _, item := range items {
		r := p.processItem(ctx, item, settings, accountID)
		if r.RecoveryFailed {
			recoveryFailed = true
		}
		if r.Err != nil {
			logger.Error().Err(r.Err).
				Str("itemId", item.ID).
				Int("processedBeforeAbort", processed).
				Msg("processItem fatal error — aborting sync without advancing cursor")
			// Return accumulated commands so Sync can execute any
			// ItemClose/ItemDelete commands queued by already-committed
			// handlers before returning the error.
			return processed, commands, recoveryFailed, fmt.Errorf("process item %s: %w", item.ID, r.Err)
		}
		if r.Processed {
			processed++
		}
		commands = append(commands, r.Commands...)
	}
	return processed, commands, recoveryFailed, nil
}

// processItem processes a single Todoist item from sync
func (p *CadenceSyncProvider) processItem(
	ctx context.Context,
	item SyncItem,
	settings Settings,
	accountID string,
) (result processItemResult) {
	var commands []SyncCommand

	// Recovery-failed signal propagates up to Sync (via processItems) so
	// deferSkipDrift=true forces reconcile's skip-drift branch to defer
	// when this item's recovery tx rolled back. Applied via defer so every
	// return path carries the flag.
	var recoveryFailed bool
	defer func() {
		if recoveryFailed {
			result.RecoveryFailed = true
		}
	}()

	// Find if this task is linked to a contact
	task, err := p.contactTaskRepo.GetContactTaskByExternalID(ctx, SourceName, item.ID)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return processItemResult{Err: fmt.Errorf("process item: lookup contact_task: %w", err)}
		}
		// Fallback: if processTempIDMappings failed, the contact_task still has a
		// temp ID while Todoist uses the real ID. Try to recover by matching the
		// CRM marker in the description to find a task with a pending temp ID.
		task, recoveryFailed = p.tryRecoverPendingTempID(ctx, item)
		if task == nil {
			return processItemResult{}
		}
	}

	// Skip unmanaged tasks
	if task.State != repository.ContactTaskStateManaged {
		return processItemResult{}
	}

	// Get the contact
	contact, err := p.contactRepo.GetContact(ctx, task.ContactID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Contact was deleted - delete the Todoist task
			commands = append(commands, NewItemDeleteCommand(item.ID))
			if err := p.contactTaskRepo.DeleteContactTask(ctx, task.ID); err != nil {
				logger.Warn().Err(err).Str("taskId", task.ID.String()).Msg("failed to delete contact task")
			}
			return processItemResult{Commands: commands}
		}
		return processItemResult{Err: fmt.Errorf("process item: load contact: %w", err)}
	}

	// Check for recurring detection (transition to unmanaged)
	if item.Due != nil && item.Due.IsRecurring {
		return p.handleRecurringDetection(ctx, item, task)
	}

	// Check for completion
	if item.Checked {
		return p.handleTaskCompletion(ctx, item, task, contact, settings, accountID)
	}

	// Handle legacy action tasks separately (kind-keyed, no lifecycle dispatch).
	if task.Kind == contacttask.KindAction {
		return p.handleActionTaskTriggers(ctx, item, task, settings)
	}

	// Detect skip triggers (shared across the lifecycle dispatches below).
	skipTriggered := false
	reason := ""
	switch {
	case item.IsDeleted:
		skipTriggered, reason = true, "deleted"
	case !containsLabel(item.Labels, settings.LabelName):
		skipTriggered, reason = true, "label_removed"
	case item.Deadline == nil:
		skipTriggered, reason = true, "deadline_removed"
	}

	if skipTriggered {
		logger.Info().
			Str("taskId", item.ID).
			Str("reason", reason).
			Str("kind", task.Kind).
			Str("lifecycle", task.Lifecycle).
			Msg("skip trigger detected")

		switch task.Lifecycle {
		case contacttask.LifecycleFollowUpLoop:
			return p.handleFollowUpDismissal(ctx, item, task, contact)
		case contacttask.LifecycleCadenceDue:
			return p.handleSkipTrigger(ctx, item, task, contact, settings, accountID)
		case contacttask.LifecycleManual:
			return p.handleManualDismissal(ctx, task)
		default:
			logger.Warn().
				Str("taskId", item.ID).
				Str("kind", task.Kind).
				Str("lifecycle", task.Lifecycle).
				Msg("skip trigger fired for unknown lifecycle; falling through")
			return processItemResult{Processed: true}
		}
	}

	// Check for deadline edit (Todoist wins) — only for cadence-due tasks.
	// Follow-up tasks use a separate grace-period deadline (last_outreach_at +
	// watchdog_days) that is unrelated to contact_by; allowing them through
	// here previously regressed contact_by via UpdateContactBy on the next
	// sync tick (see fix/followup-deadline-regression). Manual and legacy
	// action tasks have already been routed out above; the explicit
	// LifecycleCadenceDue check is self-documenting and load-bearing.
	if task.Lifecycle == contacttask.LifecycleCadenceDue && item.Deadline != nil && contact.ContactBy != nil {
		todoistDeadline, err := time.Parse(DateFormat, item.Deadline.Date)
		if err == nil {
			// Compare dates using UTC year/month/day to avoid timezone issues
			// time.Parse returns UTC midnight, PostgreSQL DATE loads as UTC
			tY, tM, tD := todoistDeadline.UTC().Date()
			cY, cM, cD := contact.ContactBy.UTC().Date()
			if tY != cY || tM != cM || tD != cD {
				// Gate: distinguish a stale Todoist re-delivery (item.Deadline
				// matches synced_deadline — i.e., what we last pushed) from a
				// real user-side Todoist edit (item.Deadline differs from
				// synced_deadline). When stale, fall through and let
				// reconcileContactTasks push CRM → Todoist on a later tick.
				// When synced_deadline is missing (legacy task or fresh-create
				// race), preserve pre-fix behavior and fire the clobber so a
				// genuine Todoist edit on a legacy task is not dropped (the
				// incremental sync cursor advances past unprocessed items, so
				// skipping is permanent data loss).
				syncedDeadline, _ := task.Metadata[MetadataKeySyncedDeadline].(string)
				if syncedDeadline != "" && item.Deadline.Date == syncedDeadline {
					logger.Debug().
						Str("contactId", contact.ID.String()).
						Str("taskKind", task.Kind).
						Str("itemDeadline", item.Deadline.Date).
						Str("syncedDeadline", syncedDeadline).
						Str("contactBy", contact.ContactBy.Format(DateFormat)).
						Msg("todoist deadline matches synced_deadline; CRM ahead, skipping Todoist-wins branch")
				} else {
					// Real user edit (or legacy task with no synced_deadline).
					// Apply atomically: route contact_by through CadenceUpdater
					// and write synced_deadline metadata in the same tx so a
					// half-applied state can never strand reconciliation in a
					// spurious-drift loop.
					newDeadlineStr := todoistDeadline.Format(DateFormat)
					metadata := task.Metadata
					if metadata == nil {
						metadata = make(map[string]any)
					}
					metadata[MetadataKeySyncedDeadline] = newDeadlineStr

					txErr := pgx.BeginTxFunc(ctx, p.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
						if err := p.cadenceUpdater.ApplyContactByOverride(ctx, tx, contact.ID, &todoistDeadline); err != nil {
							return fmt.Errorf("apply contact_by override from todoist deadline edit: %w", err)
						}
						if _, err := p.contactTaskRepo.UpdateContactTaskMetadataTx(ctx, tx, task.ID, metadata); err != nil {
							return fmt.Errorf("update synced_deadline after todoist deadline edit: %w", err)
						}
						return nil
					})
					if txErr != nil {
						return processItemResult{Err: fmt.Errorf("deadline-edit tx: %w", txErr)}
					}

					logger.Info().
						Str("contactId", contact.ID.String()).
						Str("taskKind", task.Kind).
						Str("syncedDeadline", syncedDeadline).
						Time("newContactBy", todoistDeadline).
						Msg("updated contact_by and synced_deadline from Todoist deadline edit")
				}
			}
		}
	}

	return processItemResult{Processed: true, Commands: commands}
}

// handleRecurringDetection transitions a task to 'unmanaged' when it has been
// edited in Todoist to become recurring. Recurring tasks are not cadence-
// tracked because they manage their own lifecycle in Todoist.
//
// Returns a fatal error via processItemResult.Err on state-update failure.
// This state transition is the entire point of the branch, so a silent failure
// would leave the row permanently mis-managed.
func (p *CadenceSyncProvider) handleRecurringDetection(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
) processItemResult {
	logger.Info().
		Str("taskId", item.ID).
		Str("contactId", task.ContactID.String()).
		Msg("task became recurring, transitioning to unmanaged")
	if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
		return processItemResult{Err: fmt.Errorf("process item: update state to unmanaged (recurring): %w", err)}
	}
	return processItemResult{Processed: true}
}

// handleTaskCompletion handles a completed Todoist task.
//
// Atomic publish + state advance: opens a pgx.Tx, publishes task.completed
// via PublishTx, transitions the local row to 'completed' via
// UpdateContactTaskStateTx, commits. The downstream InteractionRecorder
// (async via river) consumes the event and writes the interaction row.
//
// Direction derivation (spec §3.4.5): action tasks emit direction="mutual"
// (matching the legacy default), cadence/follow_up emit direction="outbound".
//
// SourceID is task.ID.String() (stable internal uuid). Using ExternalTaskID
// would collide with the temp→real id swap in tryRecoverPendingTempID,
// bypassing event-layer dedup.
func (p *CadenceSyncProvider) handleTaskCompletion(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	contact *repository.Contact,
	settings Settings,
	accountID string,
) processItemResult {
	// Parse completion timestamp.
	completedAt := accelerated.GetCurrentTime()
	if item.CompletedAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *item.CompletedAt); err == nil {
			completedAt = parsed
		}
	}

	// Reminder short-circuits BEFORE event publishing: no event, no
	// interaction row, just transition state to 'completed'. Must run
	// before the publish path because reminder-completion is supposed
	// to be invisible to the event bus.
	if task.Kind == contacttask.KindReminder {
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateCompleted); err != nil {
			return processItemResult{Err: fmt.Errorf("reminder completion: update state: %w", err)}
		}
		logger.Info().
			Str("contactId", contact.ID.String()).
			Str("taskKind", task.Kind).
			Time("completedAt", completedAt).
			Msg("reminder completed; no event published")
		return processItemResult{Processed: true}
	}

	// Direction + suppress flags by kind.
	//
	//   reach_out → outbound, no suppression (default flow: spawns follow-up).
	//   send      → outbound, suppress follow-up (one-shot send).
	//   meet      → mutual (legacy).
	//   action    → mutual (legacy).
	direction := repository.InteractionDirectionOutbound
	suppressFollowUp := false
	switch task.Kind {
	case contacttask.KindSend:
		suppressFollowUp = true
	case contacttask.KindMeet, contacttask.KindAction:
		direction = repository.InteractionDirectionMutual
	}

	payload, err := events.Marshal(events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:          2,
		ContactID:        contact.ID,
		TaskID:           task.ExternalTaskID,
		TaskKind:         task.Kind,
		CompletedAt:      completedAt,
		Direction:        direction,
		SuppressFollowUp: suppressFollowUp,
	})
	if err != nil {
		return processItemResult{Err: fmt.Errorf("marshal task.completed: %w", err)}
	}
	env := &events.Envelope{
		Source:     SourceName,
		SourceID:   task.ID.String(),
		Kind:       events.KindTaskCompleted,
		Payload:    payload,
		ObservedAt: completedAt,
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return processItemResult{Err: fmt.Errorf("begin tx: %w", err)}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Publish-before-mutate (core.md gotcha).
	if err := p.bus.PublishTx(ctx, tx, env); err != nil {
		return processItemResult{Err: fmt.Errorf("publish task.completed: %w", err)}
	}
	if env.ID == uuid.Nil {
		// Duplicate — prior tick already processed this completion. The
		// state=completed short-circuit at processItem normally prevents
		// reaching this handler on replay; this branch defends against
		// the degenerate case where the state update rolled back on the
		// earlier tick but the event committed.
		logger.Debug().
			Str("contactTaskId", task.ID.String()).
			Str("externalTaskId", task.ExternalTaskID).
			Msg("task.completed duplicate; skipping state update")
		return processItemResult{Processed: true}
	}

	// Transition local state to 'completed' inside the tx. For cadence /
	// follow_up tasks, the state advance is the pre-condition for the
	// downstream FollowUpManager's FindPendingFollowUp gate — once the tx
	// commits, the async consumer sees state='completed' and creates the
	// successor follow-up instead of refreshing the dying task.
	if _, err := p.contactTaskRepo.UpdateContactTaskStateTx(
		ctx, tx, task.ID, repository.ContactTaskStateCompleted,
	); err != nil {
		return processItemResult{Err: fmt.Errorf("update task state: %w", err)}
	}

	if err := tx.Commit(ctx); err != nil {
		return processItemResult{Err: fmt.Errorf("commit: %w", err)}
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("taskKind", task.Kind).
		Time("completedAt", completedAt).
		Str("direction", direction).
		Msg("published task.completed (atomic commit)")

	return processItemResult{Processed: true}
}

// handleSkipTrigger handles a skip trigger (task deleted, label removed, deadline removed).
//
// Atomic publish + mutate: opens a pgx.Tx, publishes task.skipped via PublishTx,
// advances contact.contact_by via UpdateContactByTx, writes metadata keys
// (pending_temp_id, synced_deadline, synced_last_*) via UpdateContactTaskMetadataTx,
// commits. Rollback on any error rolls back event row, contact_by advance, and
// metadata together — no partial commits.
//
// Replay safety: the event SourceID is task.ID.String():item.UpdatedAt. A
// replay of the same unchanged Todoist item hits the (source, source_id)
// unique on the event table; PublishTx resets env.ID to uuid.Nil and returns
// nil; the handler short-circuits without advancing contact_by. The suffix
// distinguishes genuine repeat-skips (item re-edited → new UpdatedAt) from
// replays of the same unchanged item.
//
// Ordering (publish-before-mutate per core.md): PublishTx → mutate →
// commit. Reversing would strand interactions on publish failure.
func (p *CadenceSyncProvider) handleSkipTrigger(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	contact *repository.Contact,
	settings Settings,
	accountID string,
) processItemResult {
	// Early-return: no cadence → nothing to advance → no state change, no event.
	if contact.Cadence == nil || *contact.Cadence == "" {
		return processItemResult{Processed: true}
	}
	cadenceType, err := cadence.ParseCadence(*contact.Cadence)
	if err != nil {
		logger.Warn().Err(err).
			Str("contactId", contact.ID.String()).
			Str("cadence", *contact.Cadence).
			Msg("skip trigger: invalid cadence; no-op")
		return processItemResult{Processed: true}
	}

	// Compute nextContactBy (skip semantics: later of today+cadence or
	// old contact_by+cadence).
	days := cadence.CadenceDays(cadenceType)
	now := accelerated.GetCurrentTime()
	today := cadence.Today(now)
	fromToday := today.AddDate(0, 0, days)
	fromSkipped := fromToday
	if contact.ContactBy != nil {
		fromSkipped = contact.ContactBy.AddDate(0, 0, days)
	}
	nextContactBy := fromToday
	if fromSkipped.After(fromToday) {
		nextContactBy = fromSkipped
	}

	skippedAt := accelerated.GetCurrentTime()
	payload, err := events.Marshal(events.KindTaskSkipped, events.TaskSkippedPayload{
		Version:   1,
		ContactID: contact.ID,
		TaskID:    task.ExternalTaskID,
		SkippedAt: skippedAt,
	})
	if err != nil {
		return processItemResult{Err: fmt.Errorf("marshal task.skipped: %w", err)}
	}
	// SourceID: stable internal contact_task.id + item.UpdatedAt so replays
	// of the same unchanged item dedup, while user re-edits produce new events.
	env := &events.Envelope{
		Source:     SourceName,
		SourceID:   fmt.Sprintf("%s:%s", task.ID.String(), item.UpdatedAt),
		Kind:       events.KindTaskSkipped,
		Payload:    payload,
		ObservedAt: skippedAt,
	}

	// Pre-compute replacement item_add so its TempID can land in metadata
	// atomically with the state advance.
	deadlineStr := nextContactBy.Format(DateFormat)
	replacementCmd := p.createTaskCommand(contact, settings, &deadlineStr)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return processItemResult{Err: fmt.Errorf("begin tx: %w", err)}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Publish-before-mutate (core.md gotcha).
	if err := p.bus.PublishTx(ctx, tx, env); err != nil {
		return processItemResult{Err: fmt.Errorf("publish task.skipped: %w", err)}
	}
	if env.ID == uuid.Nil {
		// Duplicate — prior tick processed this skip. No state change, no
		// replacement command. Self-healing for a previously-failed HTTP
		// batch is the responsibility of reconcileExistingTask's skip-drift
		// branch, not this handler.
		logger.Debug().
			Str("sourceId", env.SourceID).
			Msg("task.skipped duplicate; skipping state advance")
		return processItemResult{Processed: true}
	}

	// Advance contact_by via the sole writer.
	if err := p.cadenceUpdater.ApplyContactByOverride(ctx, tx, contact.ID, &nextContactBy); err != nil {
		return processItemResult{Err: fmt.Errorf("update contact_by: %w", err)}
	}

	// Update metadata. Preserve existing keys — pre-skip synced_deadline is
	// consulted by the reconciler's skip-drift branch to close+recreate on
	// HTTP failure.
	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[MetadataKeyPendingTempID] = replacementCmd.TempID
	metadata[MetadataKeySyncedDeadline] = deadlineStr
	if contact.LastContacted != nil {
		metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
	}
	if contact.LastOutreachAt != nil {
		metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
	}
	if _, err := p.contactTaskRepo.UpdateContactTaskMetadataTx(ctx, tx, task.ID, metadata); err != nil {
		return processItemResult{Err: fmt.Errorf("update task metadata: %w", err)}
	}

	if err := tx.Commit(ctx); err != nil {
		return processItemResult{Err: fmt.Errorf("commit: %w", err)}
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Time("newContactBy", nextContactBy).
		Str("sourceId", env.SourceID).
		Msg("published task.skipped (atomic commit)")

	// Return replacement item_add only after commit — the HTTP batch fires
	// outside the tx in Sync's post-loop block (core.md gotcha).
	return processItemResult{Processed: true, Commands: []SyncCommand{replacementCmd}}
}

// handleFollowUpDismissal handles a follow-up task being dismissed via Todoist.
// Triggered when the user deletes the task, removes the CRM label, or removes
// the deadline on a follow_up kind task. Marks the local contact_task as
// 'dismissed' and does NOT record an interaction, does NOT create a successor
// follow-up, and does NOT touch any contact date field (last_contacted,
// last_interaction_at, last_response_at, contact_by, last_outreach_at). The
// cadence clock is driven by real engagement and continues from its last real
// mutual/inbound interaction.
//
// State is persisted BEFORE any ItemClose command is queued. On state-update
// failure, this handler returns a fatal error via processItemResult.Err;
// processItems aborts the batch, the Sync caller preserves the pre-batch
// cursor, and the next tick replays.
//
// For non-deletion triggers, once the state transition succeeds we queue an
// ItemClose command so the batched sync path cleans up the orphaned task in
// Todoist. If the batch fails, the local row is still correctly 'dismissed'
// and subsequent syncs will skip it via the existing state != 'managed'
// early-return in processItem.
func (p *CadenceSyncProvider) handleFollowUpDismissal(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	contact *repository.Contact,
) processItemResult {
	if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateDismissed); err != nil {
		// Do NOT forward the ItemClose command — stranding local 'managed' +
		// closed Todoist would break FindPendingFollowUp permanently.
		return processItemResult{Err: fmt.Errorf("dismiss follow-up: update contact_task state: %w", err)}
	}

	var commands []SyncCommand
	if !item.IsDeleted {
		commands = append(commands, NewItemCloseCommand(item.ID))
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("contactName", contact.FullName).
		Str("todoistTaskId", task.ExternalTaskID).
		Bool("itemDeleted", item.IsDeleted).
		Int("commandsQueued", len(commands)).
		Msg("follow-up dismissed via Todoist")

	return processItemResult{Processed: true, Commands: commands}
}

// handleManualDismissal transitions a manual-lifecycle task (kind in
// reach_out / send / reminder) to state='dismissed' when the user
// removes/deletes/unlabels it in Todoist. No interaction row, no event,
// no successor task. Distinct from legacy action's `unmanaged` semantics
// per spec §2 Decision row "Legacy `action` row dismissal semantics."
//
// No ItemClose is enqueued — the dismissal trigger is "user deleted /
// unlabelled / removed deadline in Todoist", so there is nothing to
// close on the remote side.
func (p *CadenceSyncProvider) handleManualDismissal(
	ctx context.Context,
	task *repository.ContactTask,
) processItemResult {
	if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateDismissed); err != nil {
		return processItemResult{Err: fmt.Errorf("manual dismissal: update state: %w", err)}
	}

	logger.Info().
		Str("contactId", task.ContactID.String()).
		Str("contactTaskId", task.ID.String()).
		Str("kind", task.Kind).
		Str("lifecycle", task.Lifecycle).
		Msg("manual task dismissed via Todoist")

	return processItemResult{Processed: true}
}

// handleActionTaskTriggers handles unmanagement triggers for action tasks
// Action tasks are simpler than cadence tasks - they just transition to unmanaged
// when the label is removed or the task is deleted. No skip semantics.
func (p *CadenceSyncProvider) handleActionTaskTriggers(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	settings Settings,
) processItemResult {
	// Task deleted - mark as unmanaged
	if item.IsDeleted {
		logger.Info().
			Str("taskId", item.ID).
			Str("contactTaskId", task.ID.String()).
			Msg("action task deleted in Todoist, marking as unmanaged")
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
			return processItemResult{Err: fmt.Errorf("action task triggers (deleted): update state to unmanaged: %w", err)}
		}
		return processItemResult{Processed: true}
	}

	// Label removed - mark as unmanaged
	if !containsLabel(item.Labels, settings.LabelName) {
		logger.Info().
			Str("taskId", item.ID).
			Str("contactTaskId", task.ID.String()).
			Msg("CRM label removed from action task, marking as unmanaged")
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
			return processItemResult{Err: fmt.Errorf("action task triggers (label removed): update state to unmanaged: %w", err)}
		}
		return processItemResult{Processed: true}
	}

	// Update metadata with current task state (content, due_date, project)
	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["content"] = item.Content
	if item.Deadline != nil {
		metadata["due_date"] = item.Deadline.Date
	} else if item.Due != nil {
		metadata["due_date"] = item.Due.Date
	} else {
		delete(metadata, "due_date")
	}
	metadata["project_id"] = item.ProjectID

	if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
		logger.Warn().Err(err).Msg("failed to update action task metadata")
	}

	return processItemResult{Processed: true}
}

// reconcileContactTasks ensures all contacts with cadence have managed tasks
// and that existing tasks have deadlines matching the contact's contact_by.
//
// deferSkipDrift suppresses the skip-drift recovery branch in
// reconcileExistingTask for this invocation. Set when processTempIDMappings
// had a tx rollback earlier in the same Sync call — the row's
// (ExternalTaskID=<old>, pending_temp_id=<temp-new>) shape then becomes
// ambiguous between "HTTP batch failed" (branch should fire) and "HTTP
// succeeded, local mapping rolled back" (branch must NOT fire, else emits a
// duplicate item_add against an already-created remote task). Safe choice
// is to defer one tick; tryRecoverPendingTempID finalizes next tick when
// the real item arrives in syncResp.Items.
func (p *CadenceSyncProvider) reconcileContactTasks(
	ctx context.Context,
	client Client,
	settings Settings,
	accountID string,
	deferSkipDrift bool,
) []SyncCommand {
	var commands []SyncCommand

	// Get all contacts with cadence
	contacts, err := p.contactRepo.ListContactsWithContactBy(ctx, 10000)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to list contacts for reconciliation")
		return nil
	}

	for _, contact := range contacts {
		// Skip contacts without cadence
		if contact.Cadence == nil || *contact.Cadence == "" {
			continue
		}

		// Skip contacts without contact_by (nothing to sync)
		if contact.ContactBy == nil {
			continue
		}

		// Look up existing cadence task (before follow-up gate so outreach detection
		// can fire even when a pending follow-up exists).
		task, err := p.contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contact.ID, SourceName)
		if err == nil && task.State == repository.ContactTaskStateCompleted {
			// Clean up completed cadence task so a new one can be created.
			// This happens after handleTaskCompletion marks cadence tasks completed
			// before recording the outbound interaction (follow-up ordering fix).
			if deleteErr := p.contactTaskRepo.DeleteContactTask(ctx, task.ID); deleteErr != nil {
				logger.Warn().Err(deleteErr).
					Str("contactId", contact.ID.String()).
					Msg("failed to clean up completed cadence task — skipping until next cycle")
				continue
			}
			err = db.ErrNotFound
		}

		// If a managed cadence task exists, check if the contact was reached out to
		// from a non-Todoist source (e.g., Telegram). If so, close the cadence task.
		if err == nil && task.State == repository.ContactTaskStateManaged {
			closeCmds, handled := p.closeOnOutreach(ctx, task, &contact)
			if handled {
				commands = append(commands, closeCmds...)
				continue
			}
		}

		// Skip contacts with pending follow-up (grace period — waiting for response)
		_, followUpErr := p.contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
		if followUpErr == nil {
			logger.Debug().
				Str("contactId", contact.ID.String()).
				Str("contactName", contact.FullName).
				Msg("skipping reconciliation: pending follow-up task exists")
			continue
		}
		if !errors.Is(followUpErr, db.ErrNotFound) {
			continue // unexpected error, skip
		}

		currentDeadline := contact.ContactBy.Format(DateFormat)

		if err != nil {
			if !errors.Is(err, db.ErrNotFound) {
				continue
			}
			// No task exists - create one
			cmd := p.createTaskCommand(&contact, settings, &currentDeadline)
			commands = append(commands, cmd)

			// Create task link (with temp_id, synced_deadline, and synced_last_contacted)
			taskMetadata := map[string]any{
				MetadataKeyPendingTempID:  cmd.TempID,
				MetadataKeySyncedDeadline: currentDeadline,
			}
			if contact.LastContacted != nil {
				taskMetadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
			}
			if contact.LastOutreachAt != nil {
				taskMetadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
			}
			_, createErr := p.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
				ContactID:      contact.ID,
				Provider:       SourceName,
				Kind:           contacttask.KindReachOut,
				Lifecycle:      contacttask.LifecycleCadenceDue,
				ExternalTaskID: cmd.TempID, // Will be updated on sync response
				State:          string(repository.ContactTaskStateManaged),
				Metadata:       taskMetadata,
			})
			if createErr != nil {
				logger.Warn().Err(createErr).Str("contactId", contact.ID.String()).Msg("failed to create contact task")
			}
			continue
		}

		// Check if task is unmanaged (skip it)
		if task.State != repository.ContactTaskStateManaged {
			continue
		}

		// Task exists and is managed - check if deadline needs updating
		cmds := p.reconcileExistingTask(ctx, task, &contact, settings, currentDeadline, deferSkipDrift)
		commands = append(commands, cmds...)
	}

	// Follow-up close retries are now owned by TodoistFollowUpCloseJob
	// river workers (event-bus foundation cutover); no inline retry
	// loop is needed on the Todoist sync tick.

	return commands
}

// closeOnOutreach closes a managed cadence task when the contact has been reached out to
// from a non-Todoist source (e.g., Telegram). Returns (commands, true) when outreach was
// detected and handled, (nil, false) otherwise. The bool distinguishes "no outreach" from
// "outreach handled but no Todoist command needed" (e.g., pending temp ID). State
// transition must succeed before the close command is enqueued (same pattern as
// handleFollowUpDismissal).
func (p *CadenceSyncProvider) closeOnOutreach(
	ctx context.Context,
	task *repository.ContactTask,
	contact *repository.Contact,
) ([]SyncCommand, bool) {
	if p.wasReachedOutSinceSync(contact, task) {
		logger.Info().
			Str("contactId", contact.ID.String()).
			Str("contactName", contact.FullName).
			Msg("contact was reached out to since last sync (non-Todoist source), closing cadence task")

		// Mark completed locally FIRST — only enqueue the Todoist close if this succeeds.
		// If state update fails, skip the remote close entirely to avoid leaving a
		// local 'managed' row while the Todoist task is closed (same pattern as
		// handleFollowUpDismissal).
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateCompleted); err != nil {
			logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to mark cadence task completed after outreach detection — skipping close")
			return nil, false
		}

		var commands []SyncCommand

		// State transition succeeded — now safe to close in Todoist
		if task.ExternalTaskID != "" && !isPendingTempID(task) {
			commands = append(commands, NewItemCloseCommand(task.ExternalTaskID))
		}

		// Update synced_last_outreach_at so we don't re-fire on the next tick
		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		if contact.LastOutreachAt != nil {
			metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
		}
		if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
			logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to update synced_last_outreach_at after outreach detection")
		}

		return commands, true
	}

	// No outreach detected. Backfill synced_last_outreach_at if missing (legacy tasks
	// created before this feature). This backfill is critical here because
	// reconcileExistingTask (the other backfill location) is only reached AFTER the
	// follow-up gate — tasks with a pending follow-up would never get backfilled
	// without this pre-gate path.
	if _, hasSyncedLO := task.Metadata[MetadataKeySyncedLastOutreachAt].(string); !hasSyncedLO {
		if contact.LastOutreachAt != nil {
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to backfill synced_last_outreach_at (pre-gate)")
			} else {
				task.Metadata = metadata
			}
		}
	}

	return nil, false
}

// reconcileExistingTask checks if an existing managed task's deadline matches contact_by.
// If not, it completes the old task and creates a new one with the updated deadline.
//
// Race condition note: There's a small window between sync fetch and command execution where
// a user could edit the Todoist task's deadline. If this happens, the close+create commands
// will overwrite the user's manual edit. This is acceptable because:
//  1. The window is extremely narrow (milliseconds within the same sync operation)
//  2. CRM is authoritative for contact_by - manual Todoist edits outside the sync flow
//     should use processItem which runs before reconciliation
//  3. Preventing this would require fetching fresh state before each close operation,
//     adding significant complexity and latency
func (p *CadenceSyncProvider) reconcileExistingTask(
	ctx context.Context,
	task *repository.ContactTask,
	contact *repository.Contact,
	settings Settings,
	currentDeadline string,
	deferSkipDrift bool,
) []SyncCommand {
	var commands []SyncCommand

	// Skip-drift recovery: a prior handleSkipTrigger tx committed state +
	// event, but the post-commit Todoist HTTP batch (item_add) never
	// processed, so pending_temp_id is stale. Detection: metadata has a
	// non-empty pending_temp_id that does NOT match the current
	// ExternalTaskID. On the happy path, processTempIDMappings clears
	// pending_temp_id once the real Todoist id returns; a stale
	// pending_temp_id with unchanged ExternalTaskID means the mapping
	// never landed.
	//
	// Deferral: when this Sync invocation had a processTempIDMappings tx
	// roll back (deferSkipDrift=true), the row's stale-looking state
	// cannot be distinguished from "HTTP batch failed". Firing the branch
	// anyway would emit a duplicate item_add against an already-created
	// remote task. Wait one tick; next tick's syncResp.Items will deliver
	// the real task and tryRecoverPendingTempID will finalize the mapping.
	pendingTemp, hasPending := task.Metadata[MetadataKeyPendingTempID].(string)
	if !deferSkipDrift && hasPending && pendingTemp != "" && pendingTemp != task.ExternalTaskID {
		logger.Info().
			Str("contactId", contact.ID.String()).
			Str("externalTaskId", task.ExternalTaskID).
			Str("pendingTempId", pendingTemp).
			Msg("skip-drift recovery: re-emitting close+create for replacement cadence task")

		cmd := p.createTaskCommand(contact, settings, &currentDeadline)

		// Persist new pending_temp_id BEFORE emitting commands. If the
		// metadata write fails, do NOT return the commands — the new
		// temp id would never land in the row, so the post-loop HTTP
		// batch's temp_id_mapping response couldn't be applied via
		// processTempIDMappings. Emitting the commands anyway would
		// create a Todoist task with no way to map back to the contact_task.
		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		metadata[MetadataKeyPendingTempID] = cmd.TempID
		metadata[MetadataKeySyncedDeadline] = currentDeadline
		if contact.LastContacted != nil {
			metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
		}
		if contact.LastOutreachAt != nil {
			metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
		}
		if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
			logger.Warn().Err(err).
				Str("contactId", contact.ID.String()).
				Msg("skip-drift recovery: update metadata failed; not emitting commands (retry next tick)")
			return commands
		}

		if task.ExternalTaskID != "" {
			commands = append(commands, NewItemCloseCommand(task.ExternalTaskID))
		}
		commands = append(commands, cmd)
		return commands
	}

	// Get synced_deadline from metadata
	syncedDeadline, hasSyncedDeadline := task.Metadata[MetadataKeySyncedDeadline].(string)

	if !hasSyncedDeadline {
		// Backfill: No synced_deadline stored yet, assume task is in sync.
		// Note: If actual Todoist deadline differs from currentDeadline, this assumption
		// is incorrect and drift will remain undetected. This is acceptable for migration
		// of existing tasks - any actual drift will be corrected on the next contact_by update.
		logger.Warn().
			Str("contactId", contact.ID.String()).
			Str("externalTaskId", task.ExternalTaskID).
			Str("assumedDeadline", currentDeadline).
			Msg("backfilling synced_deadline - assuming current CRM state matches Todoist")

		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		metadata[MetadataKeySyncedDeadline] = currentDeadline
		if contact.LastContacted != nil {
			metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
		}
		if contact.LastOutreachAt != nil {
			metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
		}
		if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
			logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to backfill synced_deadline")
		}
		return commands
	}

	// Backfill synced_last_contacted if missing (legacy tasks created before this feature).
	// Without this, wasContactedSinceSync would always return false for legacy tasks,
	// meaning non-Todoist contacts would never be detected.
	if _, hasSyncedLC := task.Metadata[MetadataKeySyncedLastContacted].(string); !hasSyncedLC {
		if contact.LastContacted != nil {
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
			if contact.LastOutreachAt != nil {
				metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
			}
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to backfill synced_last_contacted")
			} else {
				// Update task in memory so wasContactedSinceSync can use it this cycle
				task.Metadata = metadata
			}
		}
	}

	// Backfill synced_last_outreach_at if missing (tasks created before this feature).
	// Without this, wasReachedOutSinceSync would always return false for legacy tasks.
	if _, hasSyncedLO := task.Metadata[MetadataKeySyncedLastOutreachAt].(string); !hasSyncedLO {
		if contact.LastOutreachAt != nil {
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to backfill synced_last_outreach_at")
			} else {
				task.Metadata = metadata
			}
		}
	}

	// Check if deadline has drifted
	if syncedDeadline == currentDeadline {
		// Deadlines match - but check if the contact was contacted from a non-Todoist
		// source (e.g., calendar sync). This handles the case where last_contacted was
		// updated and contact_by was recalculated to the same date (same cadence period).
		if p.wasContactedSinceSync(contact, task) {
			logger.Info().
				Str("contactId", contact.ID.String()).
				Str("deadline", currentDeadline).
				Msg("contact was contacted since last sync (non-Todoist source), completing task and creating new one")

			// Complete the old task in Todoist
			if task.ExternalTaskID != "" && !isPendingTempID(task) {
				commands = append(commands, NewItemCloseCommand(task.ExternalTaskID))
			}

			// This branch previously re-computed and wrote contact_by
			// here. Post-cutover the upstream non-Todoist interaction
			// (via InteractionRecorder → CadenceUpdater) already wrote
			// contact_by; reconciliation reads the live
			// contact.ContactBy for the new Todoist task's deadline
			// instead of re-computing + re-writing. Removing the write
			// keeps CadenceUpdater as the sole writer of contact_by.
			if contact.Cadence != nil && *contact.Cadence != "" && contact.ContactBy != nil {
				nextContactBy := *contact.ContactBy

				// Create new task with updated deadline
				deadlineStr := nextContactBy.Format(DateFormat)
				cmd := p.createTaskCommand(contact, settings, &deadlineStr)
				commands = append(commands, cmd)

				// Update metadata
				metadata := task.Metadata
				if metadata == nil {
					metadata = make(map[string]any)
				}
				metadata[MetadataKeyPendingTempID] = cmd.TempID
				metadata[MetadataKeySyncedDeadline] = deadlineStr
				if contact.LastContacted != nil {
					metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
				}
				if contact.LastOutreachAt != nil {
					metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
				}
				if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
					logger.Warn().Err(err).Msg("failed to update metadata after non-Todoist contact")
				}
			}

			return commands
		}

		return commands
	}

	// Deadline has changed in CRM - complete old task and create new one
	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("oldDeadline", syncedDeadline).
		Str("newDeadline", currentDeadline).
		Msg("contact_by changed, completing old task and creating new one")

	// Complete the old task in Todoist (if we have a real external ID)
	if task.ExternalTaskID != "" && !isPendingTempID(task) {
		commands = append(commands, NewItemCloseCommand(task.ExternalTaskID))
	}

	// Create new task with updated deadline
	cmd := p.createTaskCommand(contact, settings, &currentDeadline)
	commands = append(commands, cmd)

	// Update task record with new temp_id and synced_deadline.
	// Note: If this metadata update fails but the commands above succeed when sent to Todoist,
	// we'll have an orphaned task (no pending_temp_id to map). This is logged but not fatal
	// because the sync is async - we can't roll back Todoist commands. The orphaned task
	// will be cleaned up on the next full sync when it appears in items without a linked contact.
	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[MetadataKeyPendingTempID] = cmd.TempID
	metadata[MetadataKeySyncedDeadline] = currentDeadline
	if contact.LastContacted != nil {
		metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
	}
	if contact.LastOutreachAt != nil {
		metadata[MetadataKeySyncedLastOutreachAt] = contact.LastOutreachAt.Format(time.RFC3339)
	}
	if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
		logger.Error().
			Err(err).
			Str("contactId", contact.ID.String()).
			Str("tempId", cmd.TempID).
			Msg("failed to store pending_temp_id after queuing commands - may result in orphaned Todoist task")
	}

	return commands
}

// wasContactedSinceSync checks if the contact's last_contacted has advanced since the
// task was last synced. This detects non-Todoist contact events (e.g., calendar sync)
// that should trigger task completion even when contact_by doesn't change.
func (p *CadenceSyncProvider) wasContactedSinceSync(contact *repository.Contact, task *repository.ContactTask) bool {
	if contact.LastContacted == nil {
		return false
	}

	syncedLastContactedStr, ok := task.Metadata[MetadataKeySyncedLastContacted].(string)
	if !ok || syncedLastContactedStr == "" {
		// No synced_last_contacted stored - can't determine if contacted since sync.
		// This happens for tasks created before this feature. We don't auto-complete
		// to avoid false positives; the metadata will be backfilled on the next
		// task creation or completion cycle.
		return false
	}

	syncedLastContacted, err := time.Parse(time.RFC3339, syncedLastContactedStr)
	if err != nil {
		logger.Warn().Err(err).Str("value", syncedLastContactedStr).Msg("failed to parse synced_last_contacted")
		return false
	}

	return contact.LastContacted.Truncate(time.Second).After(syncedLastContacted)
}

// wasReachedOutSinceSync checks if the contact's last_outreach_at has advanced since the
// task was last synced. This detects non-Todoist outbound events (e.g., Telegram messages)
// that should trigger cadence task completion — the cadence task is an outreach reminder,
// and the outreach has happened.
func (p *CadenceSyncProvider) wasReachedOutSinceSync(contact *repository.Contact, task *repository.ContactTask) bool {
	if contact.LastOutreachAt == nil {
		return false
	}

	syncedLastOutreachStr, ok := task.Metadata[MetadataKeySyncedLastOutreachAt].(string)
	if !ok || syncedLastOutreachStr == "" {
		return false
	}

	syncedLastOutreach, err := time.Parse(time.RFC3339, syncedLastOutreachStr)
	if err != nil {
		logger.Warn().Err(err).Str("value", syncedLastOutreachStr).Msg("failed to parse synced_last_outreach_at")
		return false
	}

	return contact.LastOutreachAt.Truncate(time.Second).After(syncedLastOutreach)
}

// isPendingTempID checks if the task's external ID is still a pending temp ID
// (i.e., the real Todoist ID hasn't been assigned yet).
func isPendingTempID(task *repository.ContactTask) bool {
	if task.Metadata == nil {
		return false
	}
	// Check for empty ExternalTaskID first - if empty, we can't compare
	if task.ExternalTaskID == "" {
		return false
	}
	pendingTempID, ok := task.Metadata[MetadataKeyPendingTempID].(string)
	if !ok || pendingTempID == "" {
		return false
	}
	return pendingTempID == task.ExternalTaskID
}

// tryRecoverPendingTempID handles the case where processTempIDMappings failed
// to update a contact_task's external_task_id from a temp ID to the real Todoist ID.
// It parses the CRM marker in the item description to find the contact, then
// finalizes the mapping atomically inside a pgx.Tx (external_id + metadata clear
// commit together or roll back together).
//
// Returns (task, recoveryFailed). recoveryFailed is true when a CRM marker
// was matched and the atomic recovery tx rolled back — the caller
// (processItem) propagates this via processItemResult.RecoveryFailed so
// Sync can force deferSkipDrift=true for the same-tick reconcile pass.
// recoveryFailed is false when no CRM marker matched (task is nil; nothing
// to recover), when recovery succeeded, or when recovery was not applicable
// (guard rejected the task).
//
// Recovery guard: task.State == managed && pending_temp_id != "". Broadened
// from the narrow isPendingTempID check (which required pending_temp_id ==
// ExternalTaskID) so this function also recovers the post-rollback shape
// where ExternalTaskID still points at the old id and pending_temp_id is
// the temp of the (already-created remotely) new task. The CRM marker
// parse above verifies the sync item belongs to the same contact + cadence
// kind, so advancing ExternalTaskID = item.ID is safe.
func (p *CadenceSyncProvider) tryRecoverPendingTempID(ctx context.Context, item SyncItem) (*repository.ContactTask, bool) {
	// Parse CRM marker from description to get contact ID + kind/lifecycle.
	type crmMarker struct {
		CRM       bool   `json:"crm"`
		ContactID string `json:"contact_id"`
		Kind      string `json:"kind"`
		Lifecycle string `json:"lifecycle"`
	}
	var marker crmMarker
	descToTry := item.Description
	if err := json.Unmarshal([]byte(descToTry), &marker); err != nil || !marker.CRM || marker.ContactID == "" {
		marker = crmMarker{}
		if idx := strings.LastIndex(item.Description, "{"); idx >= 0 {
			descToTry = item.Description[idx:]
		}
		if err := json.Unmarshal([]byte(descToTry), &marker); err != nil || !marker.CRM || marker.ContactID == "" {
			return nil, false
		}
	}

	contactID, err := uuid.Parse(marker.ContactID)
	if err != nil {
		return nil, false
	}

	// Translate legacy markers (no lifecycle field) into the new
	// (kind, lifecycle) shape. Recovery here is cadence-only; non-cadence
	// markers fall through the gate below. The unconditional translation
	// keeps the parsed marker fields useful for future debugging.
	if marker.Lifecycle == "" {
		switch marker.Kind {
		case "cadence", "":
			marker.Kind = contacttask.KindReachOut
			marker.Lifecycle = contacttask.LifecycleCadenceDue
		case "follow_up":
			marker.Kind = contacttask.KindReachOut
			marker.Lifecycle = contacttask.LifecycleFollowUpLoop
		case "action":
			marker.Kind = contacttask.KindAction
			marker.Lifecycle = contacttask.LifecycleManual
		default:
			return nil, false
		}
	}

	// Cadence-only recovery: follow-up and manual rows reconcile via the
	// post-create external_task_id path on the next sync tick.
	if marker.Lifecycle != contacttask.LifecycleCadenceDue {
		return nil, false
	}

	task, err := p.contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contactID, SourceName)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// No local row for this contact/kind — nothing to recover.
			return nil, false
		}
		// Any non-ErrNotFound failure (DB timeout, connection error) means
		// we can't tell whether a recoverable row exists. A stale local
		// row may match the skip-drift predicate against a real remote
		// item that was just delivered; surface recoveryFailed so Sync
		// forces deferSkipDrift=true and avoids emitting a duplicate
		// item_add this tick.
		logger.Warn().Err(err).
			Str("contactId", marker.ContactID).
			Msg("tryRecoverPendingTempID: lookup by contact failed")
		return nil, true
	}

	// Broadened guard: any managed task with a non-empty pending_temp_id
	// is a candidate for finalizing the mapping. See function docstring.
	pendingTempID, _ := task.Metadata[MetadataKeyPendingTempID].(string)
	if task.State != repository.ContactTaskStateManaged || pendingTempID == "" {
		return nil, false
	}

	oldID := task.ExternalTaskID

	// Atomic: external_id update + metadata clear inside one tx.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		logger.Warn().Err(err).
			Str("contactId", marker.ContactID).
			Msg("tryRecoverPendingTempID: begin tx failed")
		return task, true
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := p.contactTaskRepo.UpdateContactTaskExternalIDTx(ctx, tx, task.ID, item.ID); err != nil {
		logger.Warn().Err(err).
			Str("contactId", marker.ContactID).
			Str("oldExternalId", oldID).
			Str("newExternalId", item.ID).
			Msg("tryRecoverPendingTempID: update external_id failed")
		return task, true
	}

	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	delete(metadata, MetadataKeyPendingTempID)
	if _, err := p.contactTaskRepo.UpdateContactTaskMetadataTx(ctx, tx, task.ID, metadata); err != nil {
		logger.Warn().Err(err).Msg("tryRecoverPendingTempID: clear pending_temp_id failed")
		return task, true
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Warn().Err(err).Msg("tryRecoverPendingTempID: commit failed")
		return task, true
	}

	// Reflect in the returned task (in-memory).
	task.ExternalTaskID = item.ID
	task.Metadata = metadata

	logger.Info().
		Str("contactId", marker.ContactID).
		Str("oldTempId", oldID).
		Str("realId", item.ID).
		Msg("recovered pending temp ID mapping (atomic commit)")

	return task, false
}

// createTaskCommand creates a Todoist task creation command
func (p *CadenceSyncProvider) createTaskCommand(contact *repository.Contact, settings Settings, deadline *string) SyncCommand {
	// Embed CRM link in contact name for easy navigation
	contactLink := fmt.Sprintf("%s/contacts/%s", p.frontendURL, contact.ID.String())
	title := fmt.Sprintf("Reach out to [%s](%s)", contact.FullName, contactLink)

	// Build description with CRM marker (link is in title)
	var descBuilder strings.Builder

	marker := map[string]any{
		"crm":        true,
		"contact_id": contact.ID.String(),
		"kind":       contacttask.KindReachOut,
		"lifecycle":  contacttask.LifecycleCadenceDue,
		"instance":   settings.IntegrationInstanceID,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		logger.Error().Err(err).Str("contactId", contact.ID.String()).Msg("failed to marshal CRM marker - task may not sync properly")
	}
	descBuilder.Write(markerJSON)

	description := descBuilder.String()

	return NewItemAddCommand(
		title,
		description,
		settings.ProjectID,
		[]string{settings.LabelName},
		deadline,
	)
}

// processTempIDMappings updates contact_task records with real Todoist task
// IDs returned in a sync response's temp_id_mapping. Each per-task update
// runs in its own tx (external_id + pending_temp_id clear commit together
// or roll back together), eliminating the partial-commit ambiguity where
// ExternalTaskID advanced but pending_temp_id stayed stale.
//
// Returns rolledBack=true if any per-task tx rolled back during this
// invocation. Callers (Sync) thread that flag into reconcileContactTasks so
// the skip-drift recovery branch defers by one tick when a rollback leaves
// the row in a state indistinguishable from an HTTP-batch failure — the
// real Todoist task already exists server-side, so firing skip-drift this
// tick would emit a duplicate item_add. Next tick, tryRecoverPendingTempID
// finalizes via the real item arriving in syncResp.Items.
func (p *CadenceSyncProvider) processTempIDMappings(ctx context.Context, tempIDMap map[string]string) (rolledBack bool) {
	if len(tempIDMap) == 0 {
		return false
	}

	for tempID, realID := range tempIDMap {
		// Find the task by pending_temp_id
		task, err := p.contactTaskRepo.GetContactTaskByPendingTempID(ctx, SourceName, tempID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				// Expected when this temp_id wasn't from us.
				continue
			}
			// Any non-ErrNotFound lookup failure (DB timeout, connection
			// error) means we can't tell whether this mapping applies to
			// a local row. The safe move is to set rolledBack=true so the
			// skip-drift recovery branch defers by one tick — otherwise
			// this-tick reconcile could emit a duplicate item_add against
			// an already-created remote task.
			logger.Warn().Err(err).
				Str("tempId", tempID).
				Str("realId", realID).
				Msg("temp_id mapping: lookup failed; deferring reconcile skip-drift this tick")
			rolledBack = true
			continue
		}

		txErr := func() error {
			tx, err := p.pool.Begin(ctx)
			if err != nil {
				return fmt.Errorf("begin tx: %w", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			if _, err := p.contactTaskRepo.UpdateContactTaskExternalIDTx(ctx, tx, task.ID, realID); err != nil {
				return fmt.Errorf("update external_id: %w", err)
			}
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			delete(metadata, MetadataKeyPendingTempID)
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadataTx(ctx, tx, task.ID, metadata); err != nil {
				return fmt.Errorf("clear pending_temp_id: %w", err)
			}
			return tx.Commit(ctx)
		}()
		if txErr != nil {
			logger.Warn().Err(txErr).
				Str("tempId", tempID).
				Str("realId", realID).
				Str("contactTaskId", task.ID.String()).
				Msg("temp_id mapping: tx rolled back; deferring reconcile skip-drift this tick")
			rolledBack = true
			continue
		}

		logger.Debug().
			Str("tempId", tempID).
			Str("realId", realID).
			Str("contactTaskId", task.ID.String()).
			Msg("updated contact task with real Todoist ID (atomic commit)")
	}
	return rolledBack
}

// handleSyncError classifies and returns appropriate error
func (p *CadenceSyncProvider) handleSyncError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsAuthError() {
			return fmt.Errorf("authentication error: %w", err)
		}
		if apiErr.IsRateLimitError() {
			return fmt.Errorf("rate limited: %w", err)
		}
	}
	return err
}

// updateSyncStateMetadata updates the sync state metadata with new settings
func (p *CadenceSyncProvider) updateSyncStateMetadata(ctx context.Context, stateID uuid.UUID, settings Settings) error {
	metadata := map[string]any{
		MetadataKeyProjectID:           settings.ProjectID,
		MetadataKeyProjectName:         settings.ProjectName,
		MetadataKeyLabelID:             settings.LabelID,
		MetadataKeyLabelName:           settings.LabelName,
		MetadataKeyIntegrationInstance: settings.IntegrationInstanceID,
		MetadataKeyUserTimezone:        settings.UserTimezone,
	}

	_, err := p.syncRepo.UpdateSyncStateMetadata(ctx, stateID, metadata)
	return err
}

// Settings holds Todoist integration settings
type Settings struct {
	ProjectID             string `json:"project_id"`
	ProjectName           string `json:"project_name"`
	LabelID               string `json:"label_id"`
	LabelName             string `json:"label_name"`
	IntegrationInstanceID string `json:"integration_instance_id"`
	UserTimezone          string `json:"user_timezone"`
}

// getSettingsFromMetadata extracts settings from sync state metadata
func getSettingsFromMetadata(metadata map[string]any) Settings {
	settings := Settings{}
	if metadata == nil {
		return settings
	}

	if v, ok := metadata[MetadataKeyProjectID].(string); ok {
		settings.ProjectID = v
	}
	if v, ok := metadata[MetadataKeyProjectName].(string); ok {
		settings.ProjectName = v
	}
	if v, ok := metadata[MetadataKeyLabelID].(string); ok {
		settings.LabelID = v
	}
	if v, ok := metadata[MetadataKeyLabelName].(string); ok {
		settings.LabelName = v
	}
	if v, ok := metadata[MetadataKeyIntegrationInstance].(string); ok {
		settings.IntegrationInstanceID = v
	}
	if v, ok := metadata[MetadataKeyUserTimezone].(string); ok {
		settings.UserTimezone = v
	}

	return settings
}

// containsLabel checks if a label name is in a list of labels
func containsLabel(labels []string, labelName string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, labelName) {
			return true
		}
	}
	return false
}
