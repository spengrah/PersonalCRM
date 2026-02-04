package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// Sentinel errors for contact task operations
var (
	ErrNoTodoistAccount     = errors.New("no todoist account connected")
	ErrTodoistNotConfigured = errors.New("todoist settings not configured")
	ErrTodoistMissingLabel  = errors.New("todoist settings not configured: missing label")
)

// ContactTaskService handles business logic for contact tasks
type ContactTaskService struct {
	contactTaskRepo *repository.ContactTaskRepository
	contactRepo     *repository.ContactRepository
	syncRepo        *repository.SyncRepository
	oauthService    *todoist.OAuthService
	frontendURL     string
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
		contactTaskRepo: contactTaskRepo,
		contactRepo:     contactRepo,
		syncRepo:        syncRepo,
		oauthService:    oauthService,
		frontendURL:     cfg.CORS.FrontendURL,
	}
}

// CreateActionTaskRequest contains the parameters for creating an action task
type CreateActionTaskRequest struct {
	ContactID uuid.UUID
	Text      string // Quick add text with optional dates, #project, @label, p1-p4
	Notes     string // Optional notes for the task description
}

// ActionTaskResponse represents the response for an action task
type ActionTaskResponse struct {
	ID             uuid.UUID `json:"id"`
	ContactID      uuid.UUID `json:"contact_id"`
	ExternalTaskID string    `json:"external_task_id"`
	Content        string    `json:"content"`
	DueDate        *string   `json:"due_date,omitempty"`
	ProjectID      string    `json:"project_id,omitempty"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateActionTask creates a new action task for a contact using Todoist Quick Add
func (s *ContactTaskService) CreateActionTask(ctx context.Context, req CreateActionTaskRequest) (*ActionTaskResponse, error) {
	// Verify contact exists
	contact, err := s.contactRepo.GetContact(ctx, req.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	// Get Todoist settings
	settings, accountID, err := s.getTodoistSettings(ctx)
	if err != nil {
		return nil, err
	}

	// Get access token
	accessToken, err := s.oauthService.GetAccessToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// Build the Quick Add text with contact name link prefix and CRM label
	contactLink := fmt.Sprintf("%s/contacts/%s", s.frontendURL, contact.ID.String())
	quickAddText := fmt.Sprintf("[%s](%s): %s", contact.FullName, contactLink, req.Text)
	if settings.LabelName != "" && !strings.Contains(strings.ToLower(req.Text), "@"+strings.ToLower(settings.LabelName)) {
		quickAddText = fmt.Sprintf("%s @%s", quickAddText, settings.LabelName)
	}

	// Note: Quick Add API doesn't support project_id parameter directly.
	// Tasks will go to inbox unless user specifies #project in their text.
	// This is acceptable UX - users have full control via natural language syntax.

	// Build the task description (CRM link is in content, so just notes + marker here)
	var descBuilder strings.Builder
	if req.Notes != "" {
		descBuilder.WriteString(req.Notes)
		descBuilder.WriteString("\n\n---\n")
	}

	// Add CRM marker for sync identification (use json.Marshal for safety)
	marker := map[string]any{
		"crm":        true,
		"contact_id": contact.ID.String(),
		"kind":       "action",
		"instance":   settings.IntegrationInstanceID,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return nil, fmt.Errorf("marshal marker: %w", err)
	}
	descBuilder.Write(markerJSON)

	// Step 1: Call Todoist Quick Add API (for natural language parsing)
	client := todoist.NewSyncClient(accessToken)
	task, err := client.QuickAdd(ctx, quickAddText, "")
	if err != nil {
		return nil, fmt.Errorf("todoist quick add: %w", err)
	}

	// Step 2: Update task description (and project if not specified) via Sync API
	// QuickAdd's "note" param creates comments, not descriptions
	updates := map[string]any{
		"description": descBuilder.String(),
	}
	// Default to configured project if user didn't specify one with #
	if settings.ProjectID != "" && !strings.Contains(req.Text, "#") {
		updates["project_id"] = settings.ProjectID
	}
	updateCmd := todoist.NewItemUpdateCommand(task.ID, updates)
	_, err = client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{updateCmd})
	if err != nil {
		// Log but don't fail - task was created, just missing description/project
		logger.Warn().Err(err).Str("task_id", task.ID).Msg("failed to update task")
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

	// Create contact_task record
	contactTask, err := s.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      req.ContactID,
		Provider:       todoist.SourceName,
		Kind:           todoist.TaskKindAction,
		ExternalTaskID: task.ID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("create contact task: %w", err)
	}

	// Build response
	response := &ActionTaskResponse{
		ID:             contactTask.ID,
		ContactID:      contactTask.ContactID,
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
func (s *ContactTaskService) ListContactTasks(ctx context.Context, contactID uuid.UUID, state *string, kind *string) ([]repository.ContactTask, error) {
	// Verify contact exists
	_, err := s.contactRepo.GetContact(ctx, contactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	return s.contactTaskRepo.ListContactTasksFiltered(ctx, contactID, state, kind)
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
