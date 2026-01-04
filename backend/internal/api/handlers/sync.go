package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// SyncHandler handles sync-related HTTP requests
type SyncHandler struct {
	syncService *service.SyncService
	validator   *validator.Validate
}

// NewSyncHandler creates a new sync handler
func NewSyncHandler(syncService *service.SyncService) *SyncHandler {
	return &SyncHandler{
		syncService: syncService,
		validator:   validator.New(),
	}
}

// TriggerSyncRequest represents the request body for triggering a sync
// @Description Request body for triggering a sync operation
type TriggerSyncRequest struct {
	AccountID *string `json:"account_id,omitempty" example:"user@gmail.com"`
} // @name TriggerSyncRequest

// GetSyncStatus returns status of all sync sources
// @Summary Get sync status
// @Description Get the current sync status for all external data sources
// @Tags sync
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]repository.SyncState}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/status [get]
func (h *SyncHandler) GetSyncStatus(c *gin.Context) {
	states, err := h.syncService.GetSyncStatus(c.Request.Context())
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get sync status", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, states, nil)
}

// GetAvailableProviders returns list of available sync providers
// @Summary List available sync providers
// @Description Get list of registered sync providers and their configurations
// @Tags sync
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]sync.SourceConfig}
// @Router /sync/providers [get]
func (h *SyncHandler) GetAvailableProviders(c *gin.Context) {
	providers := h.syncService.GetAvailableProviders()
	api.SendSuccess(c, http.StatusOK, providers, nil)
}

// GetSyncState returns status for a specific source
// @Summary Get sync state for a source
// @Description Get the sync state for a specific external data source
// @Tags sync
// @Produce json
// @Param source path string true "Source name (e.g., gmail, imessage)"
// @Param account_id query string false "Account ID (for multi-account sources)"
// @Success 200 {object} api.APIResponse{data=repository.SyncState}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/{source}/status [get]
func (h *SyncHandler) GetSyncState(c *gin.Context) {
	source := c.Param("source")
	if source == "" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Source is required", "")
		return
	}

	accountID := c.Query("account_id")
	var accountIDPtr *string
	if accountID != "" {
		accountIDPtr = &accountID
	}

	state, err := h.syncService.GetSyncStateBySource(c.Request.Context(), source, accountIDPtr)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Sync state not found", "")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get sync state", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, state, nil)
}

// TriggerSync manually triggers a sync for a source
// @Summary Trigger sync for a source
// @Description Manually trigger a sync operation for an external data source
// @Tags sync
// @Accept json
// @Produce json
// @Param source path string true "Source name (e.g., gmail, imessage)"
// @Param request body TriggerSyncRequest false "Sync trigger options"
// @Success 202 {object} api.APIResponse{data=map[string]string}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/{source}/trigger [post]
func (h *SyncHandler) TriggerSync(c *gin.Context) {
	source := c.Param("source")
	if source == "" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Source is required", "")
		return
	}

	var req TriggerSyncRequest
	// Allow empty body
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.syncService.TriggerSync(c.Request.Context(), source, req.AccountID); err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to trigger sync", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusAccepted, map[string]string{
		"message": "Sync triggered successfully",
		"source":  source,
	}, nil)
}

// EnableSync enables or disables sync for a source
// @Summary Enable/disable sync
// @Description Enable or disable sync for an external data source
// @Tags sync
// @Produce json
// @Param id path string true "Sync state ID"
// @Param enabled query bool true "Enable or disable"
// @Success 200 {object} api.APIResponse{data=repository.SyncState}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/{id}/enable [patch]
func (h *SyncHandler) EnableSync(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid sync state ID", err.Error())
		return
	}

	enabledStr := c.Query("enabled")
	if enabledStr == "" {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "enabled query parameter is required", "")
		return
	}

	enabled, err := strconv.ParseBool(enabledStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid enabled value", "Must be true or false")
		return
	}

	state, err := h.syncService.EnableSync(c.Request.Context(), id, enabled)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Sync state not found", "")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to update sync state", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, state, nil)
}

// GetSyncLogs returns sync logs for a source
// @Summary Get sync logs
// @Description Get sync operation logs for an external data source
// @Tags sync
// @Produce json
// @Param id path string true "Sync state ID"
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} api.APIResponse{data=[]repository.SyncLog}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/{id}/logs [get]
func (h *SyncHandler) GetSyncLogs(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid sync state ID", err.Error())
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

	offset := int32((page - 1) * limit)

	logs, err := h.syncService.GetSyncLogs(c.Request.Context(), id, int32(limit), offset)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get sync logs", err.Error())
		return
	}

	// Get total count for pagination
	total, err := h.syncService.CountSyncLogs(c.Request.Context(), id)
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to count sync logs", err.Error())
		return
	}

	pages := int(total) / limit
	if int(total)%limit > 0 {
		pages++
	}

	api.SendSuccess(c, http.StatusOK, logs, &api.Meta{
		Pagination: &api.PaginationMeta{
			Page:  page,
			Limit: limit,
			Total: total,
			Pages: pages,
		},
	})
}

// GetRecentSyncLogs returns recent sync logs across all sources
// @Summary Get recent sync logs
// @Description Get the most recent sync operation logs across all sources
// @Tags sync
// @Produce json
// @Param limit query int false "Number of logs to return" default(20)
// @Success 200 {object} api.APIResponse{data=[]repository.SyncLog}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/logs [get]
func (h *SyncHandler) GetRecentSyncLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))

	if limit < 1 || limit > 100 {
		limit = 20
	}

	logs, err := h.syncService.GetRecentSyncLogs(c.Request.Context(), int32(limit))
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get sync logs", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, logs, nil)
}
