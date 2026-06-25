package handlers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// SyncService defines the interface for sync operations used by the handler.
// This allows for easier testing with mock implementations.
type SyncService interface {
	TriggerSync(ctx context.Context, source string, accountID *string) error
	GetSyncStatus(ctx context.Context) ([]repository.SyncState, error)
	GetSyncStateBySource(ctx context.Context, source string, accountID *string) (*repository.SyncState, error)
	EnableSync(ctx context.Context, id uuid.UUID, enabled bool) (*repository.SyncState, error)
	GetSyncLogs(ctx context.Context, syncStateID uuid.UUID, limit, offset int32) ([]repository.SyncLog, error)
	CountSyncLogs(ctx context.Context, syncStateID uuid.UUID) (int64, error)
	GetRecentSyncLogs(ctx context.Context, limit int32) ([]repository.SyncLog, error)
	GetAvailableProviders() []sync.SourceConfig
}

// SyncHandler handles sync-related HTTP requests
type SyncHandler struct {
	syncService SyncService
	validator   *validator.Validate
}

// NewSyncHandler creates a new sync handler
func NewSyncHandler(syncService SyncService) *SyncHandler {
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
		api.RespondInternal(c, err)
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
		api.RespondError(c, err, "Sync state")
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

	accountID := req.AccountID

	// Pre-flight: reject account-scoped sources triggered without an account,
	// synchronously, so the client gets a 400 instead of a 202 that hides a
	// background failure. Mirrors the service-layer guard.
	for _, cfg := range h.syncService.GetAvailableProviders() {
		if cfg.Name == source {
			if cfg.RequiresAccount && service.AccountIDMissing(accountID) {
				api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
					"Account ID is required for this source", "")
				return
			}
			break
		}
	}

	// Enqueue the sync in a background goroutine with a detached context.
	// After #180 PR 3 this is just a river Insert (fast), but we keep the
	// goroutine so the HTTP client gets a 202 immediately even if the DB
	// briefly stalls. 30s timeout is plenty for an Insert under realistic
	// load; the old 5m was sized for full sync work, which is now off-handler.
	srcName := source // explicit capture for goroutine
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := h.syncService.TriggerSync(ctx, srcName, accountID); err != nil {
			// Log error - can't return to client since request already responded
			log.Error().Err(err).Str("source", srcName).Msg("background enqueue failed")
		}
	}()

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
		api.RespondError(c, err, "Sync state")
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
		api.RespondInternal(c, err)
		return
	}

	// Get total count for pagination
	total, err := h.syncService.CountSyncLogs(c.Request.Context(), id)
	if err != nil {
		api.RespondInternal(c, err)
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
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, logs, nil)
}
