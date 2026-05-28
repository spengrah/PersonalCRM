package handlers

import (
	"errors"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// meetingNoteResponse is the wire shape returned by resolve-link.
// Decouples the HTTP response from the repository struct so future
// schema additions don't accidentally leak through.
type meetingNoteResponse struct {
	ID               uuid.UUID  `json:"id"`
	AnarlogSessionID uuid.UUID  `json:"anarlog_session_id"`
	Title            *string    `json:"title"`
	LinkageState     string     `json:"linkage_state"`
	LinkedKind       *string    `json:"linked_kind"`
	LinkedID         *uuid.UUID `json:"linked_id"`
	MacHostID        *uuid.UUID `json:"mac_host_id"`
	MeetingAt        string     `json:"meeting_at"`
}

func newMeetingNoteResponse(row *repository.MeetingNote) *meetingNoteResponse {
	if row == nil {
		return nil
	}
	return &meetingNoteResponse{
		ID:               row.ID,
		AnarlogSessionID: row.AnarlogSessionID,
		Title:            row.Title,
		LinkageState:     row.LinkageState,
		LinkedKind:       row.LinkedKind,
		LinkedID:         row.LinkedID,
		MacHostID:        row.MacHostID,
		MeetingAt:        row.MeetingAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// MeetingNoteHandler handles user-driven conflict-resolution and
// needs-attention HTTP endpoints for the meeting_note staging table.
type MeetingNoteHandler struct {
	svc       *service.MeetingNoteService
	validator *validator.Validate
}

// NewMeetingNoteHandler constructs a handler backed by the given
// service.
func NewMeetingNoteHandler(svc *service.MeetingNoteService) *MeetingNoteHandler {
	return &MeetingNoteHandler{
		svc:       svc,
		validator: validator.New(),
	}
}

// resolveLinkRequest is the discriminated-union body for POST
// /meeting-notes/:id/resolve-link. Action is the discriminator; Kind
// + ID are required only when Action="link".
type resolveLinkRequest struct {
	Action string  `json:"action" binding:"required" validate:"required,oneof=link none_of_these"`
	Kind   *string `json:"kind,omitempty"`
	ID     *string `json:"id,omitempty"`
}

const (
	resolveActionLink        = "link"
	resolveActionNoneOfThese = "none_of_these"
	candidateKindEvent       = "event"
	candidateKindPhoneCall   = "phone_call"
)

// ResolveLink implements POST /api/v1/meeting-notes/:id/resolve-link.
func (h *MeetingNoteHandler) ResolveLink(c *gin.Context) {
	mnID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "Invalid meeting_note id", "id must be a valid UUID")
		return
	}

	var req resolveLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	var input *service.ResolveLinkInput
	switch req.Action {
	case resolveActionLink:
		if req.Kind == nil || req.ID == nil {
			api.SendValidationError(c, "action=link requires kind and id", "")
			return
		}
		if *req.Kind != candidateKindEvent && *req.Kind != candidateKindPhoneCall {
			api.SendValidationError(c, "action=link requires kind in {event, phone_call}", "")
			return
		}
		targetID, parseErr := uuid.Parse(*req.ID)
		if parseErr != nil {
			api.SendValidationError(c, "action=link requires id as UUID", parseErr.Error())
			return
		}
		v := service.ResolveLinkInputFromKind(*req.Kind, targetID)
		input = &v
	case resolveActionNoneOfThese:
		// Tolerate kind+id in the body — defense in depth for clients
		// that always send the fields; the action discriminator is the
		// source of truth.
		input = nil
	default:
		api.SendValidationError(c, "action must be one of {link, none_of_these}", "")
		return
	}

	if h.svc == nil {
		api.SendError(c, http.StatusServiceUnavailable, api.ErrCodeInternal,
			"meeting_note resolve-link not configured", "")
		return
	}

	result, err := h.svc.ResolveLink(c.Request.Context(), mnID, input)
	if err != nil {
		h.mapResolveError(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, gin.H{
		"meeting_note":         newMeetingNoteResponse(result.MeetingNote),
		"interactions_created": result.InteractionsCreated,
	}, nil)
}

// mapResolveError translates service-layer errors into HTTP status codes.
func (h *MeetingNoteHandler) mapResolveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrResolveLinkRowNotFound):
		api.SendNotFound(c, "Meeting note")
	case errors.Is(err, service.ErrResolveLinkNotPending):
		api.SendConflict(c, "meeting_note is not awaiting conflict resolution")
	case errors.Is(err, service.ErrResolveLinkTargetMissing):
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound,
			"linked target no longer exists", "")
	case errors.Is(err, service.ErrResolveLinkIDNotCandidate):
		api.SendValidationError(c, "id is not one of the recorded candidates", "")
	case errors.Is(err, service.ErrResolveLinkSnapshotMissing):
		api.SendError(c, http.StatusUnprocessableEntity, api.ErrCodeValidation,
			"conflict_candidates snapshot missing on conflict_pending row; please re-trigger sync", "")
	default:
		api.SendInternalError(c, err.Error())
	}
}

// ListNeedsAttention implements GET /api/v1/meeting-notes/needs-attention.
// Optional ?host_id=<uuid> query parameter scopes the response to one
// mac_host.
func (h *MeetingNoteHandler) ListNeedsAttention(c *gin.Context) {
	var hostID *uuid.UUID
	if raw := c.Query("host_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid host_id", "host_id must be a valid UUID")
			return
		}
		hostID = &parsed
	}

	if h.svc == nil {
		api.SendError(c, http.StatusServiceUnavailable, api.ErrCodeInternal,
			"meeting_note needs-attention list not configured", "")
		return
	}

	items, err := h.svc.ListNeedsAttention(c.Request.Context(), hostID)
	if err != nil {
		api.SendInternalError(c, err.Error())
		return
	}
	api.SendSuccess(c, http.StatusOK, items, nil)
}
