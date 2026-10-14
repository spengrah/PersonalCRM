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
// All writes go through manualHandler.Run, which
// opens a tx, publishes interaction.manual, invokes the consumer inline,
// and commits. When manualHandler is nil (interaction mode off),
// POST returns 503 Service Unavailable.
type InteractionHandler struct {
	manualHandler *service.ManualInteractionHandler
	content       *service.InteractionContentService
}

// NewInteractionHandler creates a new interaction handler. manualHandler
// may be nil; POST /contacts/:id/interactions returns 503 in that case.
func NewInteractionHandler(
	manualHandler *service.ManualInteractionHandler,
	content *service.InteractionContentService,
) *InteractionHandler {
	return &InteractionHandler{
		manualHandler: manualHandler,
		content:       content,
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
	timeline, err := h.content.ListTimeline(ctx, params)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	venues, err := h.content.ListContactVenues(ctx, contactID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	items := make([]InteractionListItemResponse, len(timeline.Items))
	for i, item := range timeline.Items {
		items[i] = interactionListItemResponse(item)
	}
	response := InteractionListResponse{Items: items, VenueOptions: venueTagResponses(venues)}

	api.SendSuccess(c, http.StatusOK, response, &api.Meta{Pagination: api.BuildPaginationMeta(int(page64), int(limit64), int64(timeline.Total))})
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

func interactionListItemResponse(item service.TimelineItem) InteractionListItemResponse {
	members := make([]TimelineMemberResponse, len(item.Members))
	for i, member := range item.Members {
		enriched := member.Enrichment
		members[i] = TimelineMemberResponse{
			Source: member.Source, SourceRef: member.SourceRef, Label: enriched.Label,
			ContentKind: enriched.ContentKind, MessageCount: enriched.MessageCount, IsGroup: enriched.IsGroup,
			VenueTags: venueTagResponses(enriched.VenueTags), Event: eventSummaryResponse(enriched.Event), Call: callSummaryResponse(enriched.Call),
		}
	}
	return InteractionListItemResponse{
		Identity: timelineIdentityResponse(item.Identity), Direction: item.Direction,
		OccurredAt: item.OccurredAt, CadenceInert: item.CadenceInert(), Members: members,
	}
}

func timelineIdentityResponse(identity service.TimelineIdentity) TimelineIdentityResponse {
	return TimelineIdentityResponse{Kind: identity.Kind, ID: identity.ID.String()}
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
	return &CallSummaryResponse{Service: call.Service, Direction: call.Direction, Answered: call.Answered, HasVoicemail: call.HasVoicemail, DurationSeconds: call.DurationSeconds}
}

// GetTimelineItemContent returns the assembled read-only content of a timeline
// item, member by member, with contact_id the participation context.
// @Summary Get timeline item content
// @Tags interactions
// @Produce json
// @Param kind path string true "Timeline item kind" Enums(interaction, message_burst, upcoming_event, missed_call)
// @Param id path string true "Timeline item ID" format(uuid)
// @Param contact_id query string true "Participation context contact ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=TimelineContentResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /timeline-items/{kind}/{id}/content [get]
func (h *InteractionHandler) GetTimelineItemContent(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "timeline item")
	if !ok {
		return
	}
	contactID, err := uuid.Parse(c.Query("contact_id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact_id", "contact_id must be a contact UUID")
		return
	}
	content, err := h.content.GetTimelineContent(c.Request.Context(), c.Param("kind"), id, contactID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUnknownTimelineKind):
			api.SendValidationError(c, "Invalid timeline item kind", err.Error())
		case errors.Is(err, db.ErrNotFound):
			api.SendNotFound(c, "Timeline item")
		default:
			api.RespondInternal(c, err)
		}
		return
	}
	members := make([]TimelineContentMemberResponse, len(content.Members))
	for i, member := range content.Members {
		messages := make([]ContentMessageResponse, 0, len(member.Content.Messages))
		for _, message := range member.Content.Messages {
			messages = append(messages, ContentMessageResponse{
				ID: message.ID.String(), Sender: message.Sender, IsOutgoing: message.IsOutgoing, SentAt: message.SentAt,
				Body: message.Body, VenueKey: message.VenueKey, Source: message.Source, Channel: message.Channel,
				Container: message.Container, ContentRef: message.ContentRef,
			})
		}
		notes := make([]MeetingNoteContentResponse, 0, len(member.Content.MeetingNotes))
		for _, note := range member.Content.MeetingNotes {
			notes = append(notes, MeetingNoteContentResponse{
				Title: note.Title, Summary: note.Summary, Memo: note.Memo, Source: note.Source,
				Channel: note.Channel, Container: note.Container, ContentRef: note.ContentRef,
			})
		}
		members[i] = TimelineContentMemberResponse{Source: member.Source, Kind: member.Content.Kind, Messages: messages, MeetingNotes: notes}
	}
	api.SendSuccess(c, http.StatusOK, TimelineContentResponse{Identity: timelineIdentityResponse(content.Identity), Members: members}, nil)
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
		api.SendError(c, http.StatusServiceUnavailable, api.ErrCodeInteractionsDisabled,
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
