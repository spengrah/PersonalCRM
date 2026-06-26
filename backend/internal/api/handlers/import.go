package handlers

import (
	"context"
	"net/http"
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
// PostImportHook is called after a Telegram candidate is imported/linked
// to back-fill message matching and trigger aggregation.
type PostImportHook interface {
	OnPeerLinked(ctx context.Context, peerUserID int64, peerUsername string, contactID uuid.UUID) error
}

// ImportHandler holds the dependencies the import endpoints need. It
// keeps a direct externalRepo reference because the existing import
// flow reads/writes external_contact across most methods (GetByID,
// UpdateMatch, Ignore) and a passthrough service wrapper would add
// indirection without semantic gain. The anarlog identity backfill is
// routed through identitySvc so handler code does not call the
// identity repository directly.
type ImportHandler struct {
	externalRepo   *repository.ExternalContactRepository
	identitySvc    *service.IdentityService
	contactSvc     *service.ContactService
	matchSvc       *service.ImportMatchService
	enricher       *service.EnrichmentService
	suggestionSvc  *service.SuggestionService
	validator      *validator.Validate
	postImportHook PostImportHook
}

// NewImportHandler creates a new import handler. identitySvc may be
// nil for callers that don't exercise the anarlog_humans backfill
// path; when nil the import flow silently skips the anarlog identity
// update (the meeting_note re-sync path then re-resolves as unmatched,
// which matches the no-backfill baseline behavior).
func NewImportHandler(
	externalRepo *repository.ExternalContactRepository,
	identitySvc *service.IdentityService,
	contactSvc *service.ContactService,
	matchSvc *service.ImportMatchService,
	enricher *service.EnrichmentService,
	suggestionSvc *service.SuggestionService,
) *ImportHandler {
	return &ImportHandler{
		externalRepo:  externalRepo,
		identitySvc:   identitySvc,
		contactSvc:    contactSvc,
		matchSvc:      matchSvc,
		enricher:      enricher,
		suggestionSvc: suggestionSvc,
		validator:     validator.New(),
	}
}

// SetPostImportHook sets an optional hook for post-import processing (e.g., Telegram back-linking).
func (h *ImportHandler) SetPostImportHook(hook PostImportHook) {
	h.postImportHook = hook
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
	Type          string `json:"type" binding:"required,oneof=email phone telegram signal discord twitter gchat whatsapp"`
	IsPrimary     bool   `json:"is_primary"`
}

// ImportRequest represents an optional request body for importing with method selection
type ImportRequest struct {
	SelectedMethods []SelectedMethodInput `json:"selected_methods,omitempty"`
	Cadence         *string               `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
	Name            *string               `json:"name,omitempty"`
}

// LinkRequest represents a request to link an external contact to a CRM contact
type LinkRequest struct {
	CRMContactID        string                `json:"crm_contact_id" binding:"required"`
	SelectedMethods     []SelectedMethodInput `json:"selected_methods,omitempty"`
	ConflictResolutions map[string]string     `json:"conflict_resolutions,omitempty"` // value -> "use_crm" | "use_external"
	Cadence             *string               `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
	Name                *string               `json:"name,omitempty"`
	// MethodsCurated is true when the candidate offered methods to select
	// (the modal rendered the method-selection UI) and the user made a
	// selection decision — even if SelectedMethods ends up empty (user
	// deselected all). It distinguishes an explicit empty selection from a
	// bare auto-match, since a nil slice and an explicit [] are
	// indistinguishable in Go after JSON unmarshal. The frontend sets it
	// ONLY when the candidate actually had methods to curate (a
	// zero-method candidate offered no curation choice → false → stays
	// matched).
	MethodsCurated bool `json:"methods_curated,omitempty"`
}

// ImportContactResponse wraps the created contact along with an optional rematch job ID.
type ImportContactResponse struct {
	Contact      ContactResponse `json:"contact"`
	RematchJobID *string         `json:"rematch_job_id,omitempty"`
}

// LinkContactResponse wraps the linked external contact along with an optional rematch job ID.
type LinkContactResponse struct {
	ExternalContact *repository.ExternalContact `json:"external_contact"`
	RematchJobID    *string                     `json:"rematch_job_id,omitempty"`
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
	if limit < 1 {
		limit = 20
	}
	// Allow up to MaxCandidatesForSorting since we fetch all for sorting anyway
	if limit > MaxCandidatesForSorting {
		limit = MaxCandidatesForSorting
	}

	// Check for source filter
	source := c.Query("source")
	includeUnresolvedTelegram := includeUnresolvedTelegramParam(c)

	// Build the confidence-sorted candidate list via the shared service
	// helper (the single sort implementation, also used by the suggestions
	// surface). We fetch all candidates up to MaxCandidatesForSorting
	// because confidence scores are computed in-memory and can't be sorted
	// at the DB level.
	sorted, err := h.suggestionSvc.BuildSortedCandidates(ctx, source, MaxCandidatesForSorting, includeUnresolvedTelegram)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	hiddenCount, err := h.externalRepo.CountHiddenUnresolvedTelegram(ctx, source)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Convert to response format, preserving the service's sort order.
	candidates := make([]ImportCandidateResponse, 0, len(sorted))
	for i := range sorted {
		candidates = append(candidates, h.toImportCandidateResponse(&sorted[i].External, sorted[i].Match))
	}

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

	api.SendSuccess(c, http.StatusOK, paginatedCandidates, &api.Meta{
		HiddenUnresolvedTelegramCount: hiddenCount,
		Pagination:                    api.BuildPaginationMeta(page, limit, total),
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

	id, ok := api.ParseUUIDParam(c, "id", "external contact")
	if !ok {
		return
	}

	contact, err := h.externalRepo.GetByID(ctx, id)
	if err != nil {
		api.RespondInternal(c, err)
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
// @Success 201 {object} api.APIResponse{data=ImportContactResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id}/import [post]
func (h *ImportHandler) ImportContact(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := api.ParseUUIDParam(c, "id", "external contact")
	if !ok {
		return
	}

	// Parse optional request body for method selection
	var req ImportRequest
	// Ignore binding errors - empty body is valid for backward compatibility
	_ = c.ShouldBindJSON(&req)

	// Validate the request (including cadence if provided)
	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	// Get external contact
	external, err := h.externalRepo.GetByID(ctx, id)
	if err != nil {
		api.RespondInternal(c, err)
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

	// Link-only policy (server-side teeth): a link-only source can never
	// create a NEW CRM contact. Hiding the Import button in the UI is not
	// enough — a crafted request must be rejected here.
	if service.IsLinkOnlySource(external.Source) {
		api.SendError(c, http.StatusForbidden, api.ErrCodeValidation, "This source cannot be imported as a new contact", external.Source)
		return
	}

	// Build contact creation request
	// Use custom name if provided, otherwise fall back to external source name
	fullName := ""
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		fullName = strings.TrimSpace(*req.Name)
	} else if external.DisplayName != nil {
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
		methods = service.BuildMethodsFromExternal(external)
	}

	// Build create request
	createReq := repository.CreateContactRequest{
		FullName:     fullName,
		Birthday:     external.Birthday,
		ProfilePhoto: external.PhotoURL,
		Cadence:      req.Cadence,
	}
	if len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		createReq.Location = &location
	}

	// Create the CRM contact
	contact, rematchJobID, err := h.contactSvc.CreateContact(ctx, createReq, methods)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	response := ImportContactResponse{
		Contact:      contactToResponse(contact),
		RematchJobID: nilStringPtrFromUUID(rematchJobID),
	}

	// Update external contact to link to new CRM contact
	if _, err := h.externalRepo.UpdateMatch(ctx, id, &contact.ID, repository.MatchStatusImported); err != nil {
		logger.Warn().Err(err).Str("external_id", id.String()).Msg("failed to update match status after import")
		api.SendSuccess(c, http.StatusCreated, response, nil)
		return
	}

	// Post-import hook: back-link Telegram message history
	h.triggerPostImportHook(ctx, external, contact.ID)

	// Anarlog-humans backfill: link the anarlog_human_id identity row
	// to the imported contact so a future meeting_note.recorded re-sync
	// resolves the human and produces the right interaction. Surfaces
	// errors as 500 because a silent failure would leave the user with
	// an imported contact whose anarlog sessions don't link to it.
	if err := h.backfillAnarlogIdentity(ctx, external, contact.ID); err != nil {
		logger.Error().Err(err).Str("external_id", id.String()).Msg("anarlog import backfill failed")
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusCreated, response, nil)
}

// buildMethodsFromSelection builds contact methods from user selection
func (h *ImportHandler) buildMethodsFromSelection(external *repository.ExternalContact, selected []SelectedMethodInput) []service.ContactMethodInput {
	// Build map of available values from external contact. Tracks the value
	// that should be persisted so telegram handles are stored without the
	// leading '@' even when the frontend sends '@daledobeck' as the
	// display-friendly original_value (matches buildMethodsAuto semantics).
	availableValues := make(map[string]string)
	for _, email := range external.Emails {
		availableValues[email.Value] = email.Value
	}
	for _, phone := range external.Phones {
		availableValues[phone.Value] = phone.Value
	}
	// Telegram handle from metadata — accept both the stored ('@daledobeck')
	// and bare ('daledobeck') forms as the original_value, but persist without
	// the '@' so it matches buildMethodsAuto and downstream contact-method
	// uniqueness checks.
	if external.Source == "telegram" {
		if handle, ok := external.Metadata["username"].(string); ok && handle != "" {
			bare := strings.TrimPrefix(handle, "@")
			availableValues[handle] = bare
			availableValues[bare] = bare
		}
	}

	methods := make([]service.ContactMethodInput, 0, len(selected))
	usedValues := make(map[string]bool)
	hasPrimary := false // Track if we've already assigned a primary

	for _, sel := range selected {
		// Validate the value exists in external contact, and look up the
		// canonical stored value (strips '@' for telegram handles).
		storedValue, ok := availableValues[sel.OriginalValue]
		if !ok {
			logger.Warn().Str("value", sel.OriginalValue).Msg("selected value not found in external contact")
			continue
		}

		normalized := repository.NormalizeContactMethodValue(sel.Type, storedValue)
		if normalized == "" {
			continue
		}
		key := sel.Type + ":" + normalized
		if usedValues[key] {
			logger.Warn().Str("value", sel.OriginalValue).Msg("duplicate method type+value in selection, skipping")
			continue
		}

		// Only allow one primary method - first one wins
		isPrimary := sel.IsPrimary && !hasPrimary
		if isPrimary {
			hasPrimary = true
		}

		methods = append(methods, service.ContactMethodInput{
			Type:      sel.Type,
			Value:     storedValue,
			IsPrimary: isPrimary,
		})
		usedValues[key] = true
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
// @Success 200 {object} api.APIResponse{data=LinkContactResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/{id}/link [post]
func (h *ImportHandler) LinkContact(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := api.ParseUUIDParam(c, "id", "external contact")
	if !ok {
		return
	}

	var req LinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	// Validate the request (including cadence if provided)
	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
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
		api.RespondInternal(c, err)
		return
	}
	if external == nil {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Candidate not found", "")
		return
	}

	// A link is "curated" when the request engaged any of the modal's
	// curation controls — method selections, conflict resolutions, an
	// explicit cadence, or a name override. This is the SAME signal used
	// below to pick the WithSelections enrichment path. A curated link
	// lands as `imported` so the address-book method reconcile treats a
	// missing method as a possible intentional deselection (suggest, not
	// auto-push); a bare link with no curation signal stays `matched`
	// (an un-applied method there is a genuine gap → auto-propagate).
	//
	// MethodsCurated covers the deselect-all case: the modal rendered the
	// method-selection UI and the user deselected everything, sending an
	// empty selection that is indistinguishable from a bare auto-match by
	// slice-emptiness alone. The explicit boolean makes that an `imported`
	// link, not a `matched` one.
	curated := req.MethodsCurated || len(req.SelectedMethods) > 0 || len(req.ConflictResolutions) > 0 || req.Cadence != nil || req.Name != nil
	linkStatus := repository.MatchStatusMatched
	if curated {
		linkStatus = repository.MatchStatusImported
	}

	// Update match status
	updated, err := h.externalRepo.UpdateMatch(ctx, id, &crmContactID, linkStatus)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Enrich the CRM contact - use method selections if provided
	var (
		enrichErr    error
		rematchJobID uuid.UUID
	)
	if curated {
		rematchJobID, enrichErr = h.enricher.EnrichContactFromExternalWithSelections(
			ctx,
			crmContactID,
			updated,
			toEnrichmentMethodSelections(req.SelectedMethods),
			req.ConflictResolutions,
			req.Cadence,
			req.Name,
		)
	} else {
		rematchJobID, enrichErr = h.enricher.EnrichContactFromExternal(ctx, crmContactID, updated)
	}

	if enrichErr != nil {
		// If there are contact method conflicts, return as user-facing error
		if strings.Contains(enrichErr.Error(), "contact method conflicts") {
			api.SendError(c, http.StatusConflict, api.ErrCodeValidation, "Cannot link: "+enrichErr.Error(), "")
			return
		}
		logger.Warn().Err(enrichErr).Str("external_id", id.String()).Msg("enrichment failed during link")
	}

	// Post-import hook: back-link Telegram message history
	h.triggerPostImportHook(ctx, external, crmContactID)

	// Anarlog-humans backfill: link the anarlog_human_id identity row
	// to the linked contact so a future meeting_note.recorded re-sync
	// resolves the human and produces the right interaction. Surfaces
	// errors as 500 — see ImportContact above for the rationale.
	if err := h.backfillAnarlogIdentity(ctx, external, crmContactID); err != nil {
		logger.Error().Err(err).Str("external_id", id.String()).Msg("anarlog import backfill failed")
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, LinkContactResponse{
		ExternalContact: updated,
		RematchJobID:    nilStringPtrFromUUID(rematchJobID),
	}, nil)
}

// triggerPostImportHook calls the post-import hook for Telegram candidates (best-effort).
func (h *ImportHandler) triggerPostImportHook(ctx context.Context, external *repository.ExternalContact, contactID uuid.UUID) {
	if h.postImportHook == nil || external.Source != "telegram" {
		return
	}
	peerUserID, err := strconv.ParseInt(external.SourceID, 10, 64)
	if err != nil {
		logger.Warn().Err(err).Str("source_id", external.SourceID).Msg("failed to parse telegram peer user ID for post-import hook")
		return
	}
	var peerUsername string
	if username, ok := external.Metadata["username"].(string); ok {
		peerUsername = strings.TrimPrefix(username, "@")
	}
	if err := h.postImportHook.OnPeerLinked(ctx, peerUserID, peerUsername, contactID); err != nil {
		logger.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: post-import hook failed")
	}
}

// backfillAnarlogIdentity routes through IdentityService so the handler
// stays free of direct repository calls. Returns nil + no-op when the
// external_contact is not from the anarlog_humans source, or when
// identitySvc is nil (test wiring).
func (h *ImportHandler) backfillAnarlogIdentity(ctx context.Context, external *repository.ExternalContact, contactID uuid.UUID) error {
	if h.identitySvc == nil || external == nil || external.Source != "anarlog_humans" {
		return nil
	}
	return h.identitySvc.BackfillAnarlogIdentityForImport(ctx, external.SourceID, contactID)
}

// toEnrichmentMethodSelections converts handler selections to service format
func toEnrichmentMethodSelections(selections []SelectedMethodInput) []service.MethodSelection {
	result := make([]service.MethodSelection, len(selections))
	for i, sel := range selections {
		result[i] = service.MethodSelection{
			OriginalValue: sel.OriginalValue,
			Type:          sel.Type,
			IsPrimary:     sel.IsPrimary,
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

	id, ok := api.ParseUUIDParam(c, "id", "external contact")
	if !ok {
		return
	}

	if err := h.externalRepo.Ignore(ctx, id); err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, "Contact ignored", nil)
}

// toImportCandidateResponse converts an external contact to the API response format
func (h *ImportHandler) toImportCandidateResponse(contact *repository.ExternalContact, suggestedMatch *service.ImportSuggestedMatch) ImportCandidateResponse {
	return buildImportCandidateResponse(contact, suggestedMatch)
}

// buildImportCandidateResponse converts an external contact + its suggested
// match into the candidate API response. Package-level so both the import
// handler and the suggestions handler build identical candidate payloads.
func buildImportCandidateResponse(contact *repository.ExternalContact, suggestedMatch *service.ImportSuggestedMatch) ImportCandidateResponse {
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

// getCandidateDisplayName extracts the display name from response fields for
// sorting. For Telegram candidates, falls back to metadata["username"] (with
// the stored leading "@" stripped) so handle-only peers sort alphabetically
// instead of being bunched at the end with an empty key.
func getCandidateDisplayName(displayName, firstName, lastName *string, metadata map[string]any, source string) string {
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
	if source == "telegram" {
		if u, ok := metadata["username"].(string); ok && u != "" {
			return strings.TrimPrefix(u, "@")
		}
	}
	return ""
}
