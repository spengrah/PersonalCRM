package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// Sentinel errors for contact task operations
var (
	ErrNoTodoistAccount     = errors.New("no todoist account connected")
	ErrTodoistNotConfigured = errors.New("todoist settings not configured")
	ErrTodoistMissingLabel  = errors.New("todoist settings not configured: missing label")
	ErrInvalidManualKind    = errors.New("invalid manual task kind")
)

// ValidManualKinds is the set of user-pickable kinds for CreateManualTask.
var ValidManualKinds = map[string]bool{
	contacttask.KindReachOut: true,
	contacttask.KindSend:     true,
	contacttask.KindReminder: true,
}

// ContactTaskService handles business logic for contact tasks
type ContactTaskService struct {
	contactTaskRepo   *repository.ContactTaskRepository
	contactRepo       *repository.ContactRepository
	syncRepo          *repository.SyncRepository
	oauthService      *todoist.OAuthService
	frontendURL       string
	todoistClientFunc todoist.ClientFactory
	// testAccessToken bypasses OAuth lookup when set (for testing)
	testAccessToken string
}

// NewContactTaskService creates a new contact task service
func NewContactTaskService(
	contactTaskRepo *repository.ContactTaskRepository,
	contactRepo *repository.ContactRepository,
	syncRepo *repository.SyncRepository,
	oauthService *todoist.OAuthService,
	cfg *config.Config,
) *ContactTaskService {
	return &ContactTaskService{
		contactTaskRepo:   contactTaskRepo,
		contactRepo:       contactRepo,
		syncRepo:          syncRepo,
		oauthService:      oauthService,
		frontendURL:       cfg.CORS.FrontendURL,
		todoistClientFunc: todoist.DefaultClientFactory,
	}
}

// SetTodoistClientFactory allows overriding the Todoist client factory (for testing)
func (s *ContactTaskService) SetTodoistClientFactory(factory todoist.ClientFactory) {
	s.todoistClientFunc = factory
}

// NewContactTaskServiceForTest creates a service for testing without OAuth dependency.
// The service's todoistClientFunc must be set via SetTodoistClientFactory before use.
func NewContactTaskServiceForTest(
	contactTaskRepo *repository.ContactTaskRepository,
	contactRepo *repository.ContactRepository,
	syncRepo *repository.SyncRepository,
	frontendURL string,
) *ContactTaskService {
	return &ContactTaskService{
		contactTaskRepo:   contactTaskRepo,
		contactRepo:       contactRepo,
		syncRepo:          syncRepo,
		frontendURL:       frontendURL,
		todoistClientFunc: todoist.DefaultClientFactory,
		testAccessToken:   "test-token", // Bypass OAuth lookup
	}
}

// CreateManualTaskRequest contains the parameters for creating a manual task.
// Kind must be one of the user-pickable values: reach_out / send / reminder.
type CreateManualTaskRequest struct {
	ContactID uuid.UUID
	Kind      string // reach_out | send | reminder
	Text      string // Quick add text with optional dates, #project, @label, p1-p4
	Notes     string // Optional notes for the task description
}

// ManualTaskResponse represents the response for a manual task.
type ManualTaskResponse struct {
	ID             uuid.UUID `json:"id"`
	ContactID      uuid.UUID `json:"contact_id"`
	Kind           string    `json:"kind"`
	Lifecycle      string    `json:"lifecycle"`
	ExternalTaskID string    `json:"external_task_id"`
	Content        string    `json:"content"`
	DueDate        *string   `json:"due_date,omitempty"`
	ProjectID      string    `json:"project_id,omitempty"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateManualTask creates a new user-picker task for a contact via the
// Todoist Quick Add API. Always sets Lifecycle = LifecycleManual; Kind is
// validated against the user-pickable set.
func (s *ContactTaskService) CreateManualTask(ctx context.Context, req CreateManualTaskRequest) (*ManualTaskResponse, error) {
	if !ValidManualKinds[req.Kind] {
		return nil, fmt.Errorf("%w: %q", ErrInvalidManualKind, req.Kind)
	}

	// Verify contact exists
	contact, err := s.contactRepo.GetContact(ctx, req.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	// Get Todoist settings and access token
	var settings *todoist.Settings
	var accessToken string
	if s.testAccessToken != "" {
		// Test mode: use test token and get settings from sync state directly
		accessToken = s.testAccessToken
		settings, _, err = s.getTodoistSettingsFromSyncState(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		// Production mode: get settings and token via OAuth
		var accountID string
		settings, accountID, err = s.getTodoistSettings(ctx)
		if err != nil {
			return nil, err
		}
		accessToken, err = s.oauthService.GetAccessToken(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("get access token: %w", err)
		}
	}

	// Build the Quick Add text with contact name link prefix, project, and label
	contactLink := fmt.Sprintf("%s/contacts/%s", s.frontendURL, contact.ID.String())
	quickAddText := fmt.Sprintf("[%s](%s): %s", contact.FullName, contactLink, req.Text)

	// Add default project if user didn't specify one with #
	if settings.ProjectName != "" && !strings.Contains(req.Text, "#") {
		quickAddText = fmt.Sprintf("%s #%s", quickAddText, settings.ProjectName)
	}

	// Add CRM label if not already specified
	if settings.LabelName != "" && !strings.Contains(strings.ToLower(req.Text), "@"+strings.ToLower(settings.LabelName)) {
		quickAddText = fmt.Sprintf("%s @%s", quickAddText, settings.LabelName)
	}

	// Build the task description (CRM link is in content, so just notes + marker here)
	var descBuilder strings.Builder
	if req.Notes != "" {
		descBuilder.WriteString(req.Notes)
		descBuilder.WriteString("\n\n---\n")
	}

	// Add CRM marker for sync identification (use json.Marshal for safety).
	// Both kind and lifecycle are written; readers that ignore lifecycle
	// continue to work and the backfill path translates legacy markers.
	marker := map[string]any{
		"crm":        true,
		"contact_id": contact.ID.String(),
		"kind":       req.Kind,
		"lifecycle":  contacttask.LifecycleManual,
		"instance":   settings.IntegrationInstanceID,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return nil, fmt.Errorf("marshal marker: %w", err)
	}
	descBuilder.Write(markerJSON)

	// Step 1: Call Todoist Quick Add API (for natural language parsing)
	client := s.todoistClientFunc(accessToken)
	task, err := client.QuickAdd(ctx, quickAddText, "")
	if err != nil {
		return nil, fmt.Errorf("todoist quick add: %w", err)
	}

	// Step 2: Update task description via Sync API
	// QuickAdd's "note" param creates comments, not descriptions.
	// Description contains CRM marker needed for sync reconciliation - fail if update fails.
	updateCmd := todoist.NewItemUpdateCommand(task.ID, map[string]any{
		"description": descBuilder.String(),
	})
	_, err = client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{updateCmd})
	if err != nil {
		// Delete the task since it won't have CRM marker for sync
		deleteCmd := todoist.NewItemDeleteCommand(task.ID)
		_, _ = client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{deleteCmd})
		return nil, fmt.Errorf("update task description: %w", err)
	}

	// Build metadata for the contact_task record
	metadata := map[string]any{
		"content": task.Content,
	}
	if task.Deadline != nil {
		metadata["due_date"] = task.Deadline.Date
	} else if task.Due != nil {
		metadata["due_date"] = task.Due.Date
	}
	if task.ProjectID != "" {
		metadata["project_id"] = task.ProjectID
	}

	// Create contact_task record. Lifecycle is always LifecycleManual for
	// user-picker tasks; Kind matches the validated request kind.
	contactTask, err := s.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      req.ContactID,
		Provider:       todoist.SourceName,
		Kind:           req.Kind,
		Lifecycle:      contacttask.LifecycleManual,
		ExternalTaskID: task.ID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       metadata,
	})
	if err != nil {
		// Best-effort cleanup: the external Todoist task was created but
		// the local row didn't land. Delete the orphan so a subsequent
		// sync tick doesn't try to reconcile it.
		deleteCmd := todoist.NewItemDeleteCommand(task.ID)
		_, _ = client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{deleteCmd})
		return nil, fmt.Errorf("create contact task: %w", err)
	}

	// Build response
	response := &ManualTaskResponse{
		ID:             contactTask.ID,
		ContactID:      contactTask.ContactID,
		Kind:           contactTask.Kind,
		Lifecycle:      contactTask.Lifecycle,
		ExternalTaskID: contactTask.ExternalTaskID,
		Content:        task.Content,
		ProjectID:      task.ProjectID,
		State:          string(contactTask.State),
		CreatedAt:      contactTask.CreatedAt,
	}

	if task.Deadline != nil {
		response.DueDate = &task.Deadline.Date
	} else if task.Due != nil {
		response.DueDate = &task.Due.Date
	}

	return response, nil
}

// ListContactTasks lists tasks for a contact with optional filters
func (s *ContactTaskService) ListContactTasks(ctx context.Context, contactID uuid.UUID, state *string, kind *string, lifecycle *string) ([]repository.ContactTask, error) {
	// Verify contact exists
	_, err := s.contactRepo.GetContact(ctx, contactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	return s.contactTaskRepo.ListContactTasksFiltered(ctx, contactID, state, kind, lifecycle)
}

// DeleteTaskLink removes the CRM tracking for a task (does not delete from Todoist)
func (s *ContactTaskService) DeleteTaskLink(ctx context.Context, contactID uuid.UUID, taskID uuid.UUID) error {
	// Verify contact exists
	_, err := s.contactRepo.GetContact(ctx, contactID)
	if err != nil {
		return fmt.Errorf("get contact: %w", err)
	}

	// Get the task to verify it belongs to this contact
	task, err := s.contactTaskRepo.GetContactTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	if task.ContactID != contactID {
		return db.ErrNotFound
	}

	return s.contactTaskRepo.DeleteContactTask(ctx, taskID)
}

// getTodoistSettingsFromSyncState retrieves settings directly from sync state (for testing)
func (s *ContactTaskService) getTodoistSettingsFromSyncState(ctx context.Context) (*todoist.Settings, string, error) {
	// Find any Todoist sync state (test mode doesn't need specific account)
	allStates, err := s.syncRepo.ListSyncStates(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list sync states: %w", err)
	}

	// Filter to Todoist states
	var state *repository.SyncState
	for i := range allStates {
		if allStates[i].Source == todoist.SourceName {
			state = &allStates[i]
			break
		}
	}

	if state == nil {
		return nil, "", ErrTodoistNotConfigured
	}
	accountID := ""
	if state.AccountID != nil {
		accountID = *state.AccountID
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

	return &settings, accountID, nil
}

// getTodoistSettings retrieves the Todoist settings from sync state
func (s *ContactTaskService) getTodoistSettings(ctx context.Context) (*todoist.Settings, string, error) {
	// Get Todoist accounts
	accounts, err := s.oauthService.ListAccounts(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list todoist accounts: %w", err)
	}

	if len(accounts) == 0 {
		return nil, "", ErrNoTodoistAccount
	}

	accountID := accounts[0].AccountID

	// Get sync state for settings
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

	return &settings, accountID, nil
}
