package handlers

import (
	"net/http"
	"strconv"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// IdentityHandler handles identity-related HTTP requests
type IdentityHandler struct {
	identityService *service.IdentityService
	validator       *validator.Validate
}

// NewIdentityHandler creates a new identity handler
func NewIdentityHandler(identityService *service.IdentityService) *IdentityHandler {
	return &IdentityHandler{
		identityService: identityService,
		validator:       validator.New(),
	}
}

// LinkIdentityRequest represents the request body for linking an identity
// @Description Request body for manually linking an identity to a contact
type LinkIdentityRequest struct {
	ContactID string `json:"contact_id" binding:"required,uuid" example:"123e4567-e89b-12d3-a456-426614174000"`
} // @name LinkIdentityRequest

// ListUnmatchedIdentities returns unmatched external identities
// @Summary List unmatched identities
// @Description Get external identities that couldn't be matched to CRM contacts
// @Tags identities
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(50)
// @Success 200 {object} api.APIResponse{data=[]repository.ExternalIdentity}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /identities/unmatched [get]
func (h *IdentityHandler) ListUnmatchedIdentities(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}

	offset := int32((page - 1) * limit)

	identities, err := h.identityService.ListUnmatchedIdentities(c.Request.Context(), int32(limit), offset)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	total, err := h.identityService.CountUnmatchedIdentities(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, identities, &api.Meta{
		Pagination: api.BuildPaginationMeta(page, limit, total),
	})
}

// GetIdentity returns an identity by ID
// @Summary Get identity
// @Description Get an external identity by ID
// @Tags identities
// @Produce json
// @Param id path string true "Identity ID"
// @Success 200 {object} api.APIResponse{data=repository.ExternalIdentity}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /identities/{id} [get]
func (h *IdentityHandler) GetIdentity(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "identity")
	if !ok {
		return
	}

	identity, err := h.identityService.GetIdentity(c.Request.Context(), id)
	if err != nil {
		api.RespondError(c, err, "Identity")
		return
	}

	api.SendSuccess(c, http.StatusOK, identity, nil)
}

// LinkIdentity manually links an identity to a contact
// @Summary Link identity to contact
// @Description Manually link an external identity to a CRM contact
// @Tags identities
// @Accept json
// @Produce json
// @Param id path string true "Identity ID"
// @Param request body LinkIdentityRequest true "Link request"
// @Success 200 {object} api.APIResponse{data=repository.ExternalIdentity}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /identities/{id}/link [post]
func (h *IdentityHandler) LinkIdentity(c *gin.Context) {
	identityID, ok := api.ParseUUIDParam(c, "id", "identity")
	if !ok {
		return
	}

	var req LinkIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	contactID, err := uuid.Parse(req.ContactID)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", err.Error())
		return
	}

	identity, err := h.identityService.LinkIdentity(c.Request.Context(), identityID, contactID)
	if err != nil {
		api.RespondError(c, err, "Identity")
		return
	}

	api.SendSuccess(c, http.StatusOK, identity, nil)
}

// UnlinkIdentity unlinks an identity from its contact
// @Summary Unlink identity from contact
// @Description Unlink an external identity from its CRM contact
// @Tags identities
// @Produce json
// @Param id path string true "Identity ID"
// @Success 200 {object} api.APIResponse{data=repository.ExternalIdentity}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /identities/{id}/unlink [post]
func (h *IdentityHandler) UnlinkIdentity(c *gin.Context) {
	identityID, ok := api.ParseUUIDParam(c, "id", "identity")
	if !ok {
		return
	}

	identity, err := h.identityService.UnlinkIdentity(c.Request.Context(), identityID)
	if err != nil {
		api.RespondError(c, err, "Identity")
		return
	}

	api.SendSuccess(c, http.StatusOK, identity, nil)
}

// DeleteIdentity removes an identity
// @Summary Delete identity
// @Description Delete an external identity
// @Tags identities
// @Produce json
// @Param id path string true "Identity ID"
// @Success 204 "No Content"
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /identities/{id} [delete]
func (h *IdentityHandler) DeleteIdentity(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "identity")
	if !ok {
		return
	}

	if err := h.identityService.DeleteIdentity(c.Request.Context(), id); err != nil {
		api.RespondInternal(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// ListIdentitiesForContact returns all identities for a contact
// @Summary List identities for contact
// @Description Get all external identities linked to a contact
// @Tags identities
// @Produce json
// @Param id path string true "Contact ID"
// @Success 200 {object} api.APIResponse{data=[]repository.ExternalIdentity}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /contacts/{id}/identities [get]
func (h *IdentityHandler) ListIdentitiesForContact(c *gin.Context) {
	contactID, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	identities, err := h.identityService.ListIdentitiesForContact(c.Request.Context(), contactID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, identities, nil)
}
