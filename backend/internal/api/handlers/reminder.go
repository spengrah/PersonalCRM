package handlers

import (
	"net/http"
	"strconv"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

type ReminderHandler struct {
	reminderService *service.ReminderService
	validator       *validator.Validate
}

func NewReminderHandler(reminderService *service.ReminderService) *ReminderHandler {
	return &ReminderHandler{
		reminderService: reminderService,
		validator:       validator.New(),
	}
}

// CreateReminderRequest represents the request body for creating a reminder
// @Description Request body for creating a new reminder
type CreateReminderRequest struct {
	ContactID   uuid.UUID `json:"contact_id" validate:"required" example:"123e4567-e89b-12d3-a456-426614174000"`
	Title       string    `json:"title" validate:"required,max=255" example:"Follow up with John"`
	Description *string   `json:"description" validate:"omitempty,max=1000" example:"Check on project status"`
	DueDate     time.Time `json:"due_date" validate:"required" example:"2024-03-15T09:00:00Z"`
} // @name CreateReminderRequest

// UpdateReminderRequest represents the request body for updating a reminder
// @Description Request body for updating an existing reminder
type UpdateReminderRequest struct {
	Title       *string    `json:"title" validate:"omitempty,max=255" example:"Updated reminder title"`
	Description *string    `json:"description" validate:"omitempty,max=1000" example:"Updated description"`
	DueDate     *time.Time `json:"due_date" example:"2024-03-16T09:00:00Z"`
} // @name UpdateReminderRequest

// CreateReminder creates a new reminder
// @Summary Create a new reminder
// @Description Create a new reminder for a contact
// @Tags reminders
// @Accept json
// @Produce json
// @Param reminder body CreateReminderRequest true "Reminder data"
// @Success 201 {object} api.APIResponse{data=repository.Reminder}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /reminders [post]
func (h *ReminderHandler) CreateReminder(c *gin.Context) {
	var req CreateReminderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	reminder, err := h.reminderService.CreateReminder(c.Request.Context(), repository.CreateReminderRequest{
		ContactID:   req.ContactID,
		Title:       req.Title,
		Description: req.Description,
		DueDate:     req.DueDate,
	})
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create reminder", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusCreated, reminder, nil)
}

// GetReminders returns a list of reminders
// @Summary Get reminders
// @Description Get a list of reminders with optional filtering
// @Tags reminders
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Param due_today query bool false "Filter reminders due today"
// @Success 200 {object} api.APIResponse{data=[]repository.DueReminder}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /reminders [get]
func (h *ReminderHandler) GetReminders(c *gin.Context) {
	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	dueToday := c.Query("due_today") == "true"

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	if dueToday {
		// Get reminders due today
		reminders, err := h.reminderService.GetTodayReminders(c.Request.Context())
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch reminders", err.Error())
			return
		}

		api.SendSuccess(c, http.StatusOK, reminders, &api.Meta{
			Pagination: &api.PaginationMeta{
				Page:  1,
				Limit: len(reminders),
				Total: int64(len(reminders)),
				Pages: 1,
			},
		})
		return
	}

	// Get all due reminders (up to current time)
	now := time.Now()
	reminders, err := h.reminderService.GetDueReminders(c.Request.Context(), now)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch reminders", err.Error())
		return
	}

	// Apply pagination
	offset := (page - 1) * limit
	end := offset + limit
	if end > len(reminders) {
		end = len(reminders)
	}
	if offset > len(reminders) {
		offset = len(reminders)
	}

	paginatedReminders := reminders[offset:end]
	totalPages := (len(reminders) + limit - 1) / limit

	api.SendSuccess(c, http.StatusOK, paginatedReminders, &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
			Total: int64(len(reminders)),
			Pages: totalPages,
		},
	})
}

// GetRemindersByContact returns reminders for a specific contact
// @Summary Get reminders by contact
// @Description Get all reminders for a specific contact
// @Tags reminders
// @Produce json
// @Param id path string true "Contact ID"
// @Success 200 {object} api.APIResponse{data=[]repository.Reminder}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/reminders [get]
func (h *ReminderHandler) GetRemindersByContact(c *gin.Context) {
	contactIDStr := c.Param("id")
	contactID, err := uuid.Parse(contactIDStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid contact ID", err.Error())
		return
	}

	reminders, err := h.reminderService.GetRemindersByContact(c.Request.Context(), contactID)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch reminders", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, reminders, nil)
}

// CompleteReminder marks a reminder as completed
// @Summary Complete a reminder
// @Description Mark a reminder as completed
// @Tags reminders
// @Produce json
// @Param id path string true "Reminder ID"
// @Success 200 {object} api.APIResponse{data=repository.Reminder}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /reminders/{id}/complete [patch]
func (h *ReminderHandler) CompleteReminder(c *gin.Context) {
	reminderIDStr := c.Param("id")
	reminderID, err := uuid.Parse(reminderIDStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid reminder ID", err.Error())
		return
	}

	reminder, err := h.reminderService.CompleteReminder(c.Request.Context(), reminderID)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to complete reminder", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, reminder, nil)
}

// DeleteReminder deletes a reminder
// @Summary Delete a reminder
// @Description Soft delete a reminder
// @Tags reminders
// @Produce json
// @Param id path string true "Reminder ID"
// @Success 204
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /reminders/{id} [delete]
func (h *ReminderHandler) DeleteReminder(c *gin.Context) {
	reminderIDStr := c.Param("id")
	reminderID, err := uuid.Parse(reminderIDStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid reminder ID", err.Error())
		return
	}

	err = h.reminderService.DeleteReminder(c.Request.Context(), reminderID)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete reminder", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

// GetReminderStats returns reminder statistics
// @Summary Get reminder statistics
// @Description Get statistics about reminders (total, due today, overdue)
// @Tags reminders
// @Produce json
// @Success 200 {object} api.APIResponse{data=map[string]interface{}}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /reminders/stats [get]
func (h *ReminderHandler) GetReminderStats(c *gin.Context) {
	stats, err := h.reminderService.GetReminderStats(c.Request.Context())
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch reminder stats", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, stats, nil)
}
