package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// DateOnly represents a date-only value that can be unmarshaled from JSON
type DateOnly struct {
	*time.Time
}

// UnmarshalJSON implements json.Unmarshaler for DateOnly
func (d *DateOnly) UnmarshalJSON(data []byte) error {
	// Remove quotes from JSON string
	s := strings.Trim(string(data), "\"")

	if s == "null" || s == "" {
		d.Time = nil
		return nil
	}

	// Try parsing as date only first (YYYY-MM-DD)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		d.Time = &t
		return nil
	}

	// Fall back to RFC3339 format if needed
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		d.Time = &t
		return nil
	}

	return errors.New("invalid date format, expected YYYY-MM-DD")
}

// MarshalJSON implements json.Marshaler for DateOnly
func (d DateOnly) MarshalJSON() ([]byte, error) {
	if d.Time == nil {
		return []byte("null"), nil
	}
	return json.Marshal(d.Format("2006-01-02"))
}

// ContactHandler handles contact-related HTTP requests. Manual
// interactions are owned by InteractionHandler
// (POST /contacts/:id/interactions); ContactHandler does not need a
// reference to the manual-interaction pipeline.
type ContactHandler struct {
	contactService *service.ContactService
	validator      *validator.Validate
}

// NewContactHandler creates a new contact handler.
func NewContactHandler(contactService *service.ContactService) *ContactHandler {
	return &ContactHandler{
		contactService: contactService,
		validator:      sharedValidator,
	}
}

// ListContactsQuery represents query parameters for listing contacts
type ListContactsQuery struct {
	Page           int    `form:"page" validate:"omitempty,min=1" example:"1"`
	Limit          int    `form:"limit" validate:"omitempty,min=1,max=1000" example:"20"`
	Search         string `form:"search" validate:"omitempty,max=255" example:"john"`
	Sort           string `form:"sort" validate:"omitempty,oneof=name location birthday last_contacted last_response_at contact_by cadence" example:"name"`
	Order          string `form:"order" validate:"omitempty,oneof=asc desc" example:"asc"`
	IDsOnly        bool   `form:"ids_only" example:"false"`
	CadenceFilter  string `form:"cadence_filter" validate:"omitempty,oneof=has_cadence no_cadence" example:"has_cadence"`
	FollowupFilter string `form:"followup_filter" validate:"omitempty,oneof=has_followup no_followup" example:"has_followup"`
}

// Helper function to convert repository contact to response
func contactToResponse(contact *repository.Contact) ContactResponse {
	methods := make([]ContactMethodResponse, len(contact.Methods))
	for i, method := range contact.Methods {
		methods[i] = contactMethodToResponse(method)
	}

	var primaryMethod *ContactMethodResponse
	if contact.PrimaryMethod != nil {
		primary := contactMethodToResponse(*contact.PrimaryMethod)
		primaryMethod = &primary
	}

	return ContactResponse{
		ID:                contact.ID.String(),
		FullName:          contact.FullName,
		Methods:           methods,
		PrimaryMethod:     primaryMethod,
		Location:          contact.Location,
		Birthday:          contact.Birthday,
		HowMet:            contact.HowMet,
		Cadence:           contact.Cadence,
		LastContacted:     contact.LastContacted,
		ContactBy:         contact.ContactBy,
		LastInteractionAt: contact.LastInteractionAt,
		LastOutreachAt:    contact.LastOutreachAt,
		LastResponseAt:    contact.LastResponseAt,
		ProfilePhoto:      contact.ProfilePhoto,
		CreatedAt:         contact.CreatedAt,
		UpdatedAt:         contact.UpdatedAt,
	}
}

// nilStringPtrFromUUID converts a uuid to a *string, returning nil for uuid.Nil.
// Used to populate optional rematch_job_id fields on response DTOs.
func nilStringPtrFromUUID(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

func contactMethodToResponse(method repository.ContactMethod) ContactMethodResponse {
	return ContactMethodResponse{
		ID:        method.ID.String(),
		Type:      method.Type,
		Value:     method.Value,
		IsPrimary: method.IsPrimary,
	}
}

// Helper function to convert create request to repository request
func createRequestToRepo(req CreateContactRequest) repository.CreateContactRequest {
	var birthday *time.Time
	if req.Birthday != nil {
		birthday = req.Birthday.Time
	}

	// Set last_contacted to current date when creating a contact
	now := accelerated.GetCurrentTime()

	return repository.CreateContactRequest{
		FullName:      req.FullName,
		Location:      req.Location,
		Birthday:      birthday,
		HowMet:        req.HowMet,
		Cadence:       req.Cadence,
		LastContacted: &now,
		ProfilePhoto:  req.ProfilePhoto,
	}
}

// Helper function to convert update request to repository request
func updateRequestToRepo(req UpdateContactRequest) repository.UpdateContactRequest {
	var birthday *time.Time
	if req.Birthday != nil {
		birthday = req.Birthday.Time
	}

	return repository.UpdateContactRequest{
		FullName:     req.FullName,
		Location:     req.Location,
		Birthday:     birthday,
		HowMet:       req.HowMet,
		Cadence:      req.Cadence,
		ProfilePhoto: req.ProfilePhoto,
	}
}

// CreateContact creates a new contact
// @Summary Create a new contact
// @Description Create a new contact with the provided information
// @Tags contacts
// @Accept json
// @Produce json
// @Param contact body CreateContactRequest true "Contact information"
// @Success 201 {object} api.APIResponse{data=ContactResponse} "Contact created successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid request"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts [post]
func (h *ContactHandler) CreateContact(c *gin.Context) {
	var req CreateContactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	normalizedMethods, err := normalizeContactMethodRequests(req.Methods)
	if err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}
	req.Methods = normalizedMethods

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	if err := validateContactMethods(h.validator, req.Methods); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	contact, rematchJobID, err := h.contactService.CreateContact(
		c.Request.Context(),
		createRequestToRepo(req),
		buildContactMethodInputs(req.Methods),
	)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	response := contactToResponse(contact)
	response.RematchJobID = nilStringPtrFromUUID(rematchJobID)
	api.SendSuccess(c, http.StatusCreated, response, nil)
}

// GetContact retrieves a contact by ID
// @Summary Get a contact by ID
// @Description Get a specific contact by its ID
// @Tags contacts
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=ContactResponse} "Contact retrieved successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid contact ID"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id} [get]
func (h *ContactHandler) GetContact(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	contact, err := h.contactService.GetContact(c.Request.Context(), id)
	if err != nil {
		api.RespondError(c, err, "Contact")
		return
	}

	if contact == nil {
		api.SendNotFound(c, "Contact")
		return
	}

	response := contactToResponse(contact)

	// Compute has_pending_followup via service layer
	hasPending, err := h.contactService.HasPendingFollowUp(c.Request.Context(), id)
	if err != nil {
		logger.Warn().Err(err).Str("contactId", id.String()).Msg("failed to check pending follow-up")
	}
	response.HasPendingFollowup = hasPending

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// ListContacts retrieves a paginated list of contacts
// @Summary List contacts
// @Description Get a paginated list of contacts with optional search and sorting. Use ids_only=true to get just IDs for navigation.
// @Tags contacts
// @Produce json
// @Param page query int false "Page number" default(1) minimum(1)
// @Param limit query int false "Items per page" default(20) minimum(1) maximum(100)
// @Param search query string false "Search term (name or contact methods)" maxlength(255)
// @Param sort query string false "Sort by field" Enums(name, location, birthday, last_contacted, last_response_at, contact_by, cadence) default("")
// @Param order query string false "Sort order" Enums(asc, desc) default("asc")
// @Param ids_only query bool false "Return only contact IDs (for navigation)" default(false)
// @Success 200 {object} api.APIResponse{data=[]ContactResponse,meta=api.Meta} "Contacts retrieved successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid query parameters"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts [get]
func (h *ContactHandler) ListContacts(c *gin.Context) {
	var query ListContactsQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		api.SendValidationError(c, "Invalid query parameters", err.Error())
		return
	}

	if err := h.validator.Struct(query); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Set defaults
	if query.Page == 0 {
		query.Page = 1
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Order == "" {
		query.Order = "asc"
	}

	// Handle IDs-only request (lightweight response for navigation)
	if query.IDsOnly {
		ids, err := h.contactService.ListContactIDs(c.Request.Context(), repository.ListContactIDsParams{
			Sort:           query.Sort,
			Order:          query.Order,
			Search:         query.Search,
			CadenceFilter:  query.CadenceFilter,
			FollowupFilter: query.FollowupFilter,
		})
		if err != nil {
			api.RespondInternal(c, err)
			return
		}

		// Convert UUIDs to strings
		idStrings := make([]string, len(ids))
		for i, id := range ids {
			idStrings[i] = id.String()
		}

		response := ContactIDsResponse{
			IDs:   idStrings,
			Total: len(idStrings),
		}
		api.SendSuccess(c, http.StatusOK, response, nil)
		return
	}

	offset := int32((query.Page - 1) * query.Limit)
	limit := int32(query.Limit)

	contacts, total, err := h.contactService.ListContactsPage(c.Request.Context(), repository.ListContactsParams{
		Query:          query.Search,
		Limit:          limit,
		Offset:         offset,
		Sort:           query.Sort,
		Order:          query.Order,
		CadenceFilter:  query.CadenceFilter,
		FollowupFilter: query.FollowupFilter,
	})
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Convert to response format
	responses := make([]ContactResponse, len(contacts))
	for i, contact := range contacts {
		responses[i] = contactToResponse(&contact)
	}

	meta := &api.Meta{
		Pagination: api.BuildPaginationMeta(query.Page, query.Limit, total),
	}

	api.SendSuccess(c, http.StatusOK, responses, meta)
}

// UpdateContact updates an existing contact
// @Summary Update a contact
// @Description Update a contact with the provided information
// @Tags contacts
// @Accept json
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Param contact body UpdateContactRequest true "Updated contact information"
// @Success 200 {object} api.APIResponse{data=ContactResponse} "Contact updated successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid request"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id} [put]
func (h *ContactHandler) UpdateContact(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	// Both binds must be ShouldBindBodyWithJSON: a plain ShouldBindJSON consumes
	// the body, and Gin caches it only when the ShouldBindBodyWith* call performs
	// the read itself — so a later probe would see nothing.
	var req UpdateContactRequest
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	// Contact methods are no longer part of this request. Dropping the field
	// silently would let a stale browser or a naive client send a method
	// addition, receive 200, and believe it landed — a silent-success failure,
	// the same class this work eliminates, merely inverted from silent
	// destruction. So the key is detected and rejected.
	var legacy legacyContactMethodsProbe
	if err := c.ShouldBindBodyWithJSON(&legacy); err == nil && legacy.Methods != nil {
		api.SendValidationError(c,
			"Validation failed",
			"contact methods are no longer accepted on this endpoint; use POST /contacts/{id}/methods",
		)
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	contact, err := h.contactService.UpdateContact(
		c.Request.Context(),
		id,
		updateRequestToRepo(req),
	)
	if err != nil {
		api.RespondError(c, err, "Contact")
		return
	}

	// RematchJobID is populated by the create path only. A rematch is triggered
	// by newly-present method values, and this request can no longer carry any.
	api.SendSuccess(c, http.StatusOK, contactToResponse(contact), nil)
}

// legacyContactMethodsProbe detects a `methods` key on the update payload.
//
// json.RawMessage is deliberately NOT a pointer: a *json.RawMessage cannot tell
// `"methods": null` from an absent key, and a client sending an explicit null
// has still addressed the retired field.
type legacyContactMethodsProbe struct {
	Methods json.RawMessage `json:"methods"`
}

// DeleteContact deletes a contact
// @Summary Delete a contact
// @Description Soft delete a contact by ID
// @Tags contacts
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Success 204 "Contact deleted successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid contact ID"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id} [delete]
func (h *ContactHandler) DeleteContact(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	err := h.contactService.DeleteContact(c.Request.Context(), id)
	if err != nil {
		api.RespondError(c, err, "Contact")
		return
	}

	c.Status(http.StatusNoContent)
}

// ListOverdueContacts retrieves contacts that are overdue for contact
// @Summary List overdue contacts
// @Description Get contacts that are overdue based on their cadence settings
// @Tags contacts
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]OverdueContactResponse} "Overdue contacts retrieved successfully"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/overdue [get]
func (h *ContactHandler) ListOverdueContacts(c *gin.Context) {
	overdueContacts, err := h.contactService.ListOverdueContacts(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	responses := make([]OverdueContactResponse, len(overdueContacts))
	for i, contact := range overdueContacts {
		responses[i] = OverdueContactResponse{
			ContactResponse: contactToResponse(&contact.Contact),
			DaysOverdue:     contact.DaysOverdue,
			NextDueDate:     contact.NextDueDate,
			SuggestedAction: contact.SuggestedAction,
		}
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}

func normalizeContactMethodRequests(methods []ContactMethodRequest) ([]ContactMethodRequest, error) {
	if methods == nil {
		return nil, nil
	}

	normalized := make([]ContactMethodRequest, 0, len(methods))
	for _, method := range methods {
		method.Type = strings.TrimSpace(method.Type)
		rawValue := strings.TrimSpace(method.Value)
		if rawValue == "" {
			continue
		}

		value := rawValue
		if method.Type == string(repository.ContactMethodTelegram) ||
			method.Type == string(repository.ContactMethodTwitter) ||
			method.Type == string(repository.ContactMethodDiscord) {
			value = strings.TrimLeft(value, "@")
			value = strings.TrimSpace(value)
		}

		if value == "" {
			continue
		}

		method.Value = value
		normalized = append(normalized, method)
	}

	return normalized, nil
}

func validateContactMethods(validate *validator.Validate, methods []ContactMethodRequest) error {
	if len(methods) == 0 {
		return nil
	}

	normalizedByType := make(map[string]struct{}, len(methods))
	primaryCount := 0

	for _, method := range methods {
		normalizedValue := repository.NormalizeContactMethodValue(method.Type, method.Value)
		if normalizedValue == "" {
			return fmt.Errorf("contact method %s has empty normalized value", method.Type)
		}
		key := method.Type + ":" + normalizedValue
		if _, exists := normalizedByType[key]; exists {
			return fmt.Errorf("duplicate contact method value for type: %s", method.Type)
		}
		normalizedByType[key] = struct{}{}

		if method.IsPrimary {
			primaryCount++
			if primaryCount > 1 {
				return errors.New("only one contact method can be primary")
			}
		}

		switch method.Type {
		case string(repository.ContactMethodEmail),
			string(repository.ContactMethodGChat):
			if err := validate.Var(method.Value, "email"); err != nil {
				return fmt.Errorf("invalid email for contact method %s", method.Type)
			}
		case string(repository.ContactMethodPhone),
			string(repository.ContactMethodSignal),
			string(repository.ContactMethodWhatsApp):
			if len(method.Value) > 50 {
				return fmt.Errorf("contact method %s must be less than 50 characters", method.Type)
			}
		}
	}

	return nil
}

func buildContactMethodInputs(methods []ContactMethodRequest) []service.ContactMethodInput {
	if len(methods) == 0 {
		return nil
	}

	inputs := make([]service.ContactMethodInput, len(methods))
	for i, method := range methods {
		inputs[i] = service.ContactMethodInput{
			Type:      method.Type,
			Value:     method.Value,
			IsPrimary: method.IsPrimary,
		}
	}

	return inputs
}

// GetMergePreview returns a preview of what will happen when merging two contacts
// @Summary Preview contact merge
// @Description Get a preview of what will be merged when combining two contacts
// @Tags contacts
// @Produce json
// @Param id path string true "Target Contact ID (contact to keep)" format(uuid)
// @Param source_id query string true "Source Contact ID (contact to merge from)" format(uuid)
// @Success 200 {object} api.APIResponse{data=MergePreviewResponse} "Merge preview retrieved successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid contact ID"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/merge/preview [get]
func (h *ContactHandler) GetMergePreview(c *gin.Context) {
	// Parse target contact ID from path
	targetID, ok := api.ParseUUIDParam(c, "id", "target contact")
	if !ok {
		return
	}

	// Parse source contact ID from query
	sourceIDStr := c.Query("source_id")
	if sourceIDStr == "" {
		api.SendValidationError(c, "Missing source_id", "source_id query parameter is required")
		return
	}
	sourceID, err := uuid.Parse(sourceIDStr)
	if err != nil {
		api.SendValidationError(c, "Invalid source contact ID", "source_id must be a valid UUID")
		return
	}

	preview, err := h.contactService.GetMergePreview(c.Request.Context(), sourceID, targetID)
	if err != nil {
		api.RespondError(c, err, "Contact")
		return
	}

	response := MergePreviewResponse{
		SourceContact:          contactToResponse(preview.SourceContact),
		TargetContact:          contactToResponse(preview.TargetContact),
		MethodsToTransfer:      preview.MethodsToTransfer,
		DuplicateMethods:       preview.DuplicateMethods,
		NotesToTransfer:        preview.NotesToTransfer,
		InteractionsToTransfer: preview.InteractionsToTransfer,
		CalendarEventsToUpdate: preview.CalendarEventsToUpdate,
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// MergeContacts merges a source contact into a target contact
// @Summary Merge contacts
// @Description Merge one contact into another. The source contact will be archived.
// @Tags contacts
// @Accept json
// @Produce json
// @Param id path string true "Target Contact ID (contact to keep)" format(uuid)
// @Param request body MergeContactsRequest true "Merge request"
// @Success 200 {object} api.APIResponse{data=ContactResponse} "Contacts merged successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid request"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/merge [post]
func (h *ContactHandler) MergeContacts(c *gin.Context) {
	// Parse target contact ID from path
	targetID, ok := api.ParseUUIDParam(c, "id", "target contact")
	if !ok {
		return
	}

	var req MergeContactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	sourceID, err := uuid.Parse(req.SourceContactID)
	if err != nil {
		api.SendValidationError(c, "Invalid source contact ID", "source_contact_id must be a valid UUID")
		return
	}

	// Build service request
	serviceReq := service.MergeContactsRequest{
		SourceContactID: sourceID,
		TargetContactID: targetID,
		FieldSelections: service.MergeFieldSelections{
			Cadence:  req.FieldSelections.Cadence,
			Location: req.FieldSelections.Location,
			Birthday: req.FieldSelections.Birthday,
		},
		NewName: req.NewName,
	}

	mergedContact, err := h.contactService.MergeContacts(c.Request.Context(), serviceReq)
	if err != nil {
		api.RespondError(c, err, "Contact")
		return
	}

	response := contactToResponse(mergedContact)
	api.SendSuccess(c, http.StatusOK, response, nil)
}
