package handlers

import (
	"errors"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// AnarlogDiscoveryHandler serves the People-tab discovery surface: the
// grouped anarlog_title token list and the token-group resolve endpoint.
// Kept as a thin handler separate from ImportHandler so the discovery
// surface stays cohesive and independently testable.
type AnarlogDiscoveryHandler struct {
	svc       *service.AnarlogDiscoveryService
	validator *validator.Validate
}

// NewAnarlogDiscoveryHandler constructs a handler backed by the given
// service.
func NewAnarlogDiscoveryHandler(svc *service.AnarlogDiscoveryService) *AnarlogDiscoveryHandler {
	return &AnarlogDiscoveryHandler{
		svc:       svc,
		validator: validator.New(),
	}
}

// resolveDiscoveryRequest is the body for POST /imports/anarlog-title/resolve.
// NormalizedToken + Action are always required; CRMContactID is required
// only for action=link (enforced in the handler after parse). Name and
// Cadence are accepted for both import and link.
type resolveDiscoveryRequest struct {
	NormalizedToken string  `json:"normalized_token" validate:"required"`
	Action          string  `json:"action" validate:"required,oneof=import link ignore"`
	Name            *string `json:"name,omitempty"`
	Cadence         *string `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
	CRMContactID    *string `json:"crm_contact_id,omitempty"`
}

// resolveDiscoveryResponse is the wire shape returned by resolve.
// ContactID is present for import (newly created) and link (the linked
// contact), and omitted for ignore.
type resolveDiscoveryResponse struct {
	Action    string  `json:"action"`
	ContactID *string `json:"contact_id,omitempty"`
}

// ListAnarlogTitle implements GET /api/v1/imports/anarlog-title.
func (h *AnarlogDiscoveryHandler) ListAnarlogTitle(c *gin.Context) {
	if h.svc == nil {
		api.SendError(c, http.StatusServiceUnavailable, api.ErrCodeInternal,
			"anarlog discovery not configured", "")
		return
	}
	groups, err := h.svc.ListGroups(c.Request.Context())
	if err != nil {
		api.SendInternalError(c, err.Error())
		return
	}
	api.SendSuccess(c, http.StatusOK, groups, nil)
}

// ResolveAnarlogTitle implements POST /api/v1/imports/anarlog-title/resolve.
func (h *AnarlogDiscoveryHandler) ResolveAnarlogTitle(c *gin.Context) {
	var req resolveDiscoveryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	svcReq := service.ResolveTokenRequest{
		NormalizedToken: req.NormalizedToken,
		Action:          req.Action,
		Name:            req.Name,
		Cadence:         req.Cadence,
	}

	if req.Action == service.DiscoveryActionLink {
		if req.CRMContactID == nil {
			api.SendValidationError(c, "action=link requires crm_contact_id", "")
			return
		}
		contactID, parseErr := uuid.Parse(*req.CRMContactID)
		if parseErr != nil {
			api.SendValidationError(c, "crm_contact_id must be a valid UUID", parseErr.Error())
			return
		}
		svcReq.CRMContactID = &contactID
	}

	if h.svc == nil {
		api.SendError(c, http.StatusServiceUnavailable, api.ErrCodeInternal,
			"anarlog discovery not configured", "")
		return
	}

	result, err := h.svc.ResolveToken(c.Request.Context(), svcReq)
	if err != nil {
		h.mapResolveError(c, err)
		return
	}

	resp := resolveDiscoveryResponse{Action: result.Action}
	if result.ContactID != nil {
		idStr := result.ContactID.String()
		resp.ContactID = &idStr
	}
	api.SendSuccess(c, http.StatusOK, resp, nil)
}

// mapResolveError translates service-layer sentinels into HTTP status codes.
func (h *AnarlogDiscoveryHandler) mapResolveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrTokenGroupNotFound):
		api.SendNotFound(c, "Token group")
	case errors.Is(err, service.ErrDiscoveryContactMissing):
		api.SendNotFound(c, "Contact")
	default:
		api.SendInternalError(c, err.Error())
	}
}
