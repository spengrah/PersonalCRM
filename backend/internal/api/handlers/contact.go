package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

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
	return json.Marshal(d.Time.Format("2006-01-02"))
}

// ContactHandler handles contact-related HTTP requests
type ContactHandler struct {
	contactRepo *repository.ContactRepository
	validator   *validator.Validate
}

// NewContactHandler creates a new contact handler
func NewContactHandler(contactRepo *repository.ContactRepository) *ContactHandler {
	return &ContactHandler{
		contactRepo: contactRepo,
		validator:   validator.New(),
	}
}

// Contact response model
// @Description Contact information
type ContactResponse struct {
	ID            string     `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	FullName      string     `json:"full_name" example:"John Doe"`
	Email         *string    `json:"email,omitempty" example:"john.doe@example.com"`
	Phone         *string    `json:"phone,omitempty" example:"+1-555-0123"`
	Location      *string    `json:"location,omitempty" example:"San Francisco, CA"`
	Birthday      *time.Time `json:"birthday,omitempty" example:"1990-01-15T00:00:00Z"`
	HowMet        *string    `json:"how_met,omitempty" example:"Met at tech conference"`
	Cadence       *string    `json:"cadence,omitempty" example:"monthly" enums:"weekly,monthly,quarterly,biannual,annual"`
	LastContacted *time.Time `json:"last_contacted,omitempty" example:"2024-01-15T10:30:00Z"`
	ProfilePhoto  *string    `json:"profile_photo,omitempty" example:"https://example.com/photo.jpg"`
	CreatedAt     time.Time  `json:"created_at" example:"2024-01-01T00:00:00Z"`
	UpdatedAt     time.Time  `json:"updated_at" example:"2024-01-15T10:30:00Z"`
}

// CreateContactRequest represents the request to create a contact
// @Description Create contact request
type CreateContactRequest struct {
	FullName     string    `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Email        *string   `json:"email,omitempty" validate:"omitempty,email,max=255" example:"john.doe@example.com"`
	Phone        *string   `json:"phone,omitempty" validate:"omitempty,max=50" example:"+1-555-0123"`
	Location     *string   `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly `json:"birthday,omitempty" example:"1990-01-15"`
	HowMet       *string   `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string   `json:"cadence,omitempty" validate:"omitempty,oneof=weekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string   `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// UpdateContactRequest represents the request to update a contact
// @Description Update contact request
type UpdateContactRequest struct {
	FullName     string    `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Email        *string   `json:"email,omitempty" validate:"omitempty,email,max=255" example:"john.doe@example.com"`
	Phone        *string   `json:"phone,omitempty" validate:"omitempty,max=50" example:"+1-555-0123"`
	Location     *string   `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly `json:"birthday,omitempty" example:"1990-01-15"`
	HowMet       *string   `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string   `json:"cadence,omitempty" validate:"omitempty,oneof=weekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string   `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// ListContactsQuery represents query parameters for listing contacts
type ListContactsQuery struct {
	Page   int    `form:"page" validate:"omitempty,min=1" example:"1"`
	Limit  int    `form:"limit" validate:"omitempty,min=1,max=1000" example:"20"`
	Search string `form:"search" validate:"omitempty,max=255" example:"john"`
}

// Helper function to convert repository contact to response
func contactToResponse(contact *repository.Contact) ContactResponse {
	return ContactResponse{
		ID:            contact.ID.String(),
		FullName:      contact.FullName,
		Email:         contact.Email,
		Phone:         contact.Phone,
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

// Helper function to convert create request to repository request
func createRequestToRepo(req CreateContactRequest) repository.CreateContactRequest {
	var birthday *time.Time
	if req.Birthday != nil {
		birthday = req.Birthday.Time
	}
	
	return repository.CreateContactRequest{
		FullName:     req.FullName,
		Email:        req.Email,
		Phone:        req.Phone,
		Location:     req.Location,
		Birthday:     birthday,
		HowMet:       req.HowMet,
		Cadence:      req.Cadence,
		ProfilePhoto: req.ProfilePhoto,
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
		Email:        req.Email,
		Phone:        req.Phone,
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
// @Failure 409 {object} api.APIResponse{error=api.APIError} "Contact already exists"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts [post]
func (h *ContactHandler) CreateContact(c *gin.Context) {
	var req CreateContactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Check if email already exists
	if req.Email != nil {
		existing, err := h.contactRepo.GetContactByEmail(c.Request.Context(), *req.Email)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			api.SendInternalError(c, "Failed to check existing contact")
			return
		}
		if existing != nil {
			api.SendConflict(c, "Contact with this email already exists")
			return
		}
	}

	contact, err := h.contactRepo.CreateContact(c.Request.Context(), createRequestToRepo(req))
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

	contact, err := h.contactRepo.GetContact(c.Request.Context(), id)
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
// @Description Get a paginated list of contacts with optional search
// @Tags contacts
// @Produce json
// @Param page query int false "Page number" default(1) minimum(1)
// @Param limit query int false "Items per page" default(20) minimum(1) maximum(100)
// @Param search query string false "Search term (name or email)" maxlength(255)
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

	offset := (query.Page - 1) * query.Limit

	var contacts []repository.Contact
	var err error

	if query.Search != "" {
		// Search contacts
		contacts, err = h.contactRepo.SearchContacts(c.Request.Context(), repository.SearchContactsParams{
			Query:  query.Search,
			Limit:  int32(query.Limit),
			Offset: int32(offset),
		})
	} else {
		// List all contacts
		contacts, err = h.contactRepo.ListContacts(c.Request.Context(), repository.ListContactsParams{
			Limit:  int32(query.Limit),
			Offset: int32(offset),
		})
	}

	if err != nil {
		api.SendInternalError(c, "Failed to retrieve contacts")
		return
	}

	// Convert to response format
	responses := make([]ContactResponse, len(contacts))
	for i, contact := range contacts {
		responses[i] = contactToResponse(&contact)
	}

	// Get total count for pagination
	total, err := h.contactRepo.CountContacts(c.Request.Context())
	if err != nil {
		api.SendInternalError(c, "Failed to count contacts")
		return
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
// @Failure 409 {object} api.APIResponse{error=api.APIError} "Email already exists"
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

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Check if contact exists
	existing, err := h.contactRepo.GetContact(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to retrieve contact")
		return
	}

	if existing == nil {
		api.SendNotFound(c, "Contact")
		return
	}

	// Check email uniqueness if email is being changed
	if req.Email != nil && (existing.Email == nil || *req.Email != *existing.Email) {
		emailExists, err := h.contactRepo.GetContactByEmail(c.Request.Context(), *req.Email)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			api.SendInternalError(c, "Failed to check existing email")
			return
		}
		if emailExists != nil {
			api.SendConflict(c, "Contact with this email already exists")
			return
		}
	}

	contact, err := h.contactRepo.UpdateContact(c.Request.Context(), id, updateRequestToRepo(req))
	if err != nil {
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

	// Check if contact exists
	existing, err := h.contactRepo.GetContact(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to retrieve contact")
		return
	}

	if existing == nil {
		api.SendNotFound(c, "Contact")
		return
	}

	err = h.contactRepo.SoftDeleteContact(c.Request.Context(), id)
	if err != nil {
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

	// Check if contact exists
	existing, err := h.contactRepo.GetContact(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to retrieve contact")
		return
	}

	if existing == nil {
		api.SendNotFound(c, "Contact")
		return
	}

	err = h.contactRepo.UpdateContactLastContacted(c.Request.Context(), id, time.Now())
	if err != nil {
		api.SendInternalError(c, "Failed to update last contacted date")
		return
	}

	api.SendSuccess(c, http.StatusOK, gin.H{"message": "Last contacted date updated successfully"}, nil)
}
