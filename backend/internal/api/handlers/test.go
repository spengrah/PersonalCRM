package handlers

import (
	"fmt"
	"net/http"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// TestHandler handles test data management endpoints
// These endpoints are only available when CRM_ENV=testing or CRM_ENV=test
//
// Two provisioning paths live here during the migration to declared seeding:
//
//   - BESPOKE (the seven /test/seed/* endpoints and the prefix shape of
//     /test/cleanup): thin handlers that parse + validate, map to a
//     service.TestSeedService input, and encode the result. All row
//     construction lives in the service (handler → service → repository → DB).
//   - DECLARED (/test/seed/declared and the namespaces shape of /test/cleanup,
//     in test_declared.go): the handler drives internal/synthetic/declare
//     directly, under a documented layering exception — service cannot import
//     synthetic without a cycle, and the toolkit itself writes through the real
//     services and repositories, so the layer rule holds one level down.
//
// No handler in either path calls sqlc queries.
type TestHandler struct {
	seedSvc *service.TestSeedService
	lockSvc *service.TestLockService
	// database backs the declared path only (see the layering exception above).
	database  *db.Database
	validator *validator.Validate
}

// NewTestHandler creates a new test handler over the test-seed service, the
// named-mutex arbiter, and the database the declared-seeding path drives.
func NewTestHandler(seedSvc *service.TestSeedService, lockSvc *service.TestLockService, database *db.Database) *TestHandler {
	return &TestHandler{
		seedSvc:   seedSvc,
		lockSvc:   lockSvc,
		database:  database,
		validator: sharedValidator,
	}
}

// SeedExternalContactInput represents input for creating an external contact.
// Source defaults to "test"; pass "telegram"/"gcontacts"/"gcal_attendee" to
// seed source-specific rows. display_name is optional for Telegram seeds
// where the candidate may only have a metadata.username.
type SeedExternalContactInput struct {
	DisplayName  string         `json:"display_name,omitempty" validate:"omitempty,max=255"`
	FirstName    string         `json:"first_name,omitempty" validate:"omitempty,max=255"`
	LastName     string         `json:"last_name,omitempty" validate:"omitempty,max=255"`
	Source       string         `json:"source,omitempty" validate:"omitempty,oneof=test telegram gcontacts gcal_attendee icloud_contacts anarlog_humans anarlog_title gmail_correspondence"`
	Emails       []string       `json:"emails,omitempty"`
	Phones       []string       `json:"phones,omitempty"`
	Organization string         `json:"organization,omitempty"`
	JobTitle     string         `json:"job_title,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// SeedExternalContactsRequest represents the request to seed external contacts
type SeedExternalContactsRequest struct {
	Prefix   string                     `json:"prefix" validate:"required,min=1,max=50"`
	Contacts []SeedExternalContactInput `json:"contacts" validate:"required,min=1,max=100,dive"`
	// HostID associates the seeded rows with a specific mac_host. Used
	// by host-scoped tests (e.g., the GET /host/:id/source-counts
	// endpoint). Optional — when unset, rows are seeded with NULL
	// host_id.
	HostID *string `json:"host_id,omitempty" validate:"omitempty,uuid"`
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

	var hostUUID *uuid.UUID
	if req.HostID != nil && *req.HostID != "" {
		parsed, parseErr := uuid.Parse(*req.HostID)
		if parseErr != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "invalid host_id", parseErr.Error())
			return
		}
		hostUUID = &parsed
	}

	inputs := make([]service.SeedExternalContactInput, 0, len(req.Contacts))
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

		source := input.Source
		if source == "" {
			source = "test"
		}

		// Pick a source-aware source_id format so Cleanup's prefix-based
		// DeleteExternalContactsBySourceIDPrefix picks up the rows later.
		sourceIDSuffix := "contact"
		if source == "telegram" {
			sourceIDSuffix = "tg"
		}

		svcInput := service.SeedExternalContactInput{
			Source:   source,
			SourceID: fmt.Sprintf("%s-%s-%d", req.Prefix, sourceIDSuffix, i),
			HostID:   hostUUID,
			Emails:   emails,
			Phones:   phones,
			Metadata: input.Metadata,
		}
		// display_name keeps the prefix when provided so cleanup-by-display_name works too.
		if input.DisplayName != "" {
			displayName := req.Prefix + "-" + input.DisplayName
			svcInput.DisplayName = &displayName
		}
		if input.FirstName != "" {
			firstName := input.FirstName
			svcInput.FirstName = &firstName
		}
		if input.LastName != "" {
			lastName := input.LastName
			svcInput.LastName = &lastName
		}
		if input.Organization != "" {
			svcInput.Organization = &input.Organization
		}
		if input.JobTitle != "" {
			svcInput.JobTitle = &input.JobTitle
		}
		inputs = append(inputs, svcInput)
	}

	ids, err := h.seedSvc.SeedExternalContacts(ctx, inputs)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, SeedExternalContactsResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}

// SeedMethodSuggestionMethodInput is one pending/dismissed (type,value)
// method to seed onto an address-book row.
type SeedMethodSuggestionMethodInput struct {
	Type  string `json:"type" validate:"required,oneof=email phone telegram discord twitter signal gchat whatsapp"`
	Value string `json:"value" validate:"required,max=255"`
}

// SeedMethodSuggestionsRequest seeds a linked `imported` address-book row
// carrying pending_method_suggestions (the method-suggestion card), plus
// the CRM contact it links to. The existing SeedExternalContacts helper
// cannot link a row or set the JSONB columns, so this dedicated test
// endpoint exists for the suggestions E2E. All writes go through repository
// methods + test-only sqlc setters (no raw SQL).
type SeedMethodSuggestionsRequest struct {
	Prefix string `json:"prefix" validate:"required,min=1,max=50"`
	// ContactName is the linked contact's name (prefix is prepended for
	// cleanup). The seeded contact carries no methods, so the pending
	// suggestions are not pre-applied.
	ContactName string `json:"contact_name" validate:"required,min=1,max=255"`
	// Source must be an address-book source (the suggestion surface is
	// v1-scoped to these).
	Source    string                            `json:"source,omitempty" validate:"omitempty,oneof=gcontacts icloud_contacts"`
	Pending   []SeedMethodSuggestionMethodInput `json:"pending" validate:"required,min=1,max=20,dive"`
	Dismissed []SeedMethodSuggestionMethodInput `json:"dismissed,omitempty" validate:"omitempty,max=20,dive"`
}

// SeedMethodSuggestionsResponse echoes the seeded ids for assertions.
type SeedMethodSuggestionsResponse struct {
	ExternalContactID string `json:"external_contact_id"`
	ContactID         string `json:"contact_id"`
}

// SeedMethodSuggestions creates a linked `imported` address-book row with
// pending (and optional dismissed) method suggestions.
// @Summary Seed method suggestions for testing
// @Description Create a linked imported address-book row with pending_method_suggestions for e2e
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedMethodSuggestionsRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedMethodSuggestionsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/method-suggestions [post]
func (h *TestHandler) SeedMethodSuggestions(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedMethodSuggestionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	source := req.Source
	if source == "" {
		source = "gcontacts"
	}

	// Build the pending method values as emails/phones (so they are appliable).
	emails := make([]repository.EmailEntry, 0, len(req.Pending))
	phones := make([]repository.PhoneEntry, 0, len(req.Pending))
	for _, m := range req.Pending {
		switch m.Type {
		case "email":
			emails = append(emails, repository.EmailEntry{Value: m.Value})
		case "phone":
			phones = append(phones, repository.PhoneEntry{Value: m.Value})
		}
	}

	input := service.SeedMethodSuggestionsInput{
		Source:      source,
		ContactName: req.Prefix + "-" + req.ContactName,
		DisplayName: req.Prefix + "-" + req.ContactName,
		SourceID:    fmt.Sprintf("%s-method-suggestion", req.Prefix),
		Emails:      emails,
		Phones:      phones,
		Pending:     normalizeSeedSuggestions(req.Pending),
		Dismissed:   normalizeSeedSuggestions(req.Dismissed),
	}

	res, err := h.seedSvc.SeedMethodSuggestions(ctx, input)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, SeedMethodSuggestionsResponse{
		ExternalContactID: res.ExternalContactID,
		ContactID:         res.ContactID,
	}, nil)
}

// normalizeSeedSuggestions converts seed method inputs to repository
// suggestion entries with normalized values (matching the reconcile
// path's storage convention).
func normalizeSeedSuggestions(inputs []SeedMethodSuggestionMethodInput) []repository.PendingMethodSuggestion {
	out := make([]repository.PendingMethodSuggestion, 0, len(inputs))
	for _, m := range inputs {
		out = append(out, repository.PendingMethodSuggestion{
			Type:  m.Type,
			Value: repository.NormalizeContactMethodValue(m.Type, m.Value),
		})
	}
	return out
}

// SeedContactMethodInput represents a contact method for seeding
type SeedContactMethodInput struct {
	Type      string `json:"type" validate:"required,oneof=email phone telegram discord twitter signal gchat whatsapp"`
	Value     string `json:"value" validate:"required,max=255"`
	IsPrimary bool   `json:"is_primary"`
}

// SeedContactInput represents input for creating a contact
type SeedContactInput struct {
	FullName             string                   `json:"full_name" validate:"required,min=1,max=255"`
	Location             string                   `json:"location,omitempty" validate:"omitempty,max=255"`
	Notes                string                   `json:"notes,omitempty" validate:"omitempty,max=2000"`
	Cadence              string                   `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
	Methods              []SeedContactMethodInput `json:"methods,omitempty" validate:"omitempty,max=20,dive"`
	LastContactedDaysAgo int                      `json:"last_contacted_days_ago,omitempty" validate:"omitempty,min=0,max=3650"`
}

// SeedContactsRequest represents the request to seed contacts
type SeedContactsRequest struct {
	Prefix   string             `json:"prefix" validate:"required,min=1,max=50"`
	Contacts []SeedContactInput `json:"contacts" validate:"required,min=1,max=100,dive"`
}

// SeedContactsResponse represents the response from seeding contacts
type SeedContactsResponse struct {
	Created int      `json:"created"`
	IDs     []string `json:"ids"`
}

// SeedContacts creates contacts for testing with full field support
// @Summary Seed contacts for testing
// @Description Create contacts with optional methods, location, notes, and cadence for e2e testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedContactsRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedContactsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/contacts [post]
func (h *TestHandler) SeedContacts(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedContactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	inputs := make([]service.SeedContactInput, 0, len(req.Contacts))
	for i, input := range req.Contacts {
		// Normalize and validate contact methods (reuse contact handler logic).
		// This is HTTP-boundary validation, so it stays in the handler.
		methodRequests := make([]ContactMethodRequest, len(input.Methods))
		for j, m := range input.Methods {
			methodRequests[j] = ContactMethodRequest(m)
		}

		normalizedMethods, err := normalizeContactMethodRequests(methodRequests)
		if err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
				fmt.Sprintf("Contact %d (%s): method normalization failed", i+1, input.FullName), err.Error())
			return
		}

		if err := validateContactMethods(h.validator, normalizedMethods); err != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
				fmt.Sprintf("Contact %d (%s): method validation failed", i+1, input.FullName), err.Error())
			return
		}

		inputs = append(inputs, service.SeedContactInput{
			// Prepend prefix to full_name for cleanup.
			FullName:             req.Prefix + "-" + input.FullName,
			Location:             input.Location,
			Cadence:              input.Cadence,
			Methods:              buildContactMethodInputs(normalizedMethods),
			LastContactedDaysAgo: input.LastContactedDaysAgo,
		})
	}

	ids, err := h.seedSvc.SeedContacts(ctx, inputs)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, SeedContactsResponse{
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

	inputs := make([]service.SeedOverdueContactInput, 0, len(req.Contacts))
	for _, input := range req.Contacts {
		inputs = append(inputs, service.SeedOverdueContactInput{
			// Prepend prefix to full_name for cleanup.
			FullName:    req.Prefix + "-" + input.FullName,
			Cadence:     input.Cadence,
			DaysOverdue: input.DaysOverdue,
			Email:       input.Email,
		})
	}

	ids, err := h.seedSvc.SeedOverdueContacts(ctx, inputs)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, SeedOverdueContactsResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}

// SeedCalendarEventInput represents input for creating a calendar event
type SeedCalendarEventInput struct {
	Title          string   `json:"title" validate:"required,min=1,max=255"`
	Location       string   `json:"location,omitempty"`
	HtmlLink       string   `json:"html_link,omitempty"`
	IsPast         bool     `json:"is_past,omitempty"`    // If true, event is set in the past
	DaysAgo        int      `json:"days_ago,omitempty"`   // If is_past, how many days ago (default: 7)
	DaysAhead      int      `json:"days_ahead,omitempty"` // If not is_past, how many days ahead (default: 7)
	AttendeeEmails []string `json:"attendee_emails,omitempty"`
	// Unmatched, when true, seeds the event with attendee emails but does NOT
	// add contact_id to matched_contact_ids. Used for rematch E2E tests.
	Unmatched bool `json:"unmatched,omitempty"`
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
	inputs := make([]service.SeedCalendarEventInput, 0, len(req.Events))

	for i, input := range req.Events {
		// Calculate event times based on is_past flag (HTTP-input semantics —
		// mapped to concrete times here, then persisted by the service).
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

		// Build attendee list from optional attendee_emails.
		attendees := make([]repository.Attendee, 0, len(input.AttendeeEmails))
		for _, email := range input.AttendeeEmails {
			attendees = append(attendees, repository.Attendee{Email: email})
		}

		// matched_contact_ids defaults to the request's primary contact, but
		// is empty when Unmatched is set so rematch E2E tests can drive the
		// "no link until method is added" flow.
		matchedIDs := []uuid.UUID{contactID}
		if input.Unmatched {
			matchedIDs = []uuid.UUID{}
		}

		inputs = append(inputs, service.SeedCalendarEventInput{
			GcalEventID:     fmt.Sprintf("%s-event-%d", req.Prefix, i),
			GoogleAccountID: fmt.Sprintf("%s-test-account", req.Prefix),
			Title:           req.Prefix + "-" + input.Title,
			Location:        input.Location,
			HtmlLink:        input.HtmlLink,
			StartTime:       startTime,
			EndTime:         endTime,
			Attendees:       attendees,
			MatchedIDs:      matchedIDs,
		})
	}

	ids, err := h.seedSvc.SeedCalendarEvents(ctx, inputs)
	if err != nil {
		api.RespondInternal(c, err)
		return
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

// CleanupRequest represents the request to cleanup test data. It is dual-shape
// during the migration to declared seeding, and the caller must supply EXACTLY
// ONE of the two:
//
//   - prefix  — the bespoke shape: delete rows whose identifying strings carry
//     the test's prefix. Unchanged behavior.
//   - namespaces — the declared shape: remove the worlds those requested
//     namespace tokens resolved to, per-namespace outcomes.
type CleanupRequest struct {
	Prefix string `json:"prefix,omitempty" validate:"omitempty,min=1,max=50"`
	// HostID, when set, also hard-deletes every meeting_note owned by that
	// mac_host (seeded session UUIDs are random, so there is no prefix to
	// match on — cleanup is by host instead). Prefix shape only.
	HostID string `json:"host_id,omitempty" validate:"omitempty,uuid"`
	// Namespaces are the REQUESTED namespace tokens the client seeded under.
	// The server expands each to the effective (possibly re-salted) worlds, so
	// a client that never saw a seed response can still clean up.
	Namespaces []string `json:"namespaces,omitempty" validate:"omitempty,max=32,dive,min=1,max=60"`
	// Seed must match the seed the namespaces were created with; 0 or absent
	// uses the default.
	Seed uint64 `json:"seed,omitempty"`
}

// CleanupResponse represents the response from cleanup by prefix
type CleanupResponse struct {
	DeletedContacts         int64 `json:"deleted_contacts"`
	DeletedExternalContacts int64 `json:"deleted_external_contacts"`
	DeletedCalendarEvents   int64 `json:"deleted_calendar_events"`
}

// CleanupResponseData is the DOCUMENTED shape of this endpoint's 200 payload.
// The endpoint is dual-shape and Swagger 2.0 has no oneOf, so the documented
// schema is the union of the two real payloads — embedded, never re-declared,
// so it cannot drift from what the handler actually returns:
//
//   - a `prefix` request returns exactly CleanupResponse's fields;
//   - a `namespaces` request returns exactly CleanupNamespacesResponse's.
//
// No response ever carries both groups. This type exists only to make both
// schemas — including the per-namespace outcome — reachable in the generated
// spec; the handler serializes the concrete types.
type CleanupResponseData struct {
	CleanupResponse
	CleanupNamespacesResponse
}

// Cleanup deletes test data by prefix or by declared namespace
// @Summary Cleanup test data
// @Description Delete test data. Supply EXACTLY ONE of `prefix` (bespoke shape — returns the CleanupResponse fields: contacts, external contacts and calendar events deleted by prefix) or `namespaces` (declared shape — returns the CleanupNamespacesResponse fields: per-requested-token expansions plus a per-effective-namespace outcome). The documented 200 schema is the union of the two; a given response carries one group, never both.
// @Tags test
// @Accept json
// @Produce json
// @Param body body CleanupRequest true "Cleanup request"
// @Success 200 {object} api.APIResponse{data=CleanupResponseData}
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

	// Exactly one shape. Accepting both would leave the response ambiguous
	// (the two shapes report different things); accepting neither would be a
	// silent no-op that a caller could mistake for a successful cleanup.
	switch {
	case len(req.Namespaces) > 0 && req.Prefix != "":
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
			"Supply exactly one of prefix or namespaces", "both were provided")
		return
	case len(req.Namespaces) == 0 && req.Prefix == "":
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
			"Supply exactly one of prefix or namespaces", "neither was provided")
		return
	case len(req.Namespaces) > 0:
		h.cleanupNamespaces(c, req.Namespaces, req.Seed)
		return
	}

	// Meeting notes are seeded with random session UUIDs scoped to a host, so
	// cleanup is by host id rather than by prefix. Parse it at the boundary.
	var hostID *uuid.UUID
	if req.HostID != "" {
		parsed, parseErr := uuid.Parse(req.HostID)
		if parseErr != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "invalid host_id", parseErr.Error())
			return
		}
		hostID = &parsed
	}

	res, err := h.seedSvc.Cleanup(ctx, req.Prefix, hostID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, CleanupResponse{
		DeletedContacts:         res.DeletedContacts,
		DeletedExternalContacts: res.DeletedExternalContacts,
		DeletedCalendarEvents:   res.DeletedCalendarEvents,
	}, nil)
}

// SeedMacHostRequest is the payload for /test/seed/mac-hosts. It
// creates a paired host row directly so E2E tests can exercise the
// paired-state UI without going through the real pairing flow (the
// daemon-side pairing token + key are not available to the browser).
type SeedMacHostRequest struct {
	Hostname        string         `json:"hostname"`
	DaemonVersion   string         `json:"daemon_version,omitempty"`
	ProtocolVersion int32          `json:"protocol_version,omitempty"`
	Permissions     map[string]any `json:"permissions,omitempty"`
	SourceHealth    map[string]any `json:"source_health,omitempty"`
}

// SeedMacHostResponse echoes the generated host id.
type SeedMacHostResponse struct {
	HostID string `json:"host_id"`
}

// SeedMacHost creates a mac_host row directly (bypassing the pairing
// flow) so E2E tests can exercise the paired-host UI. Routes through
// the MacHostRepository's SeedHostForTest helper so the handler does
// not call sqlc queries directly.
func (h *TestHandler) SeedMacHost(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedMacHostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if req.Hostname == "" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "hostname is required", "")
		return
	}
	if req.DaemonVersion == "" {
		req.DaemonVersion = "test-seed"
	}
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = 1
	}

	hostID, err := h.seedSvc.SeedMacHost(ctx, service.SeedMacHostInput{
		Hostname:        req.Hostname,
		DaemonVersion:   req.DaemonVersion,
		ProtocolVersion: req.ProtocolVersion,
		Permissions:     req.Permissions,
		SourceHealth:    req.SourceHealth,
	})
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, SeedMacHostResponse{HostID: hostID}, nil)
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

// SeedMeetingNoteInput describes one meeting_note row to seed. Only the
// orphan_needs_review state is supported here (no candidate snapshot is
// required), which is what the Imports Interactions E2E exercises:
// the amber badge, the orphan card, "Log as impromptu", the empty state,
// the ?session deep-link highlight, and the tab alias. Conflict rows need
// a well-formed conflict_candidates snapshot referencing real events and
// are covered by the backend integration tests instead.
type SeedMeetingNoteInput struct {
	AnarlogSessionID string `json:"anarlog_session_id" validate:"required,uuid"`
	Title            string `json:"title,omitempty" validate:"omitempty,max=500"`
	Summary          string `json:"summary,omitempty" validate:"omitempty,max=2000"`
}

// SeedMeetingNotesRequest seeds orphan meeting_note rows against a paired
// mac_host so the Imports Interactions tab has rows to render. The host id
// scopes cleanup (the seeded session UUIDs are caller-supplied but cleanup
// is by host).
type SeedMeetingNotesRequest struct {
	HostID string                 `json:"host_id" validate:"required,uuid"`
	Notes  []SeedMeetingNoteInput `json:"notes" validate:"required,min=1,max=50,dive"`
}

// SeedMeetingNotesResponse reports the created meeting_note ids.
type SeedMeetingNotesResponse struct {
	Created int      `json:"created"`
	IDs     []string `json:"ids"`
}

// SeedMeetingNotes creates orphan_needs_review meeting_note rows for E2E.
// @Summary Seed orphan meeting notes for testing
// @Description Create orphan_needs_review meeting_note rows for e2e testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedMeetingNotesRequest true "Seed request"
// @Success 201 {object} api.APIResponse{data=SeedMeetingNotesResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/seed/meeting-notes [post]
func (h *TestHandler) SeedMeetingNotes(c *gin.Context) {
	ctx := c.Request.Context()

	var req SeedMeetingNotesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	hostID, err := uuid.Parse(req.HostID)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "invalid host_id", err.Error())
		return
	}

	notes := make([]service.SeedMeetingNoteInput, 0, len(req.Notes))
	for _, note := range req.Notes {
		sessionID, parseErr := uuid.Parse(note.AnarlogSessionID)
		if parseErr != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "invalid anarlog_session_id", parseErr.Error())
			return
		}
		notes = append(notes, service.SeedMeetingNoteInput{
			AnarlogSessionID: sessionID,
			Title:            note.Title,
			Summary:          note.Summary,
		})
	}

	ids, err := h.seedSvc.SeedMeetingNotes(ctx, hostID, notes)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, SeedMeetingNotesResponse{
		Created: len(ids),
		IDs:     ids,
	}, nil)
}
