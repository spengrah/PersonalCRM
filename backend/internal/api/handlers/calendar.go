package handlers

import (
	"net/http"
	"strconv"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// CalendarHandler handles calendar-related HTTP requests
type CalendarHandler struct {
	calendarRepo *repository.CalendarEventRepository
}

// NewCalendarHandler creates a new calendar handler
func NewCalendarHandler(calendarRepo *repository.CalendarEventRepository) *CalendarHandler {
	return &CalendarHandler{
		calendarRepo: calendarRepo,
	}
}

// Pagination bounds
const (
	maxLimit     = 100
	defaultLimit = 20
)

// parsePagination parses and validates pagination parameters
func parsePagination(c *gin.Context, defaultLimitOverride int) (limit, offset int32) {
	defLimit := defaultLimit
	if defaultLimitOverride > 0 {
		defLimit = defaultLimitOverride
	}

	limit32, err := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(defLimit)))
	if err != nil || limit32 < 0 {
		limit32 = defLimit
	}
	if limit32 > maxLimit {
		limit32 = maxLimit
	}

	offset32, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset32 < 0 {
		offset32 = 0
	}

	return int32(limit32), int32(offset32)
}

// CalendarEventResponse represents a calendar event in API responses
type CalendarEventResponse struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Description   *string `json:"description,omitempty"`
	Location      *string `json:"location,omitempty"`
	StartTime     string  `json:"start_time"`
	EndTime       string  `json:"end_time"`
	Status        string  `json:"status"`
	AttendeeCount int     `json:"attendee_count"`
	HtmlLink      *string `json:"html_link,omitempty"`
}

// convertToEventResponse converts a repository event to an API response
func convertToEventResponse(event *repository.CalendarEvent) CalendarEventResponse {
	title := ""
	if event.Title != nil {
		title = *event.Title
	}

	return CalendarEventResponse{
		ID:            event.ID.String(),
		Title:         title,
		Description:   event.Description,
		Location:      event.Location,
		StartTime:     event.StartTime.Format("2006-01-02T15:04:05Z07:00"),
		EndTime:       event.EndTime.Format("2006-01-02T15:04:05Z07:00"),
		Status:        event.Status,
		AttendeeCount: len(event.Attendees),
		HtmlLink:      event.HtmlLink,
	}
}

// ListEventsForContact returns calendar events for a specific contact
// @Summary List calendar events for a contact
// @Description Get calendar events involving a specific contact
// @Tags calendar
// @Produce json
// @Param id path string true "Contact ID"
// @Param limit query int false "Items per page" default(20)
// @Param offset query int false "Offset" default(0)
// @Success 200 {object} api.APIResponse{data=[]CalendarEventResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/events [get]
func (h *CalendarHandler) ListEventsForContact(c *gin.Context) {
	// Parse contact ID
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid contact ID", err.Error())
		return
	}

	// Parse and validate pagination
	limit, offset := parsePagination(c, 0)

	// Fetch events
	events, err := h.calendarRepo.ListEventsForContact(c.Request.Context(), contactID, limit, offset)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch events", err.Error())
		return
	}

	// Convert to response format
	responses := make([]CalendarEventResponse, len(events))
	for i, event := range events {
		responses[i] = convertToEventResponse(&event)
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}

// ListUpcomingEventsForContact returns upcoming calendar events for a specific contact
// @Summary List upcoming calendar events for a contact
// @Description Get upcoming calendar events involving a specific contact
// @Tags calendar
// @Produce json
// @Param id path string true "Contact ID"
// @Param limit query int false "Max items" default(10)
// @Success 200 {object} api.APIResponse{data=[]CalendarEventResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/events/upcoming [get]
func (h *CalendarHandler) ListUpcomingEventsForContact(c *gin.Context) {
	// Parse contact ID
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid contact ID", err.Error())
		return
	}

	// Parse and validate limit (no offset for upcoming)
	limit, _ := parsePagination(c, 10)

	// Fetch upcoming events
	now := accelerated.GetCurrentTime()
	events, err := h.calendarRepo.ListUpcomingEventsForContact(c.Request.Context(), contactID, now, limit)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch events", err.Error())
		return
	}

	// Convert to response format
	responses := make([]CalendarEventResponse, len(events))
	for i, event := range events {
		responses[i] = convertToEventResponse(&event)
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}

// ListUpcomingEvents returns upcoming events with CRM contacts
// @Summary List upcoming calendar events with CRM contacts
// @Description Get upcoming calendar events that have matched CRM contacts
// @Tags calendar
// @Produce json
// @Param limit query int false "Items per page" default(20)
// @Param offset query int false "Offset" default(0)
// @Success 200 {object} api.APIResponse{data=[]CalendarEventResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /events/upcoming [get]
func (h *CalendarHandler) ListUpcomingEvents(c *gin.Context) {
	// Parse and validate pagination
	limit, offset := parsePagination(c, 0)

	// Fetch upcoming events
	now := accelerated.GetCurrentTime()
	events, err := h.calendarRepo.ListUpcomingEventsWithContacts(c.Request.Context(), now, limit, offset)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to fetch events", err.Error())
		return
	}

	// Convert to response format
	responses := make([]CalendarEventResponse, len(events))
	for i, event := range events {
		responses[i] = convertToEventResponse(&event)
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}
