package handlers

import (
	"net/http"
	"strconv"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// ImportHandler handles import candidate HTTP requests
type ImportHandler struct {
	externalRepo *repository.ExternalContactRepository
	contactSvc   *service.ContactService
	enricher     *service.EnrichmentService
	validator    *validator.Validate
}

// NewImportHandler creates a new import handler
func NewImportHandler(
	externalRepo *repository.ExternalContactRepository,
	contactSvc *service.ContactService,
	enricher *service.EnrichmentService,
) *ImportHandler {
	return &ImportHandler{
		externalRepo: externalRepo,
		contactSvc:   contactSvc,
		enricher:     enricher,
		validator:    validator.New(),
	}
}

// ImportCandidateResponse represents an import candidate for the API
type ImportCandidateResponse struct {
	ID           string   `json:"id"`
	Source       string   `json:"source"`
	AccountID    *string  `json:"account_id,omitempty"`
	DisplayName  *string  `json:"display_name,omitempty"`
	FirstName    *string  `json:"first_name,omitempty"`
	LastName     *string  `json:"last_name,omitempty"`
	Organization *string  `json:"organization,omitempty"`
	JobTitle     *string  `json:"job_title,omitempty"`
	PhotoURL     *string  `json:"photo_url,omitempty"`
	Emails       []string `json:"emails"`
	Phones       []string `json:"phones"`
}

// LinkRequest represents a request to link an external contact to a CRM contact
type LinkRequest struct {
	CRMContactID string `json:"crm_contact_id" binding:"required"`
}

// ListImportCandidates returns unmatched external contacts
// @Summary List import candidates
// @Description Get unmatched external contacts that can be imported as CRM contacts
// @Tags imports
// @Produce json
// @Param source query string false "Source filter (e.g., gcontacts)"
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} api.APIResponse{data=[]ImportCandidateResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/candidates [get]
func (h *ImportHandler) ListImportCandidates(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := int32((page - 1) * limit)

	// Check for source filter
	source := c.Query("source")

	var contacts []repository.ExternalContact
	var total int64
	var err error

	if source != "" {
		contacts, err = h.externalRepo.ListUnmatched(ctx, source, int32(limit), offset)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to list candidates", err.Error())
			return
		}
		total, _ = h.externalRepo.CountUnmatched(ctx, source)
	} else {
		contacts, err = h.externalRepo.ListAllUnmatched(ctx, int32(limit), offset)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to list candidates", err.Error())
			return
		}
		total, _ = h.externalRepo.CountAllUnmatched(ctx)
	}

	// Convert to response format
	candidates := make([]ImportCandidateResponse, 0, len(contacts))
	for _, contact := range contacts {
		candidate := h.toImportCandidateResponse(&contact)
		candidates = append(candidates, candidate)
	}

	api.SendSuccess(c, http.StatusOK, candidates, &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
			Total: total,
		},
	})
}

// GetImportCandidate returns a specific import candidate
// @Summary Get import candidate
// @Description Get details of a specific import candidate
// @Tags imports
// @Produce json
// @Param id path string true "External contact ID"
// @Success 200 {object} api.APIResponse{data=repository.ExternalContact}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id} [get]
func (h *ImportHandler) GetImportCandidate(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	contact, err := h.externalRepo.GetByID(ctx, id)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get candidate", err.Error())
		return
	}
	if contact == nil {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Candidate not found", "")
		return
	}

	api.SendSuccess(c, http.StatusOK, contact, nil)
}

// ImportContact creates a CRM contact from an external contact
// @Summary Import contact
// @Description Create a new CRM contact from an external contact
// @Tags imports
// @Accept json
// @Produce json
// @Param id path string true "External contact ID"
// @Success 201 {object} api.APIResponse{data=repository.Contact}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id}/import [post]
func (h *ImportHandler) ImportContact(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	// Get external contact
	external, err := h.externalRepo.GetByID(ctx, id)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get candidate", err.Error())
		return
	}
	if external == nil {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Candidate not found", "")
		return
	}

	// Check if already imported/matched
	if external.MatchStatus != repository.MatchStatusUnmatched {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Contact already processed", string(external.MatchStatus))
		return
	}

	// Build contact creation request
	fullName := ""
	if external.DisplayName != nil {
		fullName = *external.DisplayName
	} else if external.FirstName != nil && external.LastName != nil {
		fullName = *external.FirstName + " " + *external.LastName
	} else if external.FirstName != nil {
		fullName = *external.FirstName
	}

	if fullName == "" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Cannot import contact without a name", "")
		return
	}

	// Build methods list
	methods := make([]service.ContactMethodInput, 0)

	// Handle emails - we can only store 2 emails (email_personal and email_work)
	// Strategy: Use Google's type hints if available, otherwise assign first as personal, second as work
	var hasPersonalEmail, hasWorkEmail bool
	for _, email := range external.Emails {
		// Determine the type based on Google's type hint
		emailType := "email_personal"
		if email.Type == "work" || email.Type == "other" {
			emailType = "email_work"
		}

		// Skip if we already have this type
		if emailType == "email_personal" && hasPersonalEmail {
			// If we don't have work email yet, assign this as work instead
			if !hasWorkEmail {
				emailType = "email_work"
				hasWorkEmail = true
			} else {
				continue // Skip - we already have both email types
			}
		} else if emailType == "email_work" && hasWorkEmail {
			// If we don't have personal email yet, assign this as personal instead
			if !hasPersonalEmail {
				emailType = "email_personal"
				hasPersonalEmail = true
			} else {
				continue // Skip - we already have both email types
			}
		}

		methods = append(methods, service.ContactMethodInput{
			Type:  emailType,
			Value: email.Value,
		})

		if emailType == "email_personal" {
			hasPersonalEmail = true
		} else {
			hasWorkEmail = true
		}
	}

	// Handle phones - we can only store 1 phone due to UNIQUE(contact_id, type) constraint
	// Take the first phone, preferring one marked as primary
	if len(external.Phones) > 0 {
		// Try to find primary phone first
		phoneToUse := external.Phones[0]
		for _, phone := range external.Phones {
			if phone.Primary {
				phoneToUse = phone
				break
			}
		}

		methods = append(methods, service.ContactMethodInput{
			Type:  "phone",
			Value: phoneToUse.Value,
		})
	}

	// Build create request
	createReq := repository.CreateContactRequest{
		FullName:     fullName,
		Birthday:     external.Birthday,
		ProfilePhoto: external.PhotoURL,
	}
	if len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		createReq.Location = &location
	}

	// Create the CRM contact
	contact, err := h.contactSvc.CreateContact(ctx, createReq, methods)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to create contact", err.Error())
		return
	}

	// Update external contact to link to new CRM contact
	if _, err := h.externalRepo.UpdateMatch(ctx, id, &contact.ID, repository.MatchStatusImported); err != nil {
		logger.Warn().Err(err).Str("external_id", id.String()).Msg("failed to update match status after import")
		api.SendSuccess(c, http.StatusCreated, contact, nil)
		return
	}

	api.SendSuccess(c, http.StatusCreated, contact, nil)
}

// LinkContact links an external contact to an existing CRM contact
// @Summary Link to existing contact
// @Description Link an external contact to an existing CRM contact and enrich it
// @Tags imports
// @Accept json
// @Produce json
// @Param id path string true "External contact ID"
// @Param body body LinkRequest true "Link request"
// @Success 200 {object} api.APIResponse{data=repository.ExternalContact}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id}/link [post]
func (h *ImportHandler) LinkContact(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	var req LinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	crmContactID, err := uuid.Parse(req.CRMContactID)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid CRM contact ID", err.Error())
		return
	}

	// Get external contact
	external, err := h.externalRepo.GetByID(ctx, id)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get candidate", err.Error())
		return
	}
	if external == nil {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Candidate not found", "")
		return
	}

	// Update match status
	updated, err := h.externalRepo.UpdateMatch(ctx, id, &crmContactID, repository.MatchStatusMatched)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to link contact", err.Error())
		return
	}

	// Enrich the CRM contact
	if err := h.enricher.EnrichContactFromExternal(ctx, crmContactID, updated); err != nil {
		logger.Warn().Err(err).Str("external_id", id.String()).Msg("enrichment failed during link")
	}

	api.SendSuccess(c, http.StatusOK, updated, nil)
}

// IgnoreContact marks an external contact as ignored
// @Summary Ignore contact
// @Description Mark an external contact as ignored (won't appear in candidates)
// @Tags imports
// @Produce json
// @Param id path string true "External contact ID"
// @Success 200 {object} api.APIResponse{data=string}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id}/ignore [post]
func (h *ImportHandler) IgnoreContact(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	if err := h.externalRepo.Ignore(ctx, id); err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to ignore contact", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, "Contact ignored", nil)
}

// toImportCandidateResponse converts an external contact to the API response format
func (h *ImportHandler) toImportCandidateResponse(contact *repository.ExternalContact) ImportCandidateResponse {
	response := ImportCandidateResponse{
		ID:           contact.ID.String(),
		Source:       contact.Source,
		AccountID:    contact.AccountID,
		DisplayName:  contact.DisplayName,
		FirstName:    contact.FirstName,
		LastName:     contact.LastName,
		Organization: contact.Organization,
		JobTitle:     contact.JobTitle,
		PhotoURL:     contact.PhotoURL,
		Emails:       make([]string, 0, len(contact.Emails)),
		Phones:       make([]string, 0, len(contact.Phones)),
	}

	for _, email := range contact.Emails {
		response.Emails = append(response.Emails, email.Value)
	}
	for _, phone := range contact.Phones {
		response.Phones = append(response.Phones, phone.Value)
	}

	return response
}
