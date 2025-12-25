package handlers

import (
	"net/http"
	"strconv"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

type TimeEntryHandler struct {
	timeEntryRepo *repository.TimeEntryRepository
	validator     *validator.Validate
}

func NewTimeEntryHandler(timeEntryRepo *repository.TimeEntryRepository) *TimeEntryHandler {
	return &TimeEntryHandler{
		timeEntryRepo: timeEntryRepo,
		validator:     validator.New(),
	}
}

// CreateTimeEntryRequest represents the request body for creating a time entry
// @Description Request body for creating a new time entry
type CreateTimeEntryRequest struct {
	Description     string     `json:"description" validate:"required,max=500" example:"Working on project feature"`
	Project         *string    `json:"project,omitempty" validate:"omitempty,max=100" example:"Personal CRM"`
	ContactID       *uuid.UUID `json:"contact_id,omitempty" example:"123e4567-e89b-12d3-a456-426614174000"`
	StartTime       time.Time  `json:"start_time" validate:"required" example:"2024-03-15T09:00:00Z"`
	EndTime         *time.Time `json:"end_time,omitempty" example:"2024-03-15T10:30:00Z"`
	DurationMinutes *int32     `json:"duration_minutes,omitempty" example:"90"`
} // @name CreateTimeEntryRequest

// UpdateTimeEntryRequest represents the request body for updating a time entry
// @Description Request body for updating an existing time entry
type UpdateTimeEntryRequest struct {
	Description     *string    `json:"description,omitempty" validate:"omitempty,max=500" example:"Updated description"`
	Project         *string    `json:"project,omitempty" validate:"omitempty,max=100" example:"Updated project"`
	ContactID       *uuid.UUID `json:"contact_id,omitempty"`
	EndTime         *time.Time `json:"end_time,omitempty" example:"2024-03-15T11:00:00Z"`
	DurationMinutes *int32     `json:"duration_minutes,omitempty" example:"120"`
} // @name UpdateTimeEntryRequest

// CreateTimeEntry creates a new time entry
// @Summary Create a new time entry
// @Description Create a new time entry to track time spent
// @Tags time-entries
// @Accept json
// @Produce json
// @Param time_entry body CreateTimeEntryRequest true "Time entry data"
// @Success 201 {object} api.APIResponse{data=repository.TimeEntry}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries [post]
func (h *TimeEntryHandler) CreateTimeEntry(c *gin.Context) {
	var req CreateTimeEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	// Calculate duration if end_time is provided but duration_minutes is not
	if req.EndTime != nil && req.DurationMinutes == nil {
		duration := int32(req.EndTime.Sub(req.StartTime).Minutes())
		req.DurationMinutes = &duration
	}

	timeEntry, err := h.timeEntryRepo.CreateTimeEntry(c.Request.Context(), repository.CreateTimeEntryRequest{
		Description:     req.Description,
		Project:         req.Project,
		ContactID:       req.ContactID,
		StartTime:       req.StartTime,
		EndTime:         req.EndTime,
		DurationMinutes: req.DurationMinutes,
	})
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create time entry", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusCreated, timeEntry, nil)
}

// GetTimeEntry retrieves a time entry by ID
// @Summary Get a time entry by ID
// @Description Get a specific time entry by its ID
// @Tags time-entries
// @Produce json
// @Param id path string true "Time Entry ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=repository.TimeEntry}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries/{id} [get]
func (h *TimeEntryHandler) GetTimeEntry(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid time entry ID", err.Error())
		return
	}

	timeEntry, err := h.timeEntryRepo.GetTimeEntry(c.Request.Context(), id)
	if err != nil {
		if err == db.ErrNotFound {
			api.SendNotFound(c, "Time entry")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to retrieve time entry", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, timeEntry, nil)
}

// ListTimeEntries returns a list of time entries
// @Summary Get time entries
// @Description Get a list of time entries with optional filtering
// @Tags time-entries
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Param contact_id query string false "Filter by contact ID" format(uuid)
// @Param start_date query string false "Filter by start date (ISO 8601)" example:"2024-03-01T00:00:00Z"
// @Param end_date query string false "Filter by end date (ISO 8601)" example:"2024-03-31T23:59:59Z"
// @Success 200 {object} api.APIResponse{data=[]repository.TimeEntry}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries [get]
func (h *TimeEntryHandler) ListTimeEntries(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	contactIDStr := c.Query("contact_id")
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	// Filter by contact
	if contactIDStr != "" {
		contactID, err := uuid.Parse(contactIDStr)
		if err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid contact ID", err.Error())
			return
		}

		entries, err := h.timeEntryRepo.ListTimeEntriesByContact(c.Request.Context(), contactID)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch time entries", err.Error())
			return
		}

		api.SendSuccess(c, http.StatusOK, entries, nil)
		return
	}

	// Filter by date range
	if startDateStr != "" && endDateStr != "" {
		startDate, err := time.Parse(time.RFC3339, startDateStr)
		if err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid start_date format", err.Error())
			return
		}

		endDate, err := time.Parse(time.RFC3339, endDateStr)
		if err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid end_date format", err.Error())
			return
		}

		entries, err := h.timeEntryRepo.ListTimeEntriesByDateRange(c.Request.Context(), startDate, endDate)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch time entries", err.Error())
			return
		}

		api.SendSuccess(c, http.StatusOK, entries, nil)
		return
	}

	// List all entries with pagination
	entries, err := h.timeEntryRepo.ListTimeEntries(c.Request.Context(), repository.ListTimeEntriesParams{
		Limit:  limit,
		Offset: (page - 1) * limit,
	})
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch time entries", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, entries, &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
		},
	})
}

// GetRunningTimeEntry returns the currently running time entry (if any)
// @Summary Get running time entry
// @Description Get the currently running time entry (timer that hasn't been stopped)
// @Tags time-entries
// @Produce json
// @Success 200 {object} api.APIResponse{data=repository.TimeEntry}
// @Failure 404 {object} api.APIResponse{error=api.APIError} "No running time entry"
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries/running [get]
func (h *TimeEntryHandler) GetRunningTimeEntry(c *gin.Context) {
	entry, err := h.timeEntryRepo.GetRunningTimeEntry(c.Request.Context())
	if err != nil {
		if err == db.ErrNotFound {
			api.SendNotFound(c, "Running time entry")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to retrieve running time entry", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, entry, nil)
}

// UpdateTimeEntry updates an existing time entry
// @Summary Update a time entry
// @Description Update an existing time entry
// @Tags time-entries
// @Accept json
// @Produce json
// @Param id path string true "Time Entry ID" format(uuid)
// @Param time_entry body UpdateTimeEntryRequest true "Time entry update data"
// @Success 200 {object} api.APIResponse{data=repository.TimeEntry}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries/{id} [put]
func (h *TimeEntryHandler) UpdateTimeEntry(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid time entry ID", err.Error())
		return
	}

	var req UpdateTimeEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	// If end_time is being set, calculate duration if not provided
	if req.EndTime != nil && req.DurationMinutes == nil {
		existing, err := h.timeEntryRepo.GetTimeEntry(c.Request.Context(), id)
		if err != nil {
			if err == db.ErrNotFound {
				api.SendNotFound(c, "Time entry")
				return
			}
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to retrieve time entry", err.Error())
			return
		}

		duration := int32(req.EndTime.Sub(existing.StartTime).Minutes())
		req.DurationMinutes = &duration
	}

	timeEntry, err := h.timeEntryRepo.UpdateTimeEntry(c.Request.Context(), id, repository.UpdateTimeEntryRequest{
		Description:     req.Description,
		Project:         req.Project,
		ContactID:       req.ContactID,
		EndTime:         req.EndTime,
		DurationMinutes: req.DurationMinutes,
	})
	if err != nil {
		if err == db.ErrNotFound {
			api.SendNotFound(c, "Time entry")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to update time entry", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, timeEntry, nil)
}

// DeleteTimeEntry deletes a time entry
// @Summary Delete a time entry
// @Description Delete a time entry by ID
// @Tags time-entries
// @Produce json
// @Param id path string true "Time Entry ID" format(uuid)
// @Success 204 "Time entry deleted successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries/{id} [delete]
func (h *TimeEntryHandler) DeleteTimeEntry(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid time entry ID", err.Error())
		return
	}

	err = h.timeEntryRepo.DeleteTimeEntry(c.Request.Context(), id)
	if err != nil {
		if err == db.ErrNotFound {
			api.SendNotFound(c, "Time entry")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete time entry", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

// GetTimeEntryStats returns statistics about time entries
// @Summary Get time entry statistics
// @Description Get statistics about time entries (total, today, week, month)
// @Tags time-entries
// @Produce json
// @Success 200 {object} api.APIResponse{data=repository.TimeEntryStats}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /time-entries/stats [get]
func (h *TimeEntryHandler) GetTimeEntryStats(c *gin.Context) {
	stats, err := h.timeEntryRepo.GetTimeEntryStats(c.Request.Context())
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch statistics", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, stats, nil)
}
