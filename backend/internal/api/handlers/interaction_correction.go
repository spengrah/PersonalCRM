package handlers

import (
	"context"
	"errors"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InteractionCorrectionHandler serves the correction routes: the only HTTP
// way to merge interactions or remove participations (IXN-016, IXN-017,
// IXN-018, IXN-029). No route returns an assertion, predicate or provenance.
type InteractionCorrectionHandler struct {
	corrections *service.InteractionCorrectionService
}

// NewInteractionCorrectionHandler creates the correction routes' handler.
func NewInteractionCorrectionHandler(corrections *service.InteractionCorrectionService) *InteractionCorrectionHandler {
	return &InteractionCorrectionHandler{corrections: corrections}
}

// MergeInteractions declares the listed interactions one interaction
// @Summary Merge interactions
// @Description Declares the listed interactions one interaction for every attendee. Answers with contact_id's resulting participation.
// @Tags interactions
// @Accept json
// @Produce json
// @Param request body InteractionMergeRequest true "Interactions to merge"
// @Success 200 {object} api.APIResponse{data=InteractionMergeResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /interactions/merge [post]
func (h *InteractionCorrectionHandler) MergeInteractions(c *gin.Context) {
	h.merge(c, h.corrections.Merge)
}

// PreviewMergeInteractions answers what the merge would answer
// @Summary Preview an interaction merge
// @Description Answers exactly what POST /interactions/merge would answer for the same body, and writes nothing.
// @Tags interactions
// @Accept json
// @Produce json
// @Param request body InteractionMergeRequest true "Interactions to merge"
// @Success 200 {object} api.APIResponse{data=InteractionMergeResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /interactions/merge/preview [post]
func (h *InteractionCorrectionHandler) PreviewMergeInteractions(c *gin.Context) {
	h.merge(c, h.corrections.PreviewMerge)
}

func (h *InteractionCorrectionHandler) merge(
	c *gin.Context,
	run func(ctx context.Context, req service.InteractionMergeRequest) (*service.MergedParticipation, error),
) {
	var req InteractionMergeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", "interaction_ids and contact_id are required")
		return
	}
	contactID, err := uuid.Parse(req.ContactID)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}
	ids := make([]uuid.UUID, len(req.InteractionIDs))
	for i, raw := range req.InteractionIDs {
		if ids[i], err = uuid.Parse(raw); err != nil {
			api.SendValidationError(c, "Invalid interaction ID", "ID must be a valid UUID")
			return
		}
	}
	result, err := run(c.Request.Context(), service.InteractionMergeRequest{InteractionIDs: ids, ContactID: contactID})
	if err != nil {
		respondCorrectionError(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, InteractionMergeResponse{
		InteractionID: result.InteractionID.String(),
		Participation: InteractionParticipationResponse{Direction: result.Direction, OccurredAt: result.OccurredAt},
	}, nil)
}

// RemoveParticipations removes participations of an interaction
// @Summary Remove participations of an interaction
// @Description Removes exactly the selected attendees' participations (scope contact_ids, or everyone) of the interaction that holds the id, whatever the reason.
// @Tags interactions
// @Accept json
// @Param id path string true "Interaction ID" format(uuid)
// @Param request body InteractionRemovalRequest true "Removal scope and reason"
// @Success 204 "Participations removed"
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /interactions/{id}/removals [post]
func (h *InteractionCorrectionHandler) RemoveParticipations(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "interaction")
	if !ok {
		return
	}
	var req InteractionRemovalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", "scope and reason are required")
		return
	}
	if req.Scope.Everyone != nil && !*req.Scope.Everyone {
		api.SendValidationError(c, "Invalid scope", "everyone must be true when given")
		return
	}
	// A nil contactIDs means the body gave no contact list; a given list, even
	// an empty one, is non-nil, so the service refuses it beside everyone.
	var contactIDs []uuid.UUID
	if req.Scope.ContactIDs != nil {
		contactIDs = make([]uuid.UUID, len(*req.Scope.ContactIDs))
		for i, raw := range *req.Scope.ContactIDs {
			var err error
			if contactIDs[i], err = uuid.Parse(raw); err != nil {
				api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
				return
			}
		}
	}
	if err := h.corrections.Remove(c.Request.Context(), service.InteractionRemovalRequest{
		InteractionID: id,
		ContactIDs:    contactIDs,
		Everyone:      req.Scope.Everyone != nil,
		Reason:        req.Reason,
	}); err != nil {
		respondCorrectionError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// respondCorrectionError maps a correction's refusal: a request naming no
// applicable interaction or contact answers 400, an id naming no interaction
// node or live contact answers 404.
func respondCorrectionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrCorrectionRefused):
		api.SendValidationError(c, "Correction refused", err.Error())
	case errors.Is(err, service.ErrInteractionNotFound):
		api.SendNotFound(c, "Interaction")
	case errors.Is(err, db.ErrNotFound):
		api.SendNotFound(c, "Contact")
	default:
		api.RespondInternal(c, err)
	}
}
