package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	maxEnrichmentItems     = 500
	maxEnrichmentBodyBytes = 8 << 20 // 8 MiB
	defaultListLimit       = 500     // also the max, as the `max=500` tag on listQuery.Limit

	directionInbound  = "inbound"
	directionOutbound = "outbound"
)

// MessagesEnrichmentService is the surface the enrichment handler needs.
type MessagesEnrichmentService interface {
	ListMissingBodies(ctx context.Context, after *string, limit int) (*service.MissingBodiesPage, error)
	ListGroupChats(ctx context.Context, after *string, limit int) (*service.GroupChatsPage, error)
	ApplyItems(ctx context.Context, hostID uuid.UUID, items []service.EnrichmentItem) (*service.EnrichmentResponse, error)
}

// MessagesEnrichmentHandler serves the Mac daemon's re-scans of stored
// messages (host-bearer authenticated; the answers use the standard
// success/data envelope like every other /api/v1/host route).
type MessagesEnrichmentHandler struct {
	svc MessagesEnrichmentService
}

func NewMessagesEnrichmentHandler(svc MessagesEnrichmentService) *MessagesEnrichmentHandler {
	return &MessagesEnrichmentHandler{svc: svc}
}

// listQuery is the cursor/limit pair both enrichment listings take.
type listQuery struct {
	After *string `form:"after"`
	Limit *int    `form:"limit" binding:"omitempty,min=1,max=500"`
}

// listPage binds the cursor/limit query and answers with the page list returns.
func listPage[P any](c *gin.Context, list func(ctx context.Context, after *string, limit int) (P, error)) {
	var q listQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		api.SendValidationError(c, "Invalid query parameters", err.Error())
		return
	}
	limit := defaultListLimit
	if q.Limit != nil {
		limit = *q.Limit
	}
	page, err := list(c.Request.Context(), q.After, limit)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, page, nil)
}

// ListMissingBodies answers GET /api/v1/host/:id/messages/enrichment/missing-bodies.
func (h *MessagesEnrichmentHandler) ListMissingBodies(c *gin.Context) {
	listPage(c, h.svc.ListMissingBodies)
}

// ListGroupChats answers GET /api/v1/host/:id/messages/enrichment/group-chats.
func (h *MessagesEnrichmentHandler) ListGroupChats(c *gin.Context) {
	listPage(c, h.svc.ListGroupChats)
}

type enrichmentItem struct {
	Op        string  `json:"op"`
	Guid      string  `json:"guid"`
	Body      *string `json:"body"`
	ChatGuid  string  `json:"chat_guid"`
	Sender    *string `json:"sender"`
	Direction string  `json:"direction"`
	SentAt    string  `json:"sent_at"`
}

type enrichmentRequest struct {
	Items []enrichmentItem `json:"items"`
}

// toService validates one request item and converts it. The error names the
// first malformed field.
func (item enrichmentItem) toService() (service.EnrichmentItem, error) {
	if item.Guid == "" {
		return service.EnrichmentItem{}, errors.New("guid is required")
	}
	switch service.EnrichmentOp(item.Op) {
	case service.EnrichmentOpFillBody:
		if item.Body == nil || *item.Body == "" {
			return service.EnrichmentItem{}, errors.New("fill_body needs a non-empty body")
		}
		return service.EnrichmentItem{Op: service.EnrichmentOpFillBody, Guid: item.Guid, Body: item.Body}, nil
	case service.EnrichmentOpClearOutboundPeer:
		return service.EnrichmentItem{Op: service.EnrichmentOpClearOutboundPeer, Guid: item.Guid}, nil
	case service.EnrichmentOpAddGroupMessage:
		return item.addGroupMessage()
	default:
		return service.EnrichmentItem{}, fmt.Errorf("op %q is not one of fill_body, add_group_message, clear_outbound_peer", item.Op)
	}
}

func (item enrichmentItem) addGroupMessage() (service.EnrichmentItem, error) {
	if item.ChatGuid == "" {
		return service.EnrichmentItem{}, errors.New("add_group_message needs a chat_guid")
	}
	sentAt, err := time.Parse(time.RFC3339Nano, item.SentAt)
	if err != nil {
		return service.EnrichmentItem{}, errors.New("add_group_message needs an RFC 3339 sent_at")
	}
	if item.Body != nil && *item.Body == "" {
		return service.EnrichmentItem{}, errors.New("add_group_message body must be null or non-empty")
	}
	out := service.EnrichmentItem{
		Op: service.EnrichmentOpAddGroupMessage, Guid: item.Guid, Body: item.Body,
		ChatGuid: item.ChatGuid, SentAt: sentAt,
	}
	switch item.Direction {
	case directionInbound:
		if item.Sender == nil || *item.Sender == "" {
			return service.EnrichmentItem{}, errors.New("an inbound add_group_message needs a non-empty sender")
		}
		out.Sender = item.Sender
	case directionOutbound:
		if item.Sender != nil {
			return service.EnrichmentItem{}, errors.New("an outbound add_group_message needs a null sender")
		}
		out.Outgoing = true
	default:
		return service.EnrichmentItem{}, fmt.Errorf("direction %q is not inbound or outbound", item.Direction)
	}
	return out, nil
}

// ApplyItems answers POST /api/v1/host/:id/messages/enrichment. It validates
// the whole request before any write, so an invalid request writes nothing.
func (h *MessagesEnrichmentHandler) ApplyItems(c *gin.Context) {
	hostID, ok := auth.MacHostIDFromContext(c)
	if !ok {
		api.SendError(c, http.StatusUnauthorized, api.ErrCodeUnauthorized, "Unauthorized", "no authenticated host")
		return
	}
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
	items := make([]service.EnrichmentItem, len(req.Items))
	for i, item := range req.Items {
		converted, err := item.toService()
		if err != nil {
			api.SendValidationError(c, "Invalid enrichment item", fmt.Sprintf("items[%d]: %s", i, err))
			return
		}
		items[i] = converted
	}
	result, err := h.svc.ApplyItems(c.Request.Context(), hostID, items)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, result, nil)
}
