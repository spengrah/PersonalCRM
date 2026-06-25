package handlers

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// bindOptionalJSON binds a JSON body that may legitimately be empty. An
// empty body (io.EOF) is treated as the zero value (no methods → act on
// all), not an error. A present-but-malformed body returns the bind error.
func bindOptionalJSON(c *gin.Context, target any) error {
	if err := c.ShouldBindJSON(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// SuggestionHandler serves the composed People-tab suggestion surface:
// the method-suggestion group plus the existing confidence-ranked
// candidates, and the resolve/dismiss method actions. It is a THIN
// handler — parse/bind + error→HTTP mapping only; all logic lives in
// SuggestionService.
type SuggestionHandler struct {
	suggestionSvc *service.SuggestionService
}

// NewSuggestionHandler creates a new suggestion handler.
func NewSuggestionHandler(suggestionSvc *service.SuggestionService) *SuggestionHandler {
	return &SuggestionHandler{suggestionSvc: suggestionSvc}
}

func includeUnresolvedTelegramParam(c *gin.Context) bool {
	return c.Query("include_unresolved_telegram") == "true"
}

// MethodSuggestionResponse is the method-kind queue entry. Methods are the
// pending (type, normalized-value) pairs displayable for confirmation.
type MethodSuggestionResponse struct {
	ExternalContactID string                      `json:"external_contact_id"`
	ContactID         string                      `json:"contact_id"`
	ContactName       string                      `json:"contact_name"`
	Source            string                      `json:"source"`
	Methods           []MethodSuggestionMethodDTO `json:"methods"`
}

// MethodSuggestionMethodDTO is one (type, value) pending method.
type MethodSuggestionMethodDTO struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// CandidateSuggestionResponse wraps the existing import candidate with the
// server-declared allowed actions (link-only policy).
type CandidateSuggestionResponse struct {
	ImportCandidateResponse
	AllowedActions []string `json:"allowed_actions"`
}

// SuggestionItemResponse is the read-model union. Exactly one of Candidate
// or Suggestion is set, per Kind.
type SuggestionItemResponse struct {
	Kind       string                       `json:"kind"` // "contact" | "method"
	Candidate  *CandidateSuggestionResponse `json:"candidate,omitempty"`
	Suggestion *MethodSuggestionResponse    `json:"suggestion,omitempty"`
}

// ListSuggestions returns the composed suggestion list: the
// method-suggestion group (page 1 only) above the confidence-ranked
// candidates. The source chip filters BOTH groups; pagination meta
// reflects the CANDIDATE group only — the method group rides above the
// fold on page 1 and is excluded from the page math.
// @Summary List suggestions
// @Description Method-suggestion group + confidence-ranked import candidates as a SuggestionItem union
// @Tags imports
// @Produce json
// @Param source query string false "Source filter"
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} api.APIResponse{data=[]SuggestionItemResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /imports/suggestions [get]
func (h *SuggestionHandler) ListSuggestions(c *gin.Context) {
	ctx := c.Request.Context()

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > MaxCandidatesForSorting {
		limit = MaxCandidatesForSorting
	}

	list, err := h.suggestionSvc.ListSuggestions(ctx, service.SuggestionListParams{
		Source:                    c.Query("source"),
		Page:                      page,
		Limit:                     limit,
		IncludeUnresolvedTelegram: includeUnresolvedTelegramParam(c),
	}, MaxCandidatesForSorting)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	items := make([]SuggestionItemResponse, 0, len(list.Methods)+len(list.Candidates))
	for i := range list.Methods {
		items = append(items, SuggestionItemResponse{
			Kind:       "method",
			Suggestion: methodSuggestionToResponse(&list.Methods[i]),
		})
	}
	for i := range list.Candidates {
		cand := &list.Candidates[i]
		resp := buildImportCandidateResponse(&cand.External, cand.Match)
		items = append(items, SuggestionItemResponse{
			Kind: "contact",
			Candidate: &CandidateSuggestionResponse{
				ImportCandidateResponse: resp,
				AllowedActions:          service.AllowedActionsForSource(cand.External.Source),
			},
		})
	}

	api.SendSuccess(c, http.StatusOK, items, &api.Meta{
		HiddenUnresolvedTelegramCount: list.HiddenUnresolvedTelegramCount,
		Pagination: &api.PaginationMeta{
			Page:  list.Page,
			Limit: list.Limit,
			Total: int64(list.CandidateTotal),
			Pages: list.Pages,
		},
	})
}

// ResolveMethodSuggestionsRequest is the resolve body. Empty/omitted
// methods means confirm ALL live pending (the People-tab UI always sends
// an explicit list).
type ResolveMethodSuggestionsRequest struct {
	Methods []MethodSuggestionSelectionInput `json:"methods,omitempty"`
}

// DismissMethodSuggestionsRequest is the dismiss body. Empty/omitted
// methods means dismiss ALL actionable live pending.
type DismissMethodSuggestionsRequest struct {
	Methods []MethodSuggestionSelectionInput `json:"methods,omitempty"`
}

// MethodSuggestionSelectionInput is one (type,value) the user is acting on.
// Value is the normalized value as listed by GET /imports/suggestions.
type MethodSuggestionSelectionInput struct {
	Type  string `json:"type" binding:"required"`
	Value string `json:"value" binding:"required"`
}

// ResolveMethodSuggestionsResponse echoes the action outcome.
type ResolveMethodSuggestionsResponse struct {
	ExternalContactID string  `json:"external_contact_id"`
	ContactID         string  `json:"contact_id"`
	ResolvedCount     int     `json:"resolved_count"`
	RematchJobID      *string `json:"rematch_job_id,omitempty"`
}

// DismissMethodSuggestionsResponse echoes the dismiss outcome.
type DismissMethodSuggestionsResponse struct {
	ExternalContactID string `json:"external_contact_id"`
	DismissedCount    int    `json:"dismissed_count"`
}

// ResolveMethodSuggestions confirms selected pending methods for the
// already-linked contact, enriches it, and clears the confirmed entries
// from pending.
// @Summary Resolve method suggestions
// @Tags imports
// @Accept json
// @Produce json
// @Param id path string true "External contact ID"
// @Param body body ResolveMethodSuggestionsRequest false "Selected methods (empty = all)"
// @Success 200 {object} api.APIResponse{data=ResolveMethodSuggestionsResponse}
// @Router /imports/suggestions/{id}/methods/resolve [post]
func (h *SuggestionHandler) ResolveMethodSuggestions(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	var req ResolveMethodSuggestionsRequest
	if bindErr := bindOptionalJSON(c, &req); bindErr != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", bindErr.Error())
		return
	}

	result, err := h.suggestionSvc.ResolveMethodSuggestions(ctx, id, methodSelectionsToRepo(req.Methods))
	if err != nil {
		h.sendActionError(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, ResolveMethodSuggestionsResponse{
		ExternalContactID: id.String(),
		ContactID:         result.ContactID.String(),
		ResolvedCount:     result.Applied,
		RematchJobID:      nilStringPtrFromUUID(result.RematchJobID),
	}, nil)
}

// DismissMethodSuggestions records the selected pending methods as sticky
// dismissals and drops them from pending.
// @Summary Dismiss method suggestions
// @Tags imports
// @Accept json
// @Produce json
// @Param id path string true "External contact ID"
// @Param body body DismissMethodSuggestionsRequest false "Selected methods (empty = all)"
// @Success 200 {object} api.APIResponse{data=DismissMethodSuggestionsResponse}
// @Router /imports/suggestions/{id}/methods/dismiss [post]
func (h *SuggestionHandler) DismissMethodSuggestions(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid ID", err.Error())
		return
	}

	var req DismissMethodSuggestionsRequest
	if bindErr := bindOptionalJSON(c, &req); bindErr != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", bindErr.Error())
		return
	}

	result, err := h.suggestionSvc.DismissMethodSuggestions(ctx, id, methodSelectionsToRepo(req.Methods))
	if err != nil {
		h.sendActionError(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, DismissMethodSuggestionsResponse{
		ExternalContactID: id.String(),
		DismissedCount:    result.Dismissed,
	}, nil)
}

// sendActionError maps a resolve/dismiss service error to the right HTTP
// status. Unknown errors surface as 500.
func (h *SuggestionHandler) sendActionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Suggestion not found", "")
	case errors.Is(err, service.ErrSuggestionContactGone):
		api.SendError(c, http.StatusGone, api.ErrCodeValidation, "The linked contact no longer exists", "")
	case errors.Is(err, service.ErrSuggestionNotLinked):
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "This row is not linked to a contact", "")
	case errors.Is(err, service.ErrSuggestionInvalidMethod):
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Malformed method (empty type or value)", "")
	default:
		api.RespondInternal(c, err)
	}
}

// methodSuggestionToResponse maps the service item to the API DTO.
func methodSuggestionToResponse(item *service.MethodSuggestionItem) *MethodSuggestionResponse {
	methods := make([]MethodSuggestionMethodDTO, 0, len(item.Methods))
	for _, m := range item.Methods {
		methods = append(methods, MethodSuggestionMethodDTO{Type: m.Type, Value: m.Value})
	}
	return &MethodSuggestionResponse{
		ExternalContactID: item.ExternalID.String(),
		ContactID:         item.ContactID.String(),
		ContactName:       item.ContactName,
		Source:            item.Source,
		Methods:           methods,
	}
}

// methodSelectionsToRepo converts the request inputs to the repository
// suggestion type the service consumes.
func methodSelectionsToRepo(inputs []MethodSuggestionSelectionInput) []repository.PendingMethodSuggestion {
	if len(inputs) == 0 {
		return nil
	}
	out := make([]repository.PendingMethodSuggestion, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, repository.PendingMethodSuggestion{Type: in.Type, Value: in.Value})
	}
	return out
}
