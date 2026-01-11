package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// MaxCandidatesForSorting is the maximum number of candidates to fetch for sorting.
// We fetch all candidates (up to this limit) to enable global sorting by confidence
// score across all pages. This is necessary because confidence scores are calculated
// in-memory and cannot be sorted at the database level.
// This matches the limit used in the contacts list endpoint.
const MaxCandidatesForSorting = 10000

// ImportHandler handles import candidate HTTP requests
type ImportHandler struct {
	externalRepo *repository.ExternalContactRepository
	contactSvc   *service.ContactService
	matchSvc     *service.ImportMatchService
	enricher     *service.EnrichmentService
	validator    *validator.Validate
}

// NewImportHandler creates a new import handler
func NewImportHandler(
	externalRepo *repository.ExternalContactRepository,
	contactSvc *service.ContactService,
	matchSvc *service.ImportMatchService,
	enricher *service.EnrichmentService,
) *ImportHandler {
	return &ImportHandler{
		externalRepo: externalRepo,
		contactSvc:   contactSvc,
		matchSvc:     matchSvc,
		enricher:     enricher,
		validator:    validator.New(),
	}
}

// ImportCandidateResponse represents an import candidate for the API
type ImportCandidateResponse struct {
	ID             string          `json:"id"`
	Source         string          `json:"source"`
	AccountID      *string         `json:"account_id,omitempty"`
	DisplayName    *string         `json:"display_name,omitempty"`
	FirstName      *string         `json:"first_name,omitempty"`
	LastName       *string         `json:"last_name,omitempty"`
	Organization   *string         `json:"organization,omitempty"`
	JobTitle       *string         `json:"job_title,omitempty"`
	PhotoURL       *string         `json:"photo_url,omitempty"`
	Emails         []string        `json:"emails"`
	Phones         []string        `json:"phones"`
	SuggestedMatch *SuggestedMatch `json:"suggested_match,omitempty"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
}

// SuggestedMatch represents a suggested CRM contact match for an import candidate
type SuggestedMatch struct {
	ContactID   string  `json:"contact_id"`
	ContactName string  `json:"contact_name"`
	Confidence  float64 `json:"confidence"`
}

// SelectedMethodInput represents a user-selected contact method for import/link
type SelectedMethodInput struct {
	OriginalValue string `json:"original_value" binding:"required"`
	Type          string `json:"type" binding:"required,oneof=email_personal email_work phone telegram signal discord twitter gchat whatsapp"`
}

// ImportRequest represents an optional request body for importing with method selection
type ImportRequest struct {
	SelectedMethods []SelectedMethodInput `json:"selected_methods,omitempty"`
}

// LinkRequest represents a request to link an external contact to a CRM contact
type LinkRequest struct {
	CRMContactID        string                `json:"crm_contact_id" binding:"required"`
	SelectedMethods     []SelectedMethodInput `json:"selected_methods,omitempty"`
	ConflictResolutions map[string]string     `json:"conflict_resolutions,omitempty"` // value -> "use_crm" | "use_external"
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

	// Check for source filter
	source := c.Query("source")

	var contacts []repository.ExternalContact
	var err error

	// Fetch all candidates up to MaxCandidatesForSorting to enable global sorting
	// by confidence score across all pages. We can't use DB pagination here because
	// confidence scores are calculated in-memory via findBestMatch().
	if source != "" {
		contacts, err = h.externalRepo.ListUnmatched(ctx, source, MaxCandidatesForSorting, 0)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to list candidates", err.Error())
			return
		}
	} else {
		contacts, err = h.externalRepo.ListAllUnmatched(ctx, MaxCandidatesForSorting, 0)
		if err != nil {
			api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to list candidates", err.Error())
			return
		}
	}

	// Convert to response format with suggested matches
	candidates := make([]ImportCandidateResponse, 0, len(contacts))
	for _, contact := range contacts {
		// Find potential matching CRM contact
		suggestedMatch, err := h.matchSvc.FindBestMatch(ctx, &contact)
		if err != nil {
			logger.Warn().Err(err).Str("external_id", contact.ID.String()).Msg("failed to find suggested match")
		}
		candidate := h.toImportCandidateResponse(&contact, suggestedMatch)
		candidates = append(candidates, candidate)
	}

	// Sort candidates: by confidence descending, then alphabetically for those without matches
	sort.Slice(candidates, func(i, j int) bool {
		iMatch := candidates[i].SuggestedMatch
		jMatch := candidates[j].SuggestedMatch

		// Both have matches: sort by confidence descending
		if iMatch != nil && jMatch != nil {
			return iMatch.Confidence > jMatch.Confidence
		}

		// One has match: matched comes first
		if iMatch != nil {
			return true
		}
		if jMatch != nil {
			return false
		}

		// Neither has match: sort alphabetically by display name, empty names last
		iName := getCandidateDisplayName(candidates[i].DisplayName, candidates[i].FirstName, candidates[i].LastName)
		jName := getCandidateDisplayName(candidates[j].DisplayName, candidates[j].FirstName, candidates[j].LastName)

		// Empty names sort to end
		if iName == "" && jName != "" {
			return false
		}
		if iName != "" && jName == "" {
			return true
		}
		return iName < jName
	})

	// Apply pagination after sorting
	total := int64(len(candidates))
	offset := (page - 1) * limit
	end := offset + limit

	if offset > int(total) {
		offset = int(total)
	}
	if end > int(total) {
		end = int(total)
	}

	paginatedCandidates := candidates[offset:end]

	totalPages := int(total) / limit
	if int(total)%limit > 0 {
		totalPages++
	}

	api.SendSuccess(c, http.StatusOK, paginatedCandidates, &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
			Total: total,
			Pages: totalPages,
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
// @Param body body ImportRequest false "Optional method selection"
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

	// Parse optional request body for method selection
	var req ImportRequest
	// Ignore binding errors - empty body is valid for backward compatibility
	_ = c.ShouldBindJSON(&req)

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

	// Build methods list - use selected methods if provided, otherwise use auto-selection
	var methods []service.ContactMethodInput
	if len(req.SelectedMethods) > 0 {
		methods = h.buildMethodsFromSelection(external, req.SelectedMethods)
	} else {
		methods = h.buildMethodsAuto(external)
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

// buildMethodsFromSelection builds contact methods from user selection
func (h *ImportHandler) buildMethodsFromSelection(external *repository.ExternalContact, selected []SelectedMethodInput) []service.ContactMethodInput {
	// Build map of available values from external contact
	availableValues := make(map[string]bool)
	for _, email := range external.Emails {
		availableValues[email.Value] = true
	}
	for _, phone := range external.Phones {
		availableValues[phone.Value] = true
	}

	// Track used types to prevent duplicates
	usedTypes := make(map[string]bool)
	methods := make([]service.ContactMethodInput, 0, len(selected))

	for _, sel := range selected {
		// Validate the value exists in external contact
		if !availableValues[sel.OriginalValue] {
			logger.Warn().Str("value", sel.OriginalValue).Msg("selected value not found in external contact")
			continue
		}

		// Skip duplicate types
		if usedTypes[sel.Type] {
			logger.Warn().Str("type", sel.Type).Msg("duplicate type in selection, skipping")
			continue
		}

		methods = append(methods, service.ContactMethodInput{
			Type:  sel.Type,
			Value: sel.OriginalValue,
		})
		usedTypes[sel.Type] = true
	}

	return methods
}

// buildMethodsAuto builds contact methods using automatic selection (legacy behavior)
func (h *ImportHandler) buildMethodsAuto(external *repository.ExternalContact) []service.ContactMethodInput {
	methods := make([]service.ContactMethodInput, 0)

	// Handle emails - we can only store 2 emails (email_personal and email_work)
	// Strategy: Separate by type, then take first of each type
	var personalEmails, workEmails []string
	for _, email := range external.Emails {
		if email.Type == "work" || email.Type == "other" {
			workEmails = append(workEmails, email.Value)
		} else {
			personalEmails = append(personalEmails, email.Value)
		}
	}

	// Add first personal email if available
	if len(personalEmails) > 0 {
		methods = append(methods, service.ContactMethodInput{
			Type:  "email_personal",
			Value: personalEmails[0],
		})
	}

	// Add first work email if available
	if len(workEmails) > 0 {
		methods = append(methods, service.ContactMethodInput{
			Type:  "email_work",
			Value: workEmails[0],
		})
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

	return methods
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

	// Enrich the CRM contact - use method selections if provided
	var enrichErr error
	if len(req.SelectedMethods) > 0 || len(req.ConflictResolutions) > 0 {
		enrichErr = h.enricher.EnrichContactFromExternalWithSelections(
			ctx,
			crmContactID,
			updated,
			toEnrichmentMethodSelections(req.SelectedMethods),
			req.ConflictResolutions,
		)
	} else {
		enrichErr = h.enricher.EnrichContactFromExternal(ctx, crmContactID, updated)
	}

	if enrichErr != nil {
		// If there are contact method conflicts, return as user-facing error
		if strings.Contains(enrichErr.Error(), "contact method conflicts") {
			api.SendError(c, http.StatusConflict, api.ErrCodeValidation, "Cannot link: "+enrichErr.Error(), "")
			return
		}
		logger.Warn().Err(enrichErr).Str("external_id", id.String()).Msg("enrichment failed during link")
	}

	api.SendSuccess(c, http.StatusOK, updated, nil)
}

// toEnrichmentMethodSelections converts handler selections to service format
func toEnrichmentMethodSelections(selections []SelectedMethodInput) []service.MethodSelection {
	result := make([]service.MethodSelection, len(selections))
	for i, sel := range selections {
		result[i] = service.MethodSelection{
			OriginalValue: sel.OriginalValue,
			Type:          sel.Type,
		}
	}
	return result
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
func (h *ImportHandler) toImportCandidateResponse(contact *repository.ExternalContact, suggestedMatch *service.ImportSuggestedMatch) ImportCandidateResponse {
	var responseMatch *SuggestedMatch
	if suggestedMatch != nil {
		responseMatch = &SuggestedMatch{
			ContactID:   suggestedMatch.ContactID,
			ContactName: suggestedMatch.ContactName,
			Confidence:  suggestedMatch.Confidence,
		}
	}

	response := ImportCandidateResponse{
		ID:             contact.ID.String(),
		Source:         contact.Source,
		AccountID:      contact.AccountID,
		DisplayName:    contact.DisplayName,
		FirstName:      contact.FirstName,
		LastName:       contact.LastName,
		Organization:   contact.Organization,
		JobTitle:       contact.JobTitle,
		PhotoURL:       contact.PhotoURL,
		Emails:         make([]string, 0, len(contact.Emails)),
		Phones:         make([]string, 0, len(contact.Phones)),
		SuggestedMatch: responseMatch,
		Metadata:       contact.Metadata,
	}

	for _, email := range contact.Emails {
		response.Emails = append(response.Emails, email.Value)
	}
	for _, phone := range contact.Phones {
		response.Phones = append(response.Phones, phone.Value)
	}

	return response
}

// getCandidateDisplayName extracts the display name from response fields for sorting
func getCandidateDisplayName(displayName, firstName, lastName *string) string {
	if displayName != nil {
		return *displayName
	}
	if firstName != nil && lastName != nil {
		return *firstName + " " + *lastName
	}
	if firstName != nil {
		return *firstName
	}
	if lastName != nil {
		return *lastName
	}
	return ""
}
