package handlers

import (
	"fmt"
	"net/http"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestHandler handles test data management endpoints
// These endpoints are only available when CRM_ENV=testing or CRM_ENV=test
type TestHandler struct {
	database     *db.Database
	externalRepo *repository.ExternalContactRepository
	contactSvc   *service.ContactService
	validator    *validator.Validate
}

// NewTestHandler creates a new test handler
func NewTestHandler(
	database *db.Database,
	externalRepo *repository.ExternalContactRepository,
	contactSvc *service.ContactService,
) *TestHandler {
	return &TestHandler{
		database:     database,
		externalRepo: externalRepo,
		contactSvc:   contactSvc,
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

// CleanupRequest represents the request to cleanup test data
type CleanupRequest struct {
	Prefix string `json:"prefix" validate:"required,min=1,max=50"`
}

// CleanupResponse represents the response from cleanup
type CleanupResponse struct {
	DeletedContacts         int64 `json:"deleted_contacts"`
	DeletedExternalContacts int64 `json:"deleted_external_contacts"`
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
	prefix := pgtype.Text{String: req.Prefix, Valid: true}

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

	if err := tx.Commit(ctx); err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to commit transaction", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, CleanupResponse{
		DeletedContacts:         deletedContacts,
		DeletedExternalContacts: deletedExternal,
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
