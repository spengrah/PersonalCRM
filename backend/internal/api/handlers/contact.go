package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
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

// ContactHandler handles contact-related HTTP requests
type ContactHandler struct {
	contactService *service.ContactService
	validator      *validator.Validate
}

// NewContactHandler creates a new contact handler
func NewContactHandler(contactService *service.ContactService) *ContactHandler {
	return &ContactHandler{
		contactService: contactService,
		validator:      validator.New(),
	}
}

// Contact response model
// @Description Contact information
type ContactResponse struct {
	ID            string                  `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	FullName      string                  `json:"full_name" example:"John Doe"`
	Methods       []ContactMethodResponse `json:"methods,omitempty"`
	PrimaryMethod *ContactMethodResponse  `json:"primary_method,omitempty"`
	Location      *string                 `json:"location,omitempty" example:"San Francisco, CA"`
	Birthday      *time.Time              `json:"birthday,omitempty" example:"1990-01-15T00:00:00Z"`
	HowMet        *string                 `json:"how_met,omitempty" example:"Met at tech conference"`
	Cadence       *string                 `json:"cadence,omitempty" example:"monthly" enums:"weekly,monthly,quarterly,biannual,annual"`
	LastContacted *time.Time              `json:"last_contacted,omitempty" example:"2024-01-15T10:30:00Z"`
	ProfilePhoto  *string                 `json:"profile_photo,omitempty" example:"https://example.com/photo.jpg"`
	CreatedAt     time.Time               `json:"created_at" example:"2024-01-01T00:00:00Z"`
	UpdatedAt     time.Time               `json:"updated_at" example:"2024-01-15T10:30:00Z"`
}

// ContactMethodResponse represents a contact method in responses
type ContactMethodResponse struct {
	ID        string `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	Type      string `json:"type" example:"email_personal"`
	Value     string `json:"value" example:"john.doe@example.com"`
	IsPrimary bool   `json:"is_primary" example:"true"`
}

// OverdueContactResponse represents an overdue contact with additional metadata
// @Description Overdue contact information with action metadata
type OverdueContactResponse struct {
	ContactResponse
	DaysOverdue     int       `json:"days_overdue" example:"5"`
	NextDueDate     time.Time `json:"next_due_date" example:"2024-01-15T00:00:00Z"`
	SuggestedAction string    `json:"suggested_action" example:"Send a quick check-in message"`
}

// CreateContactRequest represents the request to create a contact
// @Description Create contact request
type CreateContactRequest struct {
	FullName     string                 `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Methods      []ContactMethodRequest `json:"methods,omitempty" validate:"omitempty,dive"`
	Location     *string                `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly              `json:"birthday,omitempty" example:"1990-01-15"`
	HowMet       *string                `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string                `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string                `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// UpdateContactRequest represents the request to update a contact
// @Description Update contact request
type UpdateContactRequest struct {
	FullName     string                 `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Methods      []ContactMethodRequest `json:"methods,omitempty" validate:"omitempty,dive"`
	Location     *string                `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly              `json:"birthday,omitempty" example:"1990-01-15"`
	HowMet       *string                `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string                `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string                `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// ListContactsQuery represents query parameters for listing contacts
type ListContactsQuery struct {
	Page   int    `form:"page" validate:"omitempty,min=1" example:"1"`
	Limit  int    `form:"limit" validate:"omitempty,min=1,max=1000" example:"20"`
	Search string `form:"search" validate:"omitempty,max=255" example:"john"`
	Sort   string `form:"sort" validate:"omitempty,oneof=name location birthday last_contacted" example:"name"`
	Order  string `form:"order" validate:"omitempty,oneof=asc desc" example:"asc"`
}

// ContactMethodRequest represents a single contact method in requests
type ContactMethodRequest struct {
	Type      string `json:"type" validate:"required,oneof=email_personal email_work phone telegram discord twitter signal gchat" example:"email_personal"`
	Value     string `json:"value" validate:"required,max=255" example:"john.doe@example.com"`
	IsPrimary bool   `json:"is_primary" example:"true"`
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
		ID:            contact.ID.String(),
		FullName:      contact.FullName,
		Methods:       methods,
		PrimaryMethod: primaryMethod,
		Location:      contact.Location,
		Birthday:      contact.Birthday,
		HowMet:        contact.HowMet,
		Cadence:       contact.Cadence,
		LastContacted: contact.LastContacted,
		ProfilePhoto:  contact.ProfilePhoto,
		CreatedAt:     contact.CreatedAt,
		UpdatedAt:     contact.UpdatedAt,
	}
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

	contact, err := h.contactService.CreateContact(
		c.Request.Context(),
		createRequestToRepo(req),
		buildContactMethodInputs(req.Methods),
	)
	if err != nil {
		api.SendInternalError(c, "Failed to create contact")
		return
	}

	response := contactToResponse(contact)
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
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	contact, err := h.contactService.GetContact(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to retrieve contact")
		return
	}

	if contact == nil {
		api.SendNotFound(c, "Contact")
		return
	}

	response := contactToResponse(contact)
	api.SendSuccess(c, http.StatusOK, response, nil)
}

// ListContacts retrieves a paginated list of contacts
// @Summary List contacts
// @Description Get a paginated list of contacts with optional search and sorting
// @Tags contacts
// @Produce json
// @Param page query int false "Page number" default(1) minimum(1)
// @Param limit query int false "Items per page" default(20) minimum(1) maximum(100)
// @Param search query string false "Search term (name or contact methods)" maxlength(255)
// @Param sort query string false "Sort by field" Enums(name, location, birthday, last_contacted) default("")
// @Param order query string false "Sort order" Enums(asc, desc) default("asc")
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

	var contacts []repository.Contact
	var err error

	// Fetch all contacts (we'll sort and paginate in memory)
	if query.Search != "" {
		// Search contacts - fetch all to allow sorting
		contacts, err = h.contactService.SearchContacts(c.Request.Context(), repository.SearchContactsParams{
			Query:  query.Search,
			Limit:  10000, // Large limit to get all search results
			Offset: 0,
		})
	} else {
		// List all contacts
		contacts, err = h.contactService.ListContacts(c.Request.Context(), repository.ListContactsParams{
			Limit:  10000, // Large limit to get all contacts
			Offset: 0,
		})
	}

	if err != nil {
		api.SendInternalError(c, "Failed to retrieve contacts")
		return
	}

	// Apply sorting if requested
	if query.Sort != "" {
		sort.Slice(contacts, func(i, j int) bool {
			var less bool
			switch query.Sort {
			case "name":
				less = contacts[i].FullName < contacts[j].FullName
			case "location":
				loc1 := ""
				loc2 := ""
				if contacts[i].Location != nil {
					loc1 = *contacts[i].Location
				}
				if contacts[j].Location != nil {
					loc2 = *contacts[j].Location
				}
				less = loc1 < loc2
			case "birthday":
				// Handle nil birthdays - put them at the end
				if contacts[i].Birthday == nil && contacts[j].Birthday == nil {
					less = false
				} else if contacts[i].Birthday == nil {
					less = false
				} else if contacts[j].Birthday == nil {
					less = true
				} else {
					less = contacts[i].Birthday.Before(*contacts[j].Birthday)
				}
			case "last_contacted":
				// Handle nil last_contacted - put them at the end
				if contacts[i].LastContacted == nil && contacts[j].LastContacted == nil {
					less = false
				} else if contacts[i].LastContacted == nil {
					less = false
				} else if contacts[j].LastContacted == nil {
					less = true
				} else {
					less = contacts[i].LastContacted.Before(*contacts[j].LastContacted)
				}
			default:
				less = contacts[i].FullName < contacts[j].FullName
			}

			if query.Order == "desc" {
				return !less
			}
			return less
		})
	}

	// Apply pagination after sorting
	total := int64(len(contacts))
	offset := (query.Page - 1) * query.Limit
	end := offset + query.Limit

	if offset > int(total) {
		offset = int(total)
	}
	if end > int(total) {
		end = int(total)
	}

	paginatedContacts := contacts[offset:end]

	// Convert to response format
	responses := make([]ContactResponse, len(paginatedContacts))
	for i, contact := range paginatedContacts {
		responses[i] = contactToResponse(&contact)
	}

	totalPages := int(total) / query.Limit
	if int(total)%query.Limit > 0 {
		totalPages++
	}

	meta := &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  query.Page,
			Limit: query.Limit,
			Total: total,
			Pages: totalPages,
		},
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
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	var req UpdateContactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	methodsProvided := req.Methods != nil
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

	contact, err := h.contactService.UpdateContact(
		c.Request.Context(),
		id,
		updateRequestToRepo(req),
		buildContactMethodInputs(req.Methods),
		methodsProvided,
	)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to update contact")
		return
	}

	response := contactToResponse(contact)
	api.SendSuccess(c, http.StatusOK, response, nil)
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
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	err = h.contactService.DeleteContact(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to delete contact")
		return
	}

	c.Status(http.StatusNoContent)
}

// UpdateContactLastContacted updates the last contacted date for a contact
// @Summary Update last contacted date
// @Description Update when a contact was last contacted
// @Tags contacts
// @Accept json
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Success 200 {object} api.APIResponse "Last contacted date updated successfully"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid contact ID"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/last-contacted [patch]
func (h *ContactHandler) UpdateContactLastContacted(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	updatedContact, err := h.contactService.UpdateContactLastContacted(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to update last contacted date")
		return
	}

	response := contactToResponse(updatedContact)
	api.SendSuccess(c, http.StatusOK, response, nil)
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
		api.SendInternalError(c, "Failed to retrieve contacts")
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
		if method.Type == string(repository.ContactMethodTelegram) || method.Type == string(repository.ContactMethodTwitter) {
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

	types := make(map[string]struct{}, len(methods))
	primaryCount := 0

	for _, method := range methods {
		if _, exists := types[method.Type]; exists {
			return fmt.Errorf("duplicate contact method type: %s", method.Type)
		}
		types[method.Type] = struct{}{}

		if method.IsPrimary {
			primaryCount++
			if primaryCount > 1 {
				return errors.New("only one contact method can be primary")
			}
		}

		switch method.Type {
		case string(repository.ContactMethodEmailPersonal),
			string(repository.ContactMethodEmailWork),
			string(repository.ContactMethodGChat):
			if err := validate.Var(method.Value, "email"); err != nil {
				return fmt.Errorf("invalid email for contact method %s", method.Type)
			}
		case string(repository.ContactMethodPhone),
			string(repository.ContactMethodSignal):
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
