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
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
)

const (
	// SourceName is the source identifier for Todoist sync
	SourceName = "todoist"
	// TaskKindCadence is the task kind for contact cadence tasks
	TaskKindCadence = "cadence"
	// TaskKindAction is the task kind for one-off action tasks
	TaskKindAction = "action"
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
)

// DateFormat is the date format used for Todoist deadlines and synced_deadline metadata (YYYY-MM-DD)
const DateFormat = "2006-01-02"

// interactionRecorder defines the method for recording interactions (satisfied by ContactService)
type interactionRecorder interface {
	RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error)
}

// CadenceSyncProvider implements SyncProvider for Todoist cadence tasks
type CadenceSyncProvider struct {
	oauthService        *OAuthService
	contactTaskRepo     *repository.ContactTaskRepository
	contactRepo         *repository.ContactRepository
	syncRepo            *repository.SyncRepository
	interactionRecorder interactionRecorder
	frontendURL         string
}

// NewCadenceSyncProvider creates a new Todoist cadence sync provider
func NewCadenceSyncProvider(
	oauthService *OAuthService,
	contactTaskRepo *repository.ContactTaskRepository,
	contactRepo *repository.ContactRepository,
	syncRepo *repository.SyncRepository,
	cfg *config.Config,
	interactionRecorder interactionRecorder,
) *CadenceSyncProvider {
	return &CadenceSyncProvider{
		oauthService:        oauthService,
		contactTaskRepo:     contactTaskRepo,
		contactRepo:         contactRepo,
		syncRepo:            syncRepo,
		interactionRecorder: interactionRecorder,
		frontendURL:         cfg.CORS.FrontendURL,
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
	client := NewSyncClient(accessToken)

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

	// Process synced items
	var commands []SyncCommand
	processedTasks := 0

	for _, item := range syncResp.Items {
		processed, cmds := p.processItem(ctx, item, settings, accountID)
		if processed {
			processedTasks++
		}
		commands = append(commands, cmds...)
	}

	result.ItemsProcessed = processedTasks

	// If we have commands to execute, send them
	if len(commands) > 0 {
		for _, batch := range BatchCommands(commands, 100) {
			cmdResp, err := client.Sync(ctx, syncResp.SyncToken, []string{}, batch)
			if err != nil {
				logger.Warn().Err(err).Int("commands", len(batch)).Msg("failed to execute Todoist commands")
			} else if cmdResp != nil {
				p.processTempIDMappings(ctx, cmdResp.TempIDMap)
			}
		}
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

	// Reconcile: ensure all contacts with cadence have tasks
	reconcileCommands := p.reconcileContactTasks(ctx, client, settings, accountID)
	if len(reconcileCommands) > 0 {
		for _, batch := range BatchCommands(reconcileCommands, 100) {
			cmdResp, err := client.Sync(ctx, result.NewCursor, []string{}, batch)
			if err != nil {
				logger.Warn().Err(err).Int("commands", len(batch)).Msg("failed to execute reconciliation commands")
			} else if cmdResp != nil {
				p.processTempIDMappings(ctx, cmdResp.TempIDMap)
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

// processItem processes a single Todoist item from sync
func (p *CadenceSyncProvider) processItem(
	ctx context.Context,
	item SyncItem,
	settings Settings,
	accountID string,
) (bool, []SyncCommand) {
	var commands []SyncCommand

	// Find if this task is linked to a contact
	task, err := p.contactTaskRepo.GetContactTaskByExternalID(ctx, SourceName, item.ID)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			logger.Warn().Err(err).Str("taskId", item.ID).Msg("failed to find contact task")
			return false, nil
		}
		// Fallback: if processTempIDMappings failed, the contact_task still has a
		// temp ID while Todoist uses the real ID. Try to recover by matching the
		// CRM marker in the description to find a task with a pending temp ID.
		task = p.tryRecoverPendingTempID(ctx, item)
		if task == nil {
			return false, nil // Not a managed task
		}
	}

	// Skip unmanaged tasks
	if task.State != repository.ContactTaskStateManaged {
		return false, nil
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
		}
		return false, commands
	}

	// Check for recurring detection (transition to unmanaged)
	if item.Due != nil && item.Due.IsRecurring {
		logger.Info().
			Str("taskId", item.ID).
			Str("contactId", task.ContactID.String()).
			Msg("task became recurring, transitioning to unmanaged")
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
			logger.Warn().Err(err).Msg("failed to update task state to unmanaged")
		}
		return true, nil
	}

	// Check for completion
	if item.Checked {
		return p.handleTaskCompletion(ctx, item, task, contact, settings, accountID)
	}

	// Handle action tasks vs cadence tasks differently for skip triggers
	if task.Kind == TaskKindAction {
		return p.handleActionTaskTriggers(ctx, item, task, settings)
	}

	// Skip triggers for cadence tasks only
	skipTriggered := false

	// 1. Task deleted
	if item.IsDeleted {
		skipTriggered = true
		logger.Info().Str("taskId", item.ID).Str("reason", "deleted").Msg("skip trigger detected")
	}

	// 2. Label removed (check if our label is still present)
	if !skipTriggered && !containsLabel(item.Labels, settings.LabelName) {
		skipTriggered = true
		logger.Info().Str("taskId", item.ID).Str("reason", "label_removed").Msg("skip trigger detected")
	}

	// 3. Deadline removed
	if !skipTriggered && item.Deadline == nil {
		skipTriggered = true
		logger.Info().Str("taskId", item.ID).Str("reason", "deadline_removed").Msg("skip trigger detected")
	}

	if skipTriggered {
		return p.handleSkipTrigger(ctx, task, contact, settings, accountID)
	}

	// Check for deadline edit (Todoist wins)
	if item.Deadline != nil && contact.ContactBy != nil {
		todoistDeadline, err := time.Parse(DateFormat, item.Deadline.Date)
		if err == nil {
			// Compare dates using UTC year/month/day to avoid timezone issues
			// time.Parse returns UTC midnight, PostgreSQL DATE loads as UTC
			tY, tM, tD := todoistDeadline.UTC().Date()
			cY, cM, cD := contact.ContactBy.UTC().Date()
			if tY != cY || tM != cM || tD != cD {
				// Update CRM contact_by to match Todoist
				if err := p.contactRepo.UpdateContactBy(ctx, contact.ID, todoistDeadline); err != nil {
					logger.Warn().Err(err).Msg("failed to update contact_by from Todoist deadline")
				} else {
					// Also update synced_deadline to prevent reconciliation from treating
					// this Todoist-originated edit as CRM-initiated drift
					newDeadlineStr := todoistDeadline.Format(DateFormat)
					metadata := task.Metadata
					if metadata == nil {
						metadata = make(map[string]any)
					}
					metadata[MetadataKeySyncedDeadline] = newDeadlineStr
					if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
						logger.Warn().Err(err).Msg("failed to update synced_deadline from Todoist deadline edit")
					}

					logger.Info().
						Str("contactId", contact.ID.String()).
						Time("newContactBy", todoistDeadline).
						Msg("updated contact_by and synced_deadline from Todoist deadline edit")
				}
			}
		}
	}

	return true, commands
}

// handleTaskCompletion handles a completed Todoist task
func (p *CadenceSyncProvider) handleTaskCompletion(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	contact *repository.Contact,
	settings Settings,
	accountID string,
) (bool, []SyncCommand) {
	var commands []SyncCommand

	// Parse completion timestamp
	completedAt := accelerated.GetCurrentTime()
	if item.CompletedAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *item.CompletedAt); err == nil {
			completedAt = parsed
		}
	}

	// Record interaction (handles last_contacted update and dedup)
	sourceRef := task.ExternalTaskID
	_, err := p.interactionRecorder.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTodoist,
		SourceRef:  &sourceRef,
		OccurredAt: completedAt,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("failed to record interaction from Todoist completion")
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("taskKind", task.Kind).
		Time("lastContacted", completedAt).
		Msg("recorded interaction from Todoist completion")

	// Handle action tasks differently - mark as completed, no new task
	if task.Kind == TaskKindAction {
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateCompleted); err != nil {
			logger.Warn().Err(err).Msg("failed to update action task state to completed")
		}
		return true, nil
	}

	// Calculate next contact_by for cadence tasks
	if contact.Cadence != nil && *contact.Cadence != "" {
		cadenceType, err := cadence.ParseCadence(*contact.Cadence)
		if err == nil {
			// Use date-based cadence for Todoist deadlines
			// This ensures consistent behavior regardless of CRM_ENV
			days := cadence.CadenceDays(cadenceType)
			today := cadence.Today(completedAt)
			nextContactBy := today.AddDate(0, 0, days)

			// Update contact_by
			if err := p.contactRepo.UpdateContactBy(ctx, contact.ID, nextContactBy); err != nil {
				logger.Warn().Err(err).Msg("failed to update contact_by")
			}

			// Create next task
			deadlineStr := nextContactBy.Format(DateFormat)
			cmd := p.createTaskCommand(contact, settings, &deadlineStr)
			commands = append(commands, cmd)

			// Update metadata with temp_id, synced_deadline, and synced_last_contacted (preserving existing keys)
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[MetadataKeyPendingTempID] = cmd.TempID
			metadata[MetadataKeySyncedDeadline] = deadlineStr
			metadata[MetadataKeySyncedLastContacted] = completedAt.Format(time.RFC3339)
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Msg("failed to store pending_temp_id and synced_deadline in task metadata")
			}
		}
	}

	return true, commands
}

// handleSkipTrigger handles a skip trigger (task deleted, label removed, deadline removed)
func (p *CadenceSyncProvider) handleSkipTrigger(
	ctx context.Context,
	task *repository.ContactTask,
	contact *repository.Contact,
	settings Settings,
	accountID string,
) (bool, []SyncCommand) {
	var commands []SyncCommand

	// Calculate new contact_by using skip semantics
	if contact.Cadence != nil && *contact.Cadence != "" {
		cadenceType, err := cadence.ParseCadence(*contact.Cadence)
		if err == nil {
			// Skip pushes deadline out by a full cadence period
			// Use the later of: (skipped contact_by + cadence) or (today + cadence)
			days := cadence.CadenceDays(cadenceType)
			now := accelerated.GetCurrentTime()
			today := cadence.Today(now)

			// Option 1: today + cadence
			fromToday := today.AddDate(0, 0, days)

			// Option 2: skipped contact_by + cadence
			fromSkipped := fromToday // fallback if no contact_by
			if contact.ContactBy != nil {
				fromSkipped = contact.ContactBy.AddDate(0, 0, days)
			}

			// Use whichever is farther in the future
			nextContactBy := fromToday
			if fromSkipped.After(fromToday) {
				nextContactBy = fromSkipped
			}

			// Update contact_by
			if err := p.contactRepo.UpdateContactBy(ctx, contact.ID, nextContactBy); err != nil {
				logger.Warn().Err(err).Msg("failed to update contact_by after skip")
			}

			// Create new task
			deadlineStr := nextContactBy.Format(DateFormat)
			cmd := p.createTaskCommand(contact, settings, &deadlineStr)
			commands = append(commands, cmd)

			// Update metadata with temp_id, synced_deadline, and synced_last_contacted (preserving existing keys)
			metadata := task.Metadata
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[MetadataKeyPendingTempID] = cmd.TempID
			metadata[MetadataKeySyncedDeadline] = deadlineStr
			if contact.LastContacted != nil {
				metadata[MetadataKeySyncedLastContacted] = contact.LastContacted.Format(time.RFC3339)
			}
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Msg("failed to store pending_temp_id and synced_deadline in task metadata")
			}

			logger.Info().
				Str("contactId", contact.ID.String()).
				Time("newContactBy", nextContactBy).
				Msg("processed skip trigger, created new task")
		}
	}

	return true, commands
}

// handleActionTaskTriggers handles unmanagement triggers for action tasks
// Action tasks are simpler than cadence tasks - they just transition to unmanaged
// when the label is removed or the task is deleted. No skip semantics.
func (p *CadenceSyncProvider) handleActionTaskTriggers(
	ctx context.Context,
	item SyncItem,
	task *repository.ContactTask,
	settings Settings,
) (bool, []SyncCommand) {
	// Task deleted - mark as unmanaged
	if item.IsDeleted {
		logger.Info().
			Str("taskId", item.ID).
			Str("contactTaskId", task.ID.String()).
			Msg("action task deleted in Todoist, marking as unmanaged")
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
			logger.Warn().Err(err).Msg("failed to update action task state to unmanaged")
		}
		return true, nil
	}

	// Label removed - mark as unmanaged
	if !containsLabel(item.Labels, settings.LabelName) {
		logger.Info().
			Str("taskId", item.ID).
			Str("contactTaskId", task.ID.String()).
			Msg("CRM label removed from action task, marking as unmanaged")
		if _, err := p.contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged); err != nil {
			logger.Warn().Err(err).Msg("failed to update action task state to unmanaged")
		}
		return true, nil
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

	return true, nil
}

// reconcileContactTasks ensures all contacts with cadence have managed tasks
// and that existing tasks have deadlines matching the contact's contact_by.
func (p *CadenceSyncProvider) reconcileContactTasks(
	ctx context.Context,
	client *SyncClient,
	settings Settings,
	accountID string,
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

		currentDeadline := contact.ContactBy.Format(DateFormat)

		// Check if contact has a managed task
		task, err := p.contactTaskRepo.GetContactTaskByContact(ctx, contact.ID, SourceName, TaskKindCadence)
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
			_, err := p.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
				ContactID:      contact.ID,
				Provider:       SourceName,
				Kind:           TaskKindCadence,
				ExternalTaskID: cmd.TempID, // Will be updated on sync response
				State:          string(repository.ContactTaskStateManaged),
				Metadata:       taskMetadata,
			})
			if err != nil {
				logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to create contact task")
			}
			continue
		}

		// Check if task is unmanaged (skip it)
		if task.State != repository.ContactTaskStateManaged {
			continue
		}

		// Task exists and is managed - check if deadline needs updating
		cmds := p.reconcileExistingTask(ctx, task, &contact, settings, currentDeadline)
		commands = append(commands, cmds...)
	}

	return commands
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
) []SyncCommand {
	var commands []SyncCommand

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
			if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
				logger.Warn().Err(err).Str("contactId", contact.ID.String()).Msg("failed to backfill synced_last_contacted")
			} else {
				// Update task in memory so wasContactedSinceSync can use it this cycle
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

			// Calculate the next contact_by from the current last_contacted
			if contact.Cadence != nil && *contact.Cadence != "" {
				cadenceType, err := cadence.ParseCadence(*contact.Cadence)
				if err == nil {
					days := cadence.CadenceDays(cadenceType)
					today := cadence.Today(*contact.LastContacted)
					nextContactBy := today.AddDate(0, 0, days)

					// Update contact_by
					if err := p.contactRepo.UpdateContactBy(ctx, contact.ID, nextContactBy); err != nil {
						logger.Warn().Err(err).Msg("failed to update contact_by after non-Todoist contact")
					}

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
					if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
						logger.Warn().Err(err).Msg("failed to update metadata after non-Todoist contact")
					}
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
// It parses the CRM marker in the item description to find the contact, then checks
// if there's a managed cadence task with a pending temp ID for that contact.
// If found, it migrates the external_task_id to the real ID.
func (p *CadenceSyncProvider) tryRecoverPendingTempID(ctx context.Context, item SyncItem) *repository.ContactTask {
	// Parse CRM marker from description to get contact ID
	var marker struct {
		CRM       bool   `json:"crm"`
		ContactID string `json:"contact_id"`
		Kind      string `json:"kind"`
	}
	descToTry := item.Description
	if err := json.Unmarshal([]byte(descToTry), &marker); err != nil || !marker.CRM || marker.ContactID == "" {
		marker = struct {
			CRM       bool   `json:"crm"`
			ContactID string `json:"contact_id"`
			Kind      string `json:"kind"`
		}{}
		if idx := strings.LastIndex(item.Description, "{"); idx >= 0 {
			descToTry = item.Description[idx:]
		}
		if err := json.Unmarshal([]byte(descToTry), &marker); err != nil || !marker.CRM || marker.ContactID == "" {
			return nil
		}
	}

	contactID, err := uuid.Parse(marker.ContactID)
	if err != nil {
		return nil
	}

	// Only recover cadence tasks
	kind := marker.Kind
	if kind == "" {
		kind = TaskKindCadence
	}
	if kind != TaskKindCadence {
		return nil
	}

	task, err := p.contactTaskRepo.GetContactTaskByContact(ctx, contactID, SourceName, kind)
	if err != nil {
		return nil
	}

	// Only recover tasks that still have a pending temp ID
	if task.State != repository.ContactTaskStateManaged || !isPendingTempID(task) {
		return nil
	}

	// Migrate external_task_id to the real Todoist ID
	oldID := task.ExternalTaskID
	if _, err := p.contactTaskRepo.UpdateContactTaskExternalID(ctx, task.ID, item.ID); err != nil {
		logger.Warn().Err(err).
			Str("contactId", marker.ContactID).
			Str("oldExternalId", oldID).
			Str("newExternalId", item.ID).
			Msg("failed to recover pending temp ID")
		return task // still process with old ID
	}

	task.ExternalTaskID = item.ID

	// Clear pending_temp_id from metadata
	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	delete(metadata, MetadataKeyPendingTempID)
	if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
		logger.Warn().Err(err).Msg("failed to clear pending_temp_id after recovery")
	}

	logger.Info().
		Str("contactId", marker.ContactID).
		Str("oldTempId", oldID).
		Str("realId", item.ID).
		Msg("recovered pending temp ID mapping")

	return task
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
		"kind":       TaskKindCadence,
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

// processTempIDMappings updates contact_task records with real Todoist task IDs
func (p *CadenceSyncProvider) processTempIDMappings(ctx context.Context, tempIDMap map[string]string) {
	if len(tempIDMap) == 0 {
		return
	}

	for tempID, realID := range tempIDMap {
		// Find the task by pending_temp_id
		task, err := p.contactTaskRepo.GetContactTaskByPendingTempID(ctx, SourceName, tempID)
		if err != nil {
			// Not found is expected if this temp_id wasn't from us
			continue
		}

		// Update with real Todoist task ID
		_, err = p.contactTaskRepo.UpdateContactTaskExternalID(ctx, task.ID, realID)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("tempId", tempID).
				Str("realId", realID).
				Msg("failed to update external task ID")
			continue
		}

		// Clear pending_temp_id from metadata (preserves synced_deadline and other keys)
		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		delete(metadata, MetadataKeyPendingTempID)
		if _, err := p.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
			logger.Warn().Err(err).Msg("failed to clear pending_temp_id from metadata")
		}

		logger.Debug().
			Str("tempId", tempID).
			Str("realId", realID).
			Str("contactTaskId", task.ID.String()).
			Msg("updated contact task with real Todoist ID")
	}
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
