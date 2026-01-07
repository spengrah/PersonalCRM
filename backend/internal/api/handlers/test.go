package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestHandler handles test data management endpoints
// These endpoints are only available when CRM_ENV=testing or CRM_ENV=test
type TestHandler struct {
	database     *db.Database
	externalRepo *repository.ExternalContactRepository
	contactSvc   *service.ContactService
	calendarRepo *repository.CalendarEventRepository
	validator    *validator.Validate
}

// NewTestHandler creates a new test handler
func NewTestHandler(
	database *db.Database,
	externalRepo *repository.ExternalContactRepository,
	contactSvc *service.ContactService,
	calendarRepo *repository.CalendarEventRepository,
) *TestHandler {
	return &TestHandler{
		database:     database,
		externalRepo: externalRepo,
		contactSvc:   contactSvc,
		calendarRepo: calendarRepo,
		validator:    validator.New(),
	}
}

// SeedExternalContactInput represents input for creating an external contact
type SeedExternalContactInput struct {
	DisplayName  string   `json:"display_name" validate:"required,min=1,max=255"`
	Emails       []string `json:"emails,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	Organization string   `json:"organization,omitempty"`
	JobTitle     string   `json:"job_title,omitempty"`
}

// SeedExternalContactsRequest represents the request to seed external contacts
type SeedExternalContactsRequest struct {
	Prefix   string                     `json:"prefix" validate:"required,min=1,max=50"`
	Contacts []SeedExternalContactInput `json:"contacts" validate:"required,min=1,max=100,dive"`
}

// SeedExternalContactsResponse represents the response from seeding external contacts
type SeedExternalContactsResponse struct {
	Created int      `json:"created"`
	IDs     []string `json:"ids"`
}

// SeedExternalContacts creates import candidates in the external_contact table
// @Summary Seed external contacts for testing
// @Description Create import candidates in the external_contact table for e2e testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedExternalContactsRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedExternalContactsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/external-contacts [post]
func (h *TestHandler) SeedExternalContacts(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedExternalContactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(req.Contacts))

	for i, input := range req.Contacts {
		// Build email entries
		emails := make([]repository.EmailEntry, 0, len(input.Emails))
		for j, email := range input.Emails {
			emails = append(emails, repository.EmailEntry{
				Value:   email,
				Type:    "personal",
				Primary: j == 0,
			})
		}

		// Build phone entries
		phones := make([]repository.PhoneEntry, 0, len(input.Phones))
		for j, phone := range input.Phones {
			phones = append(phones, repository.PhoneEntry{
				Value:   phone,
				Type:    "mobile",
				Primary: j == 0,
			})
		}

		// Create upsert request with prefix in source_id for cleanup
		displayName := req.Prefix + "-" + input.DisplayName
		upsertReq := repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    fmt.Sprintf("%s-contact-%d", req.Prefix, i),
			DisplayName: &displayName,
			Emails:      emails,
			Phones:      phones,
			SyncedAt:    &now,
		}

		if input.Organization != "" {
			upsertReq.Organization = &input.Organization
		}
		if input.JobTitle != "" {
			upsertReq.JobTitle = &input.JobTitle
		}

		contact, err := h.externalRepo.Upsert(ctx, upsertReq)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create external contact", err.Error())
			return
		}

		ids = append(ids, contact.ID.String())
	}

	api.SendSuccess(c, http.StatusCreated, SeedExternalContactsResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}

// SeedOverdueContactInput represents input for creating an overdue contact
type SeedOverdueContactInput struct {
	FullName    string `json:"full_name" validate:"required,min=1,max=255"`
	Cadence     string `json:"cadence" validate:"required,oneof=weekly biweekly monthly quarterly biannual annual"`
	DaysOverdue int    `json:"days_overdue" validate:"required,min=1,max=365"`
	Email       string `json:"email,omitempty" validate:"omitempty,email"`
}

// SeedOverdueContactsRequest represents the request to seed overdue contacts
type SeedOverdueContactsRequest struct {
	Prefix   string                    `json:"prefix" validate:"required,min=1,max=50"`
	Contacts []SeedOverdueContactInput `json:"contacts" validate:"required,min=1,max=100,dive"`
}

// SeedOverdueContactsResponse represents the response from seeding overdue contacts
type SeedOverdueContactsResponse struct {
	Created int      `json:"created"`
	IDs     []string `json:"ids"`
}

// SeedOverdueContacts creates contacts with backdated last_contacted timestamps
// @Summary Seed overdue contacts for testing
// @Description Create contacts with backdated last_contacted for e2e testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedOverdueContactsRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedOverdueContactsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/overdue-contacts [post]
func (h *TestHandler) SeedOverdueContacts(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedOverdueContactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(req.Contacts))

	for _, input := range req.Contacts {
		// Parse cadence type
		cadenceType, err := reminder.ParseCadence(input.Cadence)
		if err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid cadence", err.Error())
			return
		}

		// Calculate backdated last_contacted time
		// It should be: now - cadence_duration - days_overdue
		// Use scaled days based on environment (in testing mode, 1 "day" = weekly_cadence / 7)
		cadenceDuration := reminder.GetCadenceDuration(cadenceType)
		weeklyDuration := reminder.GetCadenceDuration(reminder.CadenceWeekly)
		scaledDayDuration := weeklyDuration / 7 // 1 "day" in current environment
		daysOverdueDuration := time.Duration(input.DaysOverdue) * scaledDayDuration
		lastContacted := now.Add(-cadenceDuration).Add(-daysOverdueDuration)

		// Build contact methods
		var methods []service.ContactMethodInput
		if input.Email != "" {
			methods = append(methods, service.ContactMethodInput{
				Type:      "email_personal",
				Value:     input.Email,
				IsPrimary: true,
			})
		}

		// Create contact with prefix in name for cleanup
		fullName := req.Prefix + "-" + input.FullName
		cadence := input.Cadence
		createReq := repository.CreateContactRequest{
			FullName:      fullName,
			Cadence:       &cadence,
			LastContacted: &lastContacted,
		}

		contact, err := h.contactSvc.CreateContact(ctx, createReq, methods)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create contact", err.Error())
			return
		}

		ids = append(ids, contact.ID.String())
	}

	api.SendSuccess(c, http.StatusCreated, SeedOverdueContactsResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}

// SeedCalendarEventInput represents input for creating a calendar event
type SeedCalendarEventInput struct {
	Title     string `json:"title" validate:"required,min=1,max=255"`
	Location  string `json:"location,omitempty"`
	HtmlLink  string `json:"html_link,omitempty"`
	IsPast    bool   `json:"is_past,omitempty"`    // If true, event is set in the past
	DaysAgo   int    `json:"days_ago,omitempty"`   // If is_past, how many days ago (default: 7)
	DaysAhead int    `json:"days_ahead,omitempty"` // If not is_past, how many days ahead (default: 7)
}

// SeedCalendarEventsRequest represents the request to seed calendar events
type SeedCalendarEventsRequest struct {
	Prefix    string                   `json:"prefix" validate:"required,min=1,max=50"`
	ContactID string                   `json:"contact_id" validate:"required"` // Primary contact to link events to
	Events    []SeedCalendarEventInput `json:"events" validate:"required,min=1,max=50,dive"`
}

// SeedCalendarEventsResponse represents the response from seeding calendar events
type SeedCalendarEventsResponse struct {
	Created int      `json:"created"`
	IDs     []string `json:"ids"`
}

// SeedCalendarEvents creates calendar events linked to a contact
// @Summary Seed calendar events for testing
// @Description Create calendar events linked to a contact for e2e testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedCalendarEventsRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedCalendarEventsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/calendar-events [post]
func (h *TestHandler) SeedCalendarEvents(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedCalendarEventsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	// Parse contact ID
	contactID, err := parseUUID(req.ContactID)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid contact_id", err.Error())
		return
	}

	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(req.Events))

	for i, input := range req.Events {
		// Calculate event times based on is_past flag
		var startTime, endTime time.Time
		if input.IsPast {
			daysAgo := input.DaysAgo
			if daysAgo == 0 {
				daysAgo = 7
			}
			startTime = now.AddDate(0, 0, -daysAgo).Add(10 * time.Hour) // 10 AM
			endTime = startTime.Add(1 * time.Hour)                      // 1 hour duration
		} else {
			daysAhead := input.DaysAhead
			if daysAhead == 0 {
				daysAhead = 7
			}
			startTime = now.AddDate(0, 0, daysAhead).Add(14 * time.Hour) // 2 PM
			endTime = startTime.Add(1 * time.Hour)                       // 1 hour duration
		}

		// Build title with prefix
		title := req.Prefix + "-" + input.Title

		// Build upsert request
		upsertReq := repository.UpsertCalendarEventRequest{
			GcalEventID:          fmt.Sprintf("%s-event-%d", req.Prefix, i),
			GcalCalendarID:       "primary",
			GoogleAccountID:      fmt.Sprintf("%s-test-account", req.Prefix),
			Title:                &title,
			StartTime:            startTime,
			EndTime:              endTime,
			Status:               "confirmed",
			MatchedContactIDs:    []uuid.UUID{contactID},
			SyncedAt:             now,
			LastContactedUpdated: false,
		}

		if input.Location != "" {
			upsertReq.Location = &input.Location
		}

		if input.HtmlLink != "" {
			upsertReq.HtmlLink = &input.HtmlLink
		}

		event, err := h.calendarRepo.Upsert(ctx, upsertReq)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create calendar event", err.Error())
			return
		}

		ids = append(ids, event.ID.String())
	}

	api.SendSuccess(c, http.StatusCreated, SeedCalendarEventsResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}

// parseUUID parses a string into a UUID
func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

// CleanupRequest represents the request to cleanup test data
type CleanupRequest struct {
	Prefix string `json:"prefix" validate:"required,min=1,max=50"`
}

// CleanupResponse represents the response from cleanup
type CleanupResponse struct {
	DeletedContacts         int64 `json:"deleted_contacts"`
	DeletedExternalContacts int64 `json:"deleted_external_contacts"`
	DeletedCalendarEvents   int64 `json:"deleted_calendar_events"`
}

// Cleanup deletes test data by prefix
// @Summary Cleanup test data
// @Description Delete test data (contacts and external contacts) by prefix
// @Tags test
// @Accept json
// @Produce json
// @Param body body CleanupRequest true "Cleanup request"
// @Success 200 {object} api.APIResponse{data=CleanupResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/cleanup [post]
func (h *TestHandler) Cleanup(c *gin.Context) {
	ctx := c.Request.Context()

	var req CleanupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	// Use a transaction for atomic cleanup
	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to start transaction", err.Error())
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	queries := db.New(tx)
	// Escape SQL LIKE wildcards to prevent injection
	// This ensures % and _ in prefixes are treated literally
	escapedPrefix := escapeSQLLikeWildcards(req.Prefix)
	prefix := pgtype.Text{String: escapedPrefix, Valid: true}

	// Delete contacts by name prefix (will cascade to contact_method via FK)
	deletedContacts, err := queries.DeleteContactsByNamePrefix(ctx, prefix)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete contacts", err.Error())
		return
	}

	// Delete external contacts by display_name prefix
	deletedExternal, err := queries.DeleteExternalContactsByDisplayNamePrefix(ctx, prefix)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete external contacts", err.Error())
		return
	}

	// Also delete by source_id prefix (in case display_name was different)
	deletedBySourceID, err := queries.DeleteExternalContactsBySourceIDPrefix(ctx, prefix)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete external contacts by source_id", err.Error())
		return
	}
	deletedExternal += deletedBySourceID

	// Delete calendar events by title prefix
	deletedCalEvents, err := queries.DeleteCalendarEventsByTitlePrefix(ctx, prefix)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete calendar events by title", err.Error())
		return
	}

	// Also delete by gcal_event_id prefix (in case title was different)
	deletedByGcalID, err := queries.DeleteCalendarEventsByGcalEventIdPrefix(ctx, prefix)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to delete calendar events by gcal_event_id", err.Error())
		return
	}
	deletedCalEvents += deletedByGcalID

	if err := tx.Commit(ctx); err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to commit transaction", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, CleanupResponse{
		DeletedContacts:         deletedContacts,
		DeletedExternalContacts: deletedExternal,
		DeletedCalendarEvents:   deletedCalEvents,
	}, nil)
}

// TriggerErrorRequest represents the request to trigger an error
type TriggerErrorRequest struct {
	ErrorType string `json:"error_type" validate:"required,oneof=500 panic"`
	Message   string `json:"message,omitempty"`
}

// TriggerError triggers an error for error boundary testing
// @Summary Trigger error for testing
// @Description Trigger a server error for error boundary testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body TriggerErrorRequest true "Error request"
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/trigger-error [post]
func (h *TestHandler) TriggerError(c *gin.Context) {
	var req TriggerErrorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	message := req.Message
	if message == "" {
		message = "Test error triggered"
	}

	switch req.ErrorType {
	case "panic":
		panic(message)
	default:
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Test error triggered", message)
	}
}

// escapeSQLLikeWildcards escapes SQL LIKE pattern wildcards (% and _)
// to prevent them from being interpreted as wildcards in LIKE queries.
// This prevents SQL wildcard injection attacks.
func escapeSQLLikeWildcards(s string) string {
	// Escape backslash first (since it's the escape character)
	s = strings.ReplaceAll(s, `\`, `\\`)
	// Escape percentage sign
	s = strings.ReplaceAll(s, `%`, `\%`)
	// Escape underscore
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
