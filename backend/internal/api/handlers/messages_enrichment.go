package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	enrichmentOpFillBody      = "fill_body"
	maxEnrichmentItems        = 500
	maxEnrichmentBodyBytes    = 8 << 20 // 8 MiB
	defaultMissingBodiesLimit = 500     // also the max, as the `max=500` tag on missingBodiesQuery.Limit
)

// MessagesEnrichmentService is the surface the enrichment handler needs.
type MessagesEnrichmentService interface {
	ListMissingBodies(ctx context.Context, after *string, limit int) (*service.MissingBodiesPage, error)
	FillBodies(ctx context.Context, items []service.FillBodyItem) (*service.FillBodiesResult, error)
}

// MessagesEnrichmentHandler serves the Mac daemon's re-scan of stored
// body-less messages (host-bearer authenticated; the answers use the standard
// success/data envelope like every other /api/v1/host route).
type MessagesEnrichmentHandler struct {
	svc MessagesEnrichmentService
}

func NewMessagesEnrichmentHandler(svc MessagesEnrichmentService) *MessagesEnrichmentHandler {
	return &MessagesEnrichmentHandler{svc: svc}
}

type missingBodiesQuery struct {
	After *string `form:"after"`
	Limit *int    `form:"limit" binding:"omitempty,min=1,max=500"`
}

// ListMissingBodies answers GET /api/v1/host/:id/messages/enrichment/missing-bodies.
func (h *MessagesEnrichmentHandler) ListMissingBodies(c *gin.Context) {
	var q missingBodiesQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		api.SendValidationError(c, "Invalid query parameters", err.Error())
		return
	}
	limit := defaultMissingBodiesLimit
	if q.Limit != nil {
		limit = *q.Limit
	}
	page, err := h.svc.ListMissingBodies(c.Request.Context(), q.After, limit)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, page, nil)
}

type enrichmentItem struct {
	Op   string `json:"op"`
	Guid string `json:"guid"`
	Body string `json:"body"`
}

type enrichmentRequest struct {
	Items []enrichmentItem `json:"items"`
}

// FillBodies answers POST /api/v1/host/:id/messages/enrichment. It validates
// the whole request before any write, so an invalid request writes nothing.
func (h *MessagesEnrichmentHandler) FillBodies(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxEnrichmentBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			api.SendError(c, http.StatusRequestEntityTooLarge, api.ErrCodeValidation,
				"Request body too large", fmt.Sprintf("body exceeds %d bytes", maxEnrichmentBodyBytes))
			return
		}
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	var req enrichmentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	if len(req.Items) > maxEnrichmentItems {
		api.SendValidationError(c, fmt.Sprintf("request exceeds maximum of %d items", maxEnrichmentItems),
			fmt.Sprintf("got %d items", len(req.Items)))
		return
	}
	items := make([]service.FillBodyItem, len(req.Items))
	for i, item := range req.Items {
		if item.Op != enrichmentOpFillBody {
			api.SendValidationError(c, "Unknown op", fmt.Sprintf("items[%d].op %q is not %q", i, item.Op, enrichmentOpFillBody))
			return
		}
		if item.Guid == "" || item.Body == "" {
			api.SendValidationError(c, "fill_body needs a guid and a non-empty body", fmt.Sprintf("items[%d]", i))
			return
		}
		items[i] = service.FillBodyItem{Guid: item.Guid, Body: item.Body}
	}
	result, err := h.svc.FillBodies(c.Request.Context(), items)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, result, nil)
}
