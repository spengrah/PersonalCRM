package handlers

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// InteractionHandler handles interaction-related HTTP requests.
//
// Post-PR-6 (cutover): all writes go through manualHandler.Run, which
// opens a tx, publishes interaction.manual, invokes the consumer inline,
// and commits. When manualHandler is nil (mode=off/shadow post-cutover),
// POST returns 503 Service Unavailable.
type InteractionHandler struct {
	interactionRepo *repository.InteractionRepository
	manualHandler   *service.ManualInteractionHandler
	content         *service.InteractionContentService
}

// NewInteractionHandler creates a new interaction handler. manualHandler
// may be nil; POST /contacts/:id/interactions returns 503 in that case.
func NewInteractionHandler(
	interactionRepo *repository.InteractionRepository,
	manualHandler *service.ManualInteractionHandler,
	content *service.InteractionContentService,
) *InteractionHandler {
	return &InteractionHandler{
		interactionRepo: interactionRepo,
		manualHandler:   manualHandler,
		content:         content,
	}
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
// @Param venue query string false "Venue key"
// @Param from query string false "Occurred-at lower bound" format(date-time)
// @Param to query string false "Occurred-at upper bound" format(date-time)
// @Success 200 {object} api.APIResponse{data=InteractionListResponse}
// @Router /contacts/{id}/interactions [get]
func (h *InteractionHandler) ListContactInteractions(c *gin.Context) {
	contactID, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	page64, limit64, valid := parseInteractionPagination(c)
	if !valid {
		return
	}
	if limit64 > 100 {
		limit64 = 100
	}
	if page64 > math.MaxInt32 {
		api.SendValidationError(c, "Invalid pagination", "page is too large")
		return
	}
	offset64 := (page64 - 1) * limit64
	if offset64 > math.MaxInt32 {
		api.SendValidationError(c, "Invalid pagination", "page offset is too large")
		return
	}

	params, valid := interactionListParams(c, contactID, int32(limit64), int32(offset64))
	if !valid {
		return
	}
	ctx := c.Request.Context()
	interactions, err := h.interactionRepo.ListContactInteractionsFiltered(ctx, params)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	total, err := h.interactionRepo.CountContactInteractionsFiltered(ctx, params)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	enrichment, err := h.content.EnrichPage(ctx, interactions)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	venues, err := h.content.ListContactVenues(ctx, contactID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	items := make([]InteractionListItemResponse, len(interactions))
	for i, interaction := range interactions {
		enriched, exists := enrichment[interaction.ID]
		if !exists {
			api.RespondInternal(c, errors.New("interaction enrichment missing"))
			return
		}
		items[i] = interactionListItemResponse(interaction, enriched)
	}
	response := InteractionListResponse{Items: items, VenueOptions: venueTagResponses(venues)}

	api.SendSuccess(c, http.StatusOK, response, &api.Meta{Pagination: api.BuildPaginationMeta(int(page64), int(limit64), total)})
}

var interactionVenuePattern = regexp.MustCompile(`^[a-z_]+:.+$`)

func parseInteractionPagination(c *gin.Context) (int64, int64, bool) {
	page64, limit64 := int64(1), int64(20)
	for name, target := range map[string]*int64{"page": &page64, "limit": &limit64} {
		if value, exists := c.GetQuery(name); exists {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 {
				api.SendValidationError(c, "Invalid pagination", name+" must be a positive integer")
				return 0, 0, false
			}
			*target = parsed
		}
	}
	return page64, limit64, true
}

func interactionListParams(c *gin.Context, contactID uuid.UUID, limit, offset int32) (repository.InteractionListFilterParams, bool) {
	params := repository.InteractionListFilterParams{ContactID: contactID, Limit: limit, Offset: offset}
	if venue, exists := c.GetQuery("venue"); exists {
		if !interactionVenuePattern.MatchString(venue) {
			api.SendValidationError(c, "Invalid venue", "venue must be source:container")
			return params, false
		}
		separator := strings.IndexByte(venue, ':')
		source, container := venue[:separator], venue[separator+1:]
		params.VenueSource, params.VenueContainer = &source, &container
	}
	for name, target := range map[string]**time.Time{"from": &params.From, "to": &params.To} {
		if value, exists := c.GetQuery(name); exists {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				api.SendValidationError(c, "Invalid time range", name+" must be RFC3339")
				return params, false
			}
			*target = &parsed
		}
	}
	return params, true
}

func interactionListItemResponse(interaction repository.Interaction, enriched service.InteractionEnrichment) InteractionListItemResponse {
	return InteractionListItemResponse{
		ID: interaction.ID.String(), ContactID: interaction.ContactID.String(), Source: interaction.Source,
		SourceRef: interaction.SourceRef, OccurredAt: interaction.OccurredAt, Description: interaction.Description,
		Direction: interaction.Direction, CreatedAt: interaction.CreatedAt, Label: enriched.Label,
		ContentKind: enriched.ContentKind, MessageCount: enriched.MessageCount, IsGroup: enriched.IsGroup,
		VenueTags: venueTagResponses(enriched.VenueTags), Event: eventSummaryResponse(enriched.Event), Call: callSummaryResponse(enriched.Call),
	}
}

func venueTagResponses(tags []service.VenueTag) []VenueTagResponse {
	result := make([]VenueTagResponse, 0, len(tags))
	for _, tag := range tags {
		result = append(result, VenueTagResponse{Key: tag.Key, Label: tag.Label, Kind: tag.Kind, IsGroup: tag.IsGroup})
	}
	return result
}

func eventSummaryResponse(event *service.EventContent) *EventSummaryResponse {
	if event == nil {
		return nil
	}
	return &EventSummaryResponse{Title: event.Title, Location: event.Location, AttendeeCount: event.AttendeeCount, StartTime: event.StartTime, EndTime: event.EndTime, HTMLLink: event.HTMLLink}
}

func callSummaryResponse(call *service.CallContent) *CallSummaryResponse {
	if call == nil {
		return nil
	}
	return &CallSummaryResponse{Service: call.Service, Answered: call.Answered, HasVoicemail: call.HasVoicemail, DurationSeconds: call.DurationSeconds}
}

// GetInteractionContent returns the assembled read-only content for an interaction.
// @Summary Get interaction content
// @Tags interactions
// @Produce json
// @Param id path string true "Interaction ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=InteractionContentResponse}
// @Failure 404 {object} api.APIResponse
// @Router /interactions/{id}/content [get]
func (h *InteractionHandler) GetInteractionContent(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "interaction")
	if !ok {
		return
	}
	content, err := h.content.GetContent(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Interaction")
			return
		}
		api.RespondInternal(c, err)
		return
	}
	messages := make([]ContentMessageResponse, 0, len(content.Messages))
	for _, message := range content.Messages {
		messages = append(messages, ContentMessageResponse{ID: message.ID.String(), Sender: message.Sender, IsOutgoing: message.IsOutgoing, SentAt: message.SentAt, Body: message.Body, VenueKey: message.VenueKey})
	}
	notes := make([]MeetingNoteContentResponse, 0, len(content.MeetingNotes))
	for _, note := range content.MeetingNotes {
		notes = append(notes, MeetingNoteContentResponse{Title: note.Title, Summary: note.Summary, Memo: note.Memo})
	}
	api.SendSuccess(c, http.StatusOK, InteractionContentResponse{InteractionID: id.String(), Kind: content.Kind, Messages: messages, MeetingNotes: notes}, nil)
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
	contactID, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	if h.manualHandler == nil {
		api.SendError(c, http.StatusServiceUnavailable, "interactions.disabled",
			"Interaction recording is disabled",
			"Set EVENT_BUS_INTERACTION_MODE=cutover to enable.")
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

	description := ""
	if req.Description != nil {
		description = *req.Description
	}

	interaction, err := h.manualHandler.Run(c.Request.Context(), contactID, direction, occurredAt, description)
	if err != nil {
		api.RespondError(c, err, "Contact")
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
	id, ok := api.ParseUUIDParam(c, "id", "interaction")
	if !ok {
		return
	}

	// Verify interaction exists
	_, err := h.interactionRepo.GetInteraction(c.Request.Context(), id)
	if err != nil {
		api.RespondError(c, err, "Interaction")
		return
	}

	if err := h.interactionRepo.SoftDeleteInteraction(c.Request.Context(), id); err != nil {
		api.RespondInternal(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}
