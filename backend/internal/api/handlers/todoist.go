package handlers

import (
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TodoistHandler handles Todoist-related API endpoints
type TodoistHandler struct {
	oauthService *todoist.OAuthService
	syncRepo     *repository.SyncRepository
}

// NewTodoistHandler creates a new Todoist handler
func NewTodoistHandler(
	oauthService *todoist.OAuthService,
	syncRepo *repository.SyncRepository,
) *TodoistHandler {
	return &TodoistHandler{
		oauthService: oauthService,
		syncRepo:     syncRepo,
	}
}

// TodoistSettingsResponse represents the Todoist integration settings
type TodoistSettingsResponse struct {
	ProjectID             *string `json:"project_id,omitempty"`
	LabelID               *string `json:"label_id,omitempty"`
	LabelName             *string `json:"label_name,omitempty"`
	IntegrationInstanceID *string `json:"integration_instance_id,omitempty"`
	UserTimezone          *string `json:"user_timezone,omitempty"`
}

// TodoistSettingsUpdateRequest represents a request to update Todoist settings
type TodoistSettingsUpdateRequest struct {
	ProjectID *string `json:"project_id,omitempty"`
	LabelID   *string `json:"label_id,omitempty"`
}

// TodoistProjectResponse represents a Todoist project
type TodoistProjectResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TodoistLabelResponse represents a Todoist label
type TodoistLabelResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetSettings returns the current Todoist integration settings
// @Summary Get Todoist settings
// @Description Get the current Todoist integration settings (project, label)
// @Tags todoist
// @Produce json
// @Success 200 {object} api.APIResponse{data=TodoistSettingsResponse}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /todoist/settings [get]
func (h *TodoistHandler) GetSettings(c *gin.Context) {
	ctx := c.Request.Context()

	// Get the first (and only for v1) Todoist account
	accounts, err := h.oauthService.ListAccounts(ctx)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	if len(accounts) == 0 {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "No Todoist account connected", "")
		return
	}

	accountID := accounts[0].AccountID

	// Get sync state for this account
	state, err := h.syncRepo.GetSyncStateBySource(ctx, todoist.SourceName, &accountID)
	if err != nil {
		// No sync state yet - return empty settings
		api.SendSuccess(c, http.StatusOK, TodoistSettingsResponse{}, nil)
		return
	}

	// Extract settings from metadata
	settings := todoist.Settings{}
	if state.Metadata != nil {
		if v, ok := state.Metadata[todoist.MetadataKeyProjectID].(string); ok {
			settings.ProjectID = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyLabelID].(string); ok {
			settings.LabelID = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyLabelName].(string); ok {
			settings.LabelName = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyIntegrationInstance].(string); ok {
			settings.IntegrationInstanceID = v
		}
		if v, ok := state.Metadata[todoist.MetadataKeyUserTimezone].(string); ok {
			settings.UserTimezone = v
		}
	}

	resp := TodoistSettingsResponse{}
	if settings.ProjectID != "" {
		resp.ProjectID = &settings.ProjectID
	}
	if settings.LabelID != "" {
		resp.LabelID = &settings.LabelID
	}
	if settings.LabelName != "" {
		resp.LabelName = &settings.LabelName
	}
	if settings.IntegrationInstanceID != "" {
		resp.IntegrationInstanceID = &settings.IntegrationInstanceID
	}
	if settings.UserTimezone != "" {
		resp.UserTimezone = &settings.UserTimezone
	}

	api.SendSuccess(c, http.StatusOK, resp, nil)
}

// UpdateSettings updates the Todoist integration settings
// @Summary Update Todoist settings
// @Description Update the Todoist integration settings (project, label)
// @Tags todoist
// @Accept json
// @Produce json
// @Param request body TodoistSettingsUpdateRequest true "Settings update request"
// @Success 200 {object} api.APIResponse{data=TodoistSettingsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /todoist/settings [patch]
func (h *TodoistHandler) UpdateSettings(c *gin.Context) {
	ctx := c.Request.Context()

	var req TodoistSettingsUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeBadRequest, "Invalid request body", err.Error())
		return
	}

	// Get the first (and only for v1) Todoist account
	accounts, err := h.oauthService.ListAccounts(ctx)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	if len(accounts) == 0 {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "No Todoist account connected", "")
		return
	}

	accountID := accounts[0].AccountID

	// Get or create sync state
	state, err := h.syncRepo.GetSyncStateBySource(ctx, todoist.SourceName, &accountID)
	if err != nil {
		// Create new sync state
		state, err = h.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    todoist.SourceName,
			AccountID: &accountID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyFetchAll,
		})
		if err != nil {
			api.RespondInternal(c, err)
			return
		}
	}

	// Build updated metadata
	metadata := make(map[string]any)
	if state.Metadata != nil {
		for k, v := range state.Metadata {
			metadata[k] = v
		}
	}

	// Update project_id if provided
	if req.ProjectID != nil {
		metadata[todoist.MetadataKeyProjectID] = *req.ProjectID
	}

	// Update label_id if provided
	if req.LabelID != nil {
		metadata[todoist.MetadataKeyLabelID] = *req.LabelID
		// Need to fetch label name - do this in next sync
		// For now, clear label_name so it gets refreshed
		delete(metadata, todoist.MetadataKeyLabelName)
	}

	// Generate integration_instance_id if not present
	if _, ok := metadata[todoist.MetadataKeyIntegrationInstance]; !ok {
		metadata[todoist.MetadataKeyIntegrationInstance] = uuid.New().String()
	}

	// Update metadata
	_, err = h.syncRepo.UpdateSyncStateMetadata(ctx, state.ID, metadata)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Return updated settings
	resp := TodoistSettingsResponse{}
	if v, ok := metadata[todoist.MetadataKeyProjectID].(string); ok && v != "" {
		resp.ProjectID = &v
	}
	if v, ok := metadata[todoist.MetadataKeyLabelID].(string); ok && v != "" {
		resp.LabelID = &v
	}
	if v, ok := metadata[todoist.MetadataKeyLabelName].(string); ok && v != "" {
		resp.LabelName = &v
	}
	if v, ok := metadata[todoist.MetadataKeyIntegrationInstance].(string); ok && v != "" {
		resp.IntegrationInstanceID = &v
	}
	if v, ok := metadata[todoist.MetadataKeyUserTimezone].(string); ok && v != "" {
		resp.UserTimezone = &v
	}

	api.SendSuccess(c, http.StatusOK, resp, nil)
}

// ListProjects returns all projects from the user's Todoist account
// @Summary List Todoist projects
// @Description Get all projects from the connected Todoist account
// @Tags todoist
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]TodoistProjectResponse}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /todoist/projects [get]
func (h *TodoistHandler) ListProjects(c *gin.Context) {
	ctx := c.Request.Context()

	// Get the first (and only for v1) Todoist account
	accounts, err := h.oauthService.ListAccounts(ctx)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	if len(accounts) == 0 {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "No Todoist account connected", "")
		return
	}

	accountID := accounts[0].AccountID

	// Get access token
	accessToken, err := h.oauthService.GetAccessToken(ctx, accountID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Fetch projects from Todoist
	client := todoist.NewSyncClient(accessToken)
	syncResp, err := client.Sync(ctx, "*", []string{"projects"}, nil)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Convert to response
	projects := make([]TodoistProjectResponse, 0, len(syncResp.Projects))
	for _, p := range syncResp.Projects {
		if !p.IsDeleted {
			projects = append(projects, TodoistProjectResponse{
				ID:   p.ID,
				Name: p.Name,
			})
		}
	}

	api.SendSuccess(c, http.StatusOK, projects, nil)
}

// ListLabels returns all labels from the user's Todoist account
// @Summary List Todoist labels
// @Description Get all labels from the connected Todoist account
// @Tags todoist
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]TodoistLabelResponse}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /todoist/labels [get]
func (h *TodoistHandler) ListLabels(c *gin.Context) {
	ctx := c.Request.Context()

	// Get the first (and only for v1) Todoist account
	accounts, err := h.oauthService.ListAccounts(ctx)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	if len(accounts) == 0 {
		api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "No Todoist account connected", "")
		return
	}

	accountID := accounts[0].AccountID

	// Get access token
	accessToken, err := h.oauthService.GetAccessToken(ctx, accountID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Fetch labels from Todoist
	client := todoist.NewSyncClient(accessToken)
	syncResp, err := client.Sync(ctx, "*", []string{"labels"}, nil)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// Convert to response
	labels := make([]TodoistLabelResponse, 0, len(syncResp.Labels))
	for _, l := range syncResp.Labels {
		if !l.IsDeleted {
			labels = append(labels, TodoistLabelResponse{
				ID:   l.ID,
				Name: l.Name,
			})
		}
	}

	api.SendSuccess(c, http.StatusOK, labels, nil)
}
