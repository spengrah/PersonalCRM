package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// todoistSettingsFunc is a function that returns Todoist settings and access token.
// Extracted as a type so tests can inject a fake without needing a real OAuthService.
type todoistSettingsFunc func(ctx context.Context) (*todoist.Settings, string, error)

// FollowUpService manages follow-up Todoist tasks for outbound interactions
type FollowUpService struct {
	contactTaskRepo     *repository.ContactTaskRepository
	contactRepo         *repository.ContactRepository
	syncRepo            *repository.SyncRepository
	oauthService        *todoist.OAuthService
	cfg                 *config.Config
	todoistClientFunc   todoist.ClientFactory
	todoistSettingsFunc todoistSettingsFunc
	frontendURL         string
}

// NewFollowUpService creates a new follow-up service
func NewFollowUpService(
	contactTaskRepo *repository.ContactTaskRepository,
	contactRepo *repository.ContactRepository,
	syncRepo *repository.SyncRepository,
	oauthService *todoist.OAuthService,
	cfg *config.Config,
) *FollowUpService {
	svc := &FollowUpService{
		contactTaskRepo:   contactTaskRepo,
		contactRepo:       contactRepo,
		syncRepo:          syncRepo,
		oauthService:      oauthService,
		cfg:               cfg,
		todoistClientFunc: todoist.DefaultClientFactory,
		frontendURL:       cfg.CORS.FrontendURL,
	}
	svc.todoistSettingsFunc = svc.getTodoistSettings
	return svc
}

// SetTodoistClientFactory allows overriding the Todoist client factory (for testing)
func (s *FollowUpService) SetTodoistClientFactory(factory todoist.ClientFactory) {
	s.todoistClientFunc = factory
}

// SetTodoistSettingsFunc allows overriding the settings lookup (for testing)
func (s *FollowUpService) SetTodoistSettingsFunc(fn todoistSettingsFunc) {
	s.todoistSettingsFunc = fn
}

// CreateOrRefreshFollowUp creates or refreshes a follow-up task for a contact after an outbound interaction
func (s *FollowUpService) CreateOrRefreshFollowUp(ctx context.Context, contact repository.Contact, outreachAt time.Time) error {
	_, err := s.CreateOrRefreshFollowUpObserved(ctx, contact, outreachAt)
	return err
}

// CreateOrRefreshFollowUpObserved is the result-returning variant of
// CreateOrRefreshFollowUp. Returns the action taken, the touched
// contact_task id, and the deadline used — the data the direct-path
// shadow drain needs to build a writer='direct' observation row.
// Existing callers that ignore the result continue to use
// CreateOrRefreshFollowUp.
func (s *FollowUpService) CreateOrRefreshFollowUpObserved(ctx context.Context, contact repository.Contact, outreachAt time.Time) (FollowUpActionResult, error) {
	// No follow-up if contact has no cadence
	if contact.Cadence == nil || *contact.Cadence == "" {
		logger.Debug().
			Str("contactId", contact.ID.String()).
			Msg("skipping follow-up: no cadence")
		return FollowUpActionResult{Action: repository.FollowUpActionSkip}, nil
	}

	// Calculate deadline using date arithmetic (not duration)
	days := watchdogDaysForCadence(*contact.Cadence, s.cfg.Watchdog)
	if days == 0 {
		return FollowUpActionResult{Action: repository.FollowUpActionSkip}, nil
	}
	deadline := cadence.Today(outreachAt).AddDate(0, 0, days)
	deadlineStr := deadline.Format(todoist.DateFormat)

	// Check for existing pending follow-up
	existing, err := s.contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return FollowUpActionResult{}, fmt.Errorf("find pending follow-up: %w", err)
	}

	if existing != nil {
		// Refresh existing follow-up: update Todoist due date
		if err := s.refreshFollowUp(ctx, existing, deadlineStr, contact); err != nil {
			return FollowUpActionResult{}, err
		}
		id := existing.ID
		d := deadline
		return FollowUpActionResult{
			Action:        repository.FollowUpActionRefresh,
			ContactTaskID: &id,
			Deadline:      &d,
		}, nil
	}

	// Create new follow-up
	createdID, err := s.createFollowUp(ctx, contact, deadlineStr)
	d := deadline
	result := FollowUpActionResult{
		Action:   repository.FollowUpActionCreate,
		Deadline: &d,
	}
	if createdID != uuid.Nil {
		id := createdID
		result.ContactTaskID = &id
	}
	return result, err
}

// CompleteFollowUp completes any pending follow-up task for a contact (when a response arrives)
func (s *FollowUpService) CompleteFollowUp(ctx context.Context, contactID uuid.UUID) error {
	_, err := s.CompleteFollowUpObserved(ctx, contactID)
	return err
}

// CompleteFollowUpObserved is the result-returning variant of
// CompleteFollowUp. Returns action=complete with the touched task id
// when a pending follow-up existed; action=skip with empty fields when
// none was found. Used by the direct-path shadow drain.
func (s *FollowUpService) CompleteFollowUpObserved(ctx context.Context, contactID uuid.UUID) (FollowUpActionResult, error) {
	existing, err := s.contactTaskRepo.FindPendingFollowUp(ctx, contactID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return FollowUpActionResult{Action: repository.FollowUpActionSkip}, nil
		}
		return FollowUpActionResult{}, fmt.Errorf("find pending follow-up: %w", err)
	}
	result := FollowUpActionResult{Action: repository.FollowUpActionComplete}
	id := existing.ID
	result.ContactTaskID = &id
	if err := s.completeFollowUpInner(ctx, contactID, existing); err != nil {
		return result, err
	}
	return result, nil
}

// completeFollowUpInner factored from CompleteFollowUp — performs the
// Todoist close + state transition for an already-located pending
// task. Keeping it package-private avoids exposing the existing task
// struct on the public surface.
func (s *FollowUpService) completeFollowUpInner(ctx context.Context, contactID uuid.UUID, existing *repository.ContactTask) error {

	// Close the Todoist task (best-effort — complete locally even if Todoist is unavailable)
	settings, accessToken, err := s.todoistSettingsFunc(ctx)
	if err != nil {
		if errors.Is(err, ErrNoTodoistAccount) || errors.Is(err, ErrTodoistNotConfigured) || errors.Is(err, ErrTodoistMissingLabel) {
			// Todoist not available — complete the local record only
			_, localErr := s.contactTaskRepo.CompleteFollowUpForContact(ctx, contactID)
			if localErr != nil {
				return fmt.Errorf("complete follow-up locally (no todoist): %w", localErr)
			}
			logger.Info().
				Str("contactId", contactID.String()).
				Str("todoistTaskId", existing.ExternalTaskID).
				Msg("follow-up completed locally (todoist unavailable)")
			return nil
		}
		return fmt.Errorf("get todoist settings: %w", err)
	}
	_ = settings // validates Todoist is configured; only accessToken needed for close

	// Attempt to close the Todoist task, but always complete locally regardless.
	// A transient Todoist failure must not leave the contact stuck in "awaiting reply" state.
	//
	// If the close fails, we set todoist_close_pending in metadata FIRST (before state change),
	// so RetryPendingCloses can find it. The state change (CompleteFollowUpForContact) only
	// updates state+updated_at, preserving the metadata flag.
	client := s.todoistClientFunc(accessToken)
	closeCmd := todoist.NewItemCloseCommand(existing.ExternalTaskID)
	_, todoistErr := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{closeCmd})
	if todoistErr != nil {
		logger.Warn().Err(todoistErr).
			Str("contactId", contactID.String()).
			Str("todoistTaskId", existing.ExternalTaskID).
			Msg("failed to close todoist follow-up task — marking for retry")
		// Set retry flag BEFORE state transition so it persists on the completed record
		metadata := existing.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		metadata["todoist_close_pending"] = true
		if _, metaErr := s.contactTaskRepo.UpdateContactTaskMetadata(ctx, existing.ID, metadata); metaErr != nil {
			// If we can't set the retry flag, the Todoist task will stay open with no automatic retry.
			// This is the lesser evil vs. leaving the CRM stuck in "awaiting reply" state.
			logger.Error().Err(metaErr).
				Str("contactId", contactID.String()).
				Str("todoistTaskId", existing.ExternalTaskID).
				Msg("failed to set todoist_close_pending flag — remote task may remain open with no retry")
		}
	}

	// Always mark local task as completed (preserves metadata including any todoist_close_pending flag)
	_, err = s.contactTaskRepo.CompleteFollowUpForContact(ctx, contactID)
	if err != nil {
		return fmt.Errorf("complete follow-up locally: %w", err)
	}

	logger.Info().
		Str("contactId", contactID.String()).
		Str("todoistTaskId", existing.ExternalTaskID).
		Bool("todoistClosePending", todoistErr != nil).
		Msg("follow-up completed (response received)")

	return nil
}

// RetryPendingCloses retries Todoist close calls that failed on previous attempts.
// Called during the sync reconciliation loop to clean up stale Todoist tasks.
func (s *FollowUpService) RetryPendingCloses(ctx context.Context) {
	tasks, err := s.contactTaskRepo.ListFollowUpsWithPendingClose(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to list follow-ups with pending close")
		return
	}

	if len(tasks) == 0 {
		return
	}

	_, accessToken, err := s.todoistSettingsFunc(ctx)
	if err != nil {
		return // Todoist not configured, nothing to retry
	}

	client := s.todoistClientFunc(accessToken)
	for _, task := range tasks {
		closeCmd := todoist.NewItemCloseCommand(task.ExternalTaskID)
		_, err := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{closeCmd})
		if err != nil {
			logger.Warn().Err(err).
				Str("taskId", task.ID.String()).
				Str("todoistTaskId", task.ExternalTaskID).
				Msg("retry: still cannot close todoist follow-up task")
			continue
		}

		// Success — clear the pending flag
		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		delete(metadata, "todoist_close_pending")
		if _, err := s.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
			logger.Warn().Err(err).Str("taskId", task.ID.String()).Msg("failed to clear todoist_close_pending flag")
		} else {
			logger.Info().
				Str("taskId", task.ID.String()).
				Str("todoistTaskId", task.ExternalTaskID).
				Msg("retry: successfully closed todoist follow-up task")
		}
	}
}

func (s *FollowUpService) refreshFollowUp(ctx context.Context, task *repository.ContactTask, newDeadline string, contact repository.Contact) error {
	settings, accessToken, err := s.todoistSettingsFunc(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	_ = settings

	// Update Todoist task due date
	client := s.todoistClientFunc(accessToken)
	updateCmd := todoist.NewItemUpdateCommand(task.ExternalTaskID, map[string]any{
		"deadline": map[string]string{"date": newDeadline},
	})
	_, err = client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{updateCmd})
	if err != nil {
		return fmt.Errorf("update todoist follow-up deadline: %w", err)
	}

	// Update local metadata
	metadata := task.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	oldDeadline, _ := metadata["due_date"].(string)
	metadata["due_date"] = newDeadline
	if _, err := s.contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata); err != nil {
		return fmt.Errorf("update follow-up metadata: %w", err)
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("contactName", contact.FullName).
		Str("oldDeadline", oldDeadline).
		Str("newDeadline", newDeadline).
		Str("todoistTaskId", task.ExternalTaskID).
		Msg("follow-up refreshed")

	return nil
}

func (s *FollowUpService) createFollowUp(ctx context.Context, contact repository.Contact, deadline string) (uuid.UUID, error) {
	settings, accessToken, err := s.todoistSettingsFunc(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get todoist settings: %w", err)
	}

	// Build follow-up task content
	contactLink := fmt.Sprintf("%s/contacts/%s", s.frontendURL, contact.ID.String())
	content := fmt.Sprintf("Follow up: [%s](%s) (awaiting reply)", contact.FullName, contactLink)

	// Build description with CRM marker
	marker := map[string]any{
		"crm":        true,
		"contact_id": contact.ID.String(),
		"kind":       todoist.TaskKindFollowUp,
		"instance":   settings.IntegrationInstanceID,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal marker: %w", err)
	}

	// Create task via Sync API
	client := s.todoistClientFunc(accessToken)
	cmd := todoist.NewItemAddCommand(
		content,
		string(markerJSON),
		settings.ProjectID,
		[]string{settings.LabelName},
		&deadline,
	)

	resp, err := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{cmd})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create todoist follow-up task: %w", err)
	}

	// Get real task ID from temp ID mapping
	realID, ok := resp.TempIDMap[cmd.TempID]
	if !ok {
		// If TempIDMap doesn't contain the mapping, the command likely failed on Todoist's side.
		// Don't store a temp ID as external_task_id — it would never be resolved since follow-up
		// tasks aren't part of the cadence sync loop that resolves pending_temp_id metadata.
		return uuid.Nil, fmt.Errorf("todoist did not return task ID for follow-up (temp_id: %s)", cmd.TempID)
	}

	// Create local contact_task record
	metadata := map[string]any{
		"due_date": deadline,
		"content":  content,
	}
	created, err := s.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       todoist.SourceName,
		Kind:           todoist.TaskKindFollowUp,
		ExternalTaskID: realID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       metadata,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create follow-up contact_task: %w", err)
	}

	logger.Info().
		Str("contactId", contact.ID.String()).
		Str("contactName", contact.FullName).
		Str("cadence", *contact.Cadence).
		Str("deadline", deadline).
		Str("todoistTaskId", realID).
		Msg("follow-up created")

	return created.ID, nil
}

func (s *FollowUpService) getTodoistSettings(ctx context.Context) (*todoist.Settings, string, error) {
	accounts, err := s.oauthService.ListAccounts(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list todoist accounts: %w", err)
	}
	if len(accounts) == 0 {
		return nil, "", ErrNoTodoistAccount
	}

	accountID := accounts[0].AccountID
	accessToken, err := s.oauthService.GetAccessToken(ctx, accountID)
	if err != nil {
		return nil, "", fmt.Errorf("get access token: %w", err)
	}

	state, err := s.syncRepo.GetSyncStateBySource(ctx, todoist.SourceName, &accountID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, "", ErrTodoistNotConfigured
		}
		return nil, "", fmt.Errorf("get sync state: %w", err)
	}

	settings := todoist.Settings{}
	if state.Metadata != nil {
		if v, ok := state.Metadata[todoist.MetadataKeyProjectID].(string); ok {
			settings.ProjectID = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyProjectName].(string); ok {
			settings.ProjectName = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyLabelID].(string); ok {
			settings.LabelID = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyLabelName].(string); ok {
			settings.LabelName = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyIntegrationInstance].(string); ok {
			settings.IntegrationInstanceID = v
		}
	}

	if settings.LabelID == "" {
		return nil, "", ErrTodoistMissingLabel
	}

	return &settings, accessToken, nil
}

// watchdogDaysForCadence returns the number of days for the follow-up watchdog window based on cadence
func watchdogDaysForCadence(cadenceStr string, cfg config.WatchdogConfig) int {
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
