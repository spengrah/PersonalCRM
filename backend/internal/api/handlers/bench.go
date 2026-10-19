package handlers

import (
	"math"
	"net/http"
	"slices"
	"strings"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AllInteractionsHandler serves the Interactions tab's list: every interaction
// across contacts, with no judgment.
type AllInteractionsHandler struct {
	content *service.InteractionContentService
}

// NewAllInteractionsHandler creates the all-interactions list's handler.
func NewAllInteractionsHandler(content *service.InteractionContentService) *AllInteractionsHandler {
	return &AllInteractionsHandler{content: content}
}

// RegisterAllInteractionsRoutes wires the all-interactions list onto a group
// whose middleware already enforces the global API key:
//
//   - GET /api/v1/interactions
func RegisterAllInteractionsRoutes(v1 *gin.RouterGroup, handler *AllInteractionsHandler) {
	v1.GET("/interactions", handler.ListInteractions)
}

// ListInteractions returns one page of every interaction across contacts.
// @Summary List all interactions
// @Description Every interaction across contacts, newest first: one row per interaction, naming each participant contact. Filters combine. A row carries no judgment, proposal or review status.
// @Tags interactions
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Param contact_id query string false "Participant contact ID" format(uuid)
// @Param type query string false "Interaction type" Enums(message, email, event, call, other)
// @Success 200 {object} api.APIResponse{data=AllInteractionsResponse}
// @Failure 400 {object} api.APIResponse
// @Router /interactions [get]
func (h *AllInteractionsHandler) ListInteractions(c *gin.Context) {
	page64, limit64, valid := parseInteractionPagination(c)
	if !valid {
		return
	}
	limit64 = min(limit64, 100)
	offset64 := (page64 - 1) * limit64
	if offset64 > math.MaxInt32 {
		api.SendValidationError(c, "Invalid pagination", "page offset is too large")
		return
	}
	filter := service.InteractionsFilter{Limit: int32(limit64), Offset: int32(offset64)}
	if raw, ok := c.GetQuery("contact_id"); ok {
		id, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid contact_id", "contact_id must be a UUID")
			return
		}
		filter.ContactID = &id
	}
	if raw, ok := c.GetQuery("type"); ok {
		keys := make([]string, len(service.InteractionTypes))
		for i, t := range service.InteractionTypes {
			keys[i] = string(t.Key)
		}
		if !slices.Contains(keys, raw) {
			api.SendValidationError(c, "Invalid type", "type must be one of "+strings.Join(keys, ", "))
			return
		}
		typ := service.InteractionType(raw)
		filter.Type = &typ
	}

	idx, err := h.content.IndexInteractions(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	page, err := h.content.ListInteractions(c.Request.Context(), idx, filter)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	items := make([]AllInteractionItemResponse, len(page.Rows))
	for i, row := range page.Rows {
		items[i] = allInteractionItemResponse(row)
	}
	options := make([]InteractionTypeOptionResponse, len(service.InteractionTypes))
	for i, t := range service.InteractionTypes {
		options[i] = InteractionTypeOptionResponse{Key: string(t.Key), Label: t.Label}
	}
	api.SendSuccess(c, http.StatusOK, AllInteractionsResponse{Items: items, TypeOptions: options},
		&api.Meta{Pagination: api.BuildPaginationMeta(int(page64), int(limit64), int64(page.Total))})
}

func allInteractionItemResponse(row service.InteractionRow) AllInteractionItemResponse {
	participants := make([]InteractionParticipantResponse, len(row.Participants))
	for i, p := range row.Participants {
		participants[i] = InteractionParticipantResponse{ContactID: p.ContactID.String(), Name: p.Name}
	}
	out := AllInteractionItemResponse{
		Item:             interactionListItemResponse(row.TimelineItem),
		ContextContactID: row.Participants[0].ContactID.String(),
		Participants:     participants,
	}
	if row.Preview != nil {
		out.Preview = &InteractionPreviewResponse{Text: row.Preview.Text, Note: row.Preview.Note}
		if !row.Preview.Note {
			out.Preview.Sender = &row.Preview.Sender
		}
	}
	return out
}
