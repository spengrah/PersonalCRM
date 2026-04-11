package handlers

import (
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// ContactTaskHandler handles contact task-related HTTP requests
type ContactTaskHandler struct {
	contactTaskService *service.ContactTaskService
	validator          *validator.Validate
}

// NewContactTaskHandler creates a new contact task handler
func NewContactTaskHandler(contactTaskService *service.ContactTaskService) *ContactTaskHandler {
	return &ContactTaskHandler{
		contactTaskService: contactTaskService,
		validator:          validator.New(),
	}
}

// CreateActionTaskRequest represents the request to create an action task
type CreateActionTaskRequest struct {
	Text  string `json:"text" validate:"required,min=1,max=1000"`
	Notes string `json:"notes,omitempty" validate:"omitempty,max=5000"`
}

// ContactTaskResponse represents a task in API responses
type ContactTaskResponse struct {
	ID             string    `json:"id"`
	ContactID      string    `json:"contact_id"`
	Kind           string    `json:"kind"`
	ExternalTaskID string    `json:"external_task_id"`
	Content        string    `json:"content,omitempty"`
	DueDate        *string   `json:"due_date,omitempty"`
	ProjectID      string    `json:"project_id,omitempty"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListContactTasksQuery represents query parameters for listing tasks
type ListContactTasksQuery struct {
	State string `form:"state" validate:"omitempty,oneof=managed completed unmanaged dismissed"`
	Kind  string `form:"kind" validate:"omitempty,oneof=action cadence follow_up"`
}

// CreateActionTask creates a new action task for a contact
// @Summary Create action task
// @Description Create a new one-off task for a contact using Todoist Quick Add
// @Tags contact-tasks
// @Accept json
// @Produce json
// @Param id path string true "Contact ID"
// @Param request body CreateActionTaskRequest true "Task creation request"
// @Success 201 {object} api.APIResponse{data=ContactTaskResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/tasks [post]
func (h *ContactTaskHandler) CreateActionTask(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse contact ID
	contactID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	// Bind request
	var req CreateActionTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	// Validate request
	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Create task
	result, err := h.contactTaskService.CreateActionTask(ctx, service.CreateActionTaskRequest{
		ContactID: contactID,
		Text:      req.Text,
		Notes:     req.Notes,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		// Check for sentinel errors
		if errors.Is(err, service.ErrNoTodoistAccount) {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeBadRequest, "No Todoist account connected", "Connect Todoist in Settings")
			return
		}
		if errors.Is(err, service.ErrTodoistNotConfigured) || errors.Is(err, service.ErrTodoistMissingLabel) {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeBadRequest, "Todoist settings not configured", "Configure Todoist project and label in Settings")
			return
		}
		api.SendInternalError(c, "Failed to create task")
		return
	}

	// Build response
	response := ContactTaskResponse{
		ID:             result.ID.String(),
		ContactID:      result.ContactID.String(),
		Kind:           "action",
		ExternalTaskID: result.ExternalTaskID,
		Content:        result.Content,
		DueDate:        result.DueDate,
		ProjectID:      result.ProjectID,
		State:          result.State,
		CreatedAt:      result.CreatedAt,
	}

	api.SendSuccess(c, http.StatusCreated, response, nil)
}

// ListContactTasks lists tasks for a contact
// @Summary List contact tasks
// @Description List all tasks for a contact with optional filters
// @Tags contact-tasks
// @Produce json
// @Param id path string true "Contact ID"
// @Param state query string false "Filter by state (managed, completed, unmanaged, dismissed)"
// @Param kind query string false "Filter by kind (action, cadence, follow_up)"
// @Success 200 {object} api.APIResponse{data=[]ContactTaskResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/tasks [get]
func (h *ContactTaskHandler) ListContactTasks(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse contact ID
	contactID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	// Parse query params
	var query ListContactTasksQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		api.SendValidationError(c, "Invalid query parameters", err.Error())
		return
	}

	// Validate query
	if err := h.validator.Struct(query); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Build filters
	var stateFilter, kindFilter *string
	if query.State != "" {
		stateFilter = &query.State
	}
	if query.Kind != "" {
		kindFilter = &query.Kind
	}

	// List tasks
	tasks, err := h.contactTaskService.ListContactTasks(ctx, contactID, stateFilter, kindFilter)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to list tasks")
		return
	}

	// Build response
	responses := make([]ContactTaskResponse, len(tasks))
	for i, task := range tasks {
		responses[i] = contactTaskToResponse(task)
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}

// DeleteTaskLink removes CRM tracking for a task
// @Summary Delete task link
// @Description Remove CRM tracking for a task (does not delete from Todoist)
// @Tags contact-tasks
// @Param id path string true "Contact ID"
// @Param taskId path string true "Task ID"
// @Success 204 "No Content"
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/tasks/{taskId} [delete]
func (h *ContactTaskHandler) DeleteTaskLink(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse contact ID
	contactID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	// Parse task ID
	taskID, err := uuid.Parse(c.Param("taskId"))
	if err != nil {
		api.SendValidationError(c, "Invalid task ID", "ID must be a valid UUID")
		return
	}

	// Delete task link
	err = h.contactTaskService.DeleteTaskLink(ctx, contactID, taskID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Task")
			return
		}
		api.SendInternalError(c, "Failed to delete task link")
		return
	}

	c.Status(http.StatusNoContent)
}

// contactTaskToResponse converts a repository task to an API response
func contactTaskToResponse(task repository.ContactTask) ContactTaskResponse {
	response := ContactTaskResponse{
		ID:             task.ID.String(),
		ContactID:      task.ContactID.String(),
		Kind:           task.Kind,
		ExternalTaskID: task.ExternalTaskID,
		State:          string(task.State),
		CreatedAt:      task.CreatedAt,
	}

	// Extract metadata fields
	if task.Metadata != nil {
		if content, ok := task.Metadata["content"].(string); ok {
			response.Content = content
		}
		if dueDate, ok := task.Metadata["due_date"].(string); ok {
			response.DueDate = &dueDate
		}
		if projectID, ok := task.Metadata["project_id"].(string); ok {
			response.ProjectID = projectID
		}
	}

	return response
}
