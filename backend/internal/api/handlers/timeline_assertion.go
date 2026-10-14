package handlers

import (
	"errors"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TimelineAssertionHandler serves the one HTTP read of the assertion store
// (KNW-038): the assertions beside the content a timeline item's drill-down
// displays. It is read-only.
type TimelineAssertionHandler struct {
	assertions *service.TimelineAssertionService
}

// NewTimelineAssertionHandler creates the timeline assertions read's handler.
func NewTimelineAssertionHandler(assertions *service.TimelineAssertionService) *TimelineAssertionHandler {
	return &TimelineAssertionHandler{assertions: assertions}
}

// GetTimelineItemAssertions returns the assertions whose provenance cites a
// content item the timeline item's drill-down displays, with contact_id the
// participation context.
// @Summary Get timeline item assertions
// @Description Read-only. Each entry is one assertion beside one displayed content item, keyed by that item's content_ref. Correction assertions are never returned.
// @Tags interactions
// @Produce json
// @Param kind path string true "Timeline item kind" Enums(interaction, message_burst, upcoming_event, missed_call)
// @Param id path string true "Timeline item ID" format(uuid)
// @Param contact_id query string true "Participation context contact ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=TimelineAssertionsResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /timeline-items/{kind}/{id}/assertions [get]
func (h *TimelineAssertionHandler) GetTimelineItemAssertions(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "timeline item")
	if !ok {
		return
	}
	contactID, err := uuid.Parse(c.Query("contact_id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact_id", "contact_id must be a contact UUID")
		return
	}
	cited, err := h.assertions.ListTimelineAssertions(c.Request.Context(), c.Param("kind"), id, contactID)
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
	items := make([]TimelineAssertionResponse, len(cited))
	for i, a := range cited {
		items[i] = TimelineAssertionResponse{
			ContentRef: a.ContentRef, AssertionID: a.AssertionID.String(), Predicate: a.PredicateKey,
			Subject: a.SubjectLabel, Object: a.ObjectLabel, ValueText: a.ValueText, ValueNum: a.ValueNum,
			ValueBool: a.ValueBool, Status: a.Status,
		}
		if a.ValueDate != nil {
			date := a.ValueDate.Format("2006-01-02")
			items[i].ValueDate = &date
		}
	}
	api.SendSuccess(c, http.StatusOK, TimelineAssertionsResponse{Items: items}, nil)
}
