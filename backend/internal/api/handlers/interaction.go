package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// interactionRecorder defines the method for recording interactions
type interactionRecorder interface {
	RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error)
}

// InteractionHandler handles interaction-related HTTP requests
type InteractionHandler struct {
	recorder        interactionRecorder
	interactionRepo *repository.InteractionRepository
}

// NewInteractionHandler creates a new interaction handler
func NewInteractionHandler(recorder interactionRecorder, interactionRepo *repository.InteractionRepository) *InteractionHandler {
	return &InteractionHandler{
		recorder:        recorder,
		interactionRepo: interactionRepo,
	}
}

// InteractionResponse represents an interaction in API responses
type InteractionResponse struct {
	ID          string    `json:"id"`
	ContactID   string    `json:"contact_id"`
	Source      string    `json:"source"`
	SourceRef   *string   `json:"source_ref,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	Description *string   `json:"description,omitempty"`
	Direction   string    `json:"direction"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateInteractionRequest represents the request to create an interaction
type CreateInteractionRequest struct {
	OccurredAt  *string `json:"occurred_at,omitempty"`
	Description *string `json:"description,omitempty"`
	Direction   *string `json:"direction,omitempty"`
}

func interactionToResponse(i *repository.Interaction) InteractionResponse {
	return InteractionResponse{
		ID:          i.ID.String(),
		ContactID:   i.ContactID.String(),
		Source:      i.Source,
		SourceRef:   i.SourceRef,
		OccurredAt:  i.OccurredAt,
		Description: i.Description,
		Direction:   i.Direction,
		CreatedAt:   i.CreatedAt,
	}
}

// ListContactInteractions retrieves interactions for a contact
// @Summary List contact interactions
// @Tags interactions
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} api.APIResponse{data=[]InteractionResponse}
// @Router /contacts/{id}/interactions [get]
func (h *InteractionHandler) ListContactInteractions(c *gin.Context) {
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	interactions, err := h.interactionRepo.ListContactInteractions(c.Request.Context(), contactID, int32(limit), int32(offset))
	if err != nil {
		api.SendInternalError(c, "Failed to list interactions")
		return
	}

	total, err := h.interactionRepo.CountContactInteractions(c.Request.Context(), contactID)
	if err != nil {
		api.SendInternalError(c, "Failed to count interactions")
		return
	}

	responses := make([]InteractionResponse, len(interactions))
	for i, interaction := range interactions {
		responses[i] = interactionToResponse(&interaction)
	}

	totalPages := int(total) / limit
	if int(total)%limit > 0 {
		totalPages++
	}

	meta := &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
			Total: total,
			Pages: totalPages,
		},
	}

	api.SendSuccess(c, http.StatusOK, responses, meta)
}

// CreateInteraction creates a manual interaction for a contact
// @Summary Create interaction
// @Tags interactions
// @Accept json
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Param request body CreateInteractionRequest false "Interaction details"
// @Success 201 {object} api.APIResponse{data=InteractionResponse}
// @Router /contacts/{id}/interactions [post]
func (h *InteractionHandler) CreateInteraction(c *gin.Context) {
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	var req CreateInteractionRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			api.SendValidationError(c, "Invalid request body", err.Error())
			return
		}
	}

	occurredAt := accelerated.GetCurrentTime()
	if req.OccurredAt != nil {
		parsed, err := time.Parse(time.RFC3339, *req.OccurredAt)
		if err != nil {
			// Try date-only format
			parsed, err = time.Parse("2006-01-02", *req.OccurredAt)
			if err != nil {
				api.SendValidationError(c, "Invalid date format", "Use RFC3339 or YYYY-MM-DD format")
				return
			}
		}

		now := accelerated.GetCurrentTime()
		if parsed.After(now) {
			api.SendValidationError(c, "Invalid date", "Interaction date cannot be in the future")
			return
		}
		occurredAt = parsed
	}

	var direction string
	if req.Direction != nil {
		switch *req.Direction {
		case "outbound", "inbound", "mutual":
			direction = *req.Direction
		default:
			api.SendValidationError(c, "Invalid direction", "must be one of: outbound, inbound, mutual")
			return
		}
	}

	interaction, err := h.recorder.RecordInteraction(c.Request.Context(), repository.RecordInteractionRequest{
		ContactID:   contactID,
		Source:      repository.InteractionSourceManual,
		OccurredAt:  occurredAt,
		Description: req.Description,
		Direction:   direction,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to create interaction")
		return
	}

	api.SendSuccess(c, http.StatusCreated, interactionToResponse(interaction), nil)
}

// DeleteInteraction soft-deletes an interaction
// @Summary Delete interaction
// @Tags interactions
// @Param id path string true "Interaction ID" format(uuid)
// @Success 204 "Interaction deleted"
// @Router /interactions/{id} [delete]
func (h *InteractionHandler) DeleteInteraction(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid interaction ID", "ID must be a valid UUID")
		return
	}

	// Verify interaction exists
	_, err = h.interactionRepo.GetInteraction(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Interaction")
			return
		}
		api.SendInternalError(c, "Failed to retrieve interaction")
		return
	}

	if err := h.interactionRepo.SoftDeleteInteraction(c.Request.Context(), id); err != nil {
		api.SendInternalError(c, "Failed to delete interaction")
		return
	}

	c.Status(http.StatusNoContent)
}
