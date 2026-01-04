package handlers

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// OAuthHandler handles OAuth-related HTTP requests
type OAuthHandler struct {
	googleOAuth google.OAuthServiceInterface
	// State store for CSRF protection (in-memory, expires after 10 minutes)
	stateStore   map[string]time.Time
	stateStoreMu sync.RWMutex
	frontendURL  string
}

// NewOAuthHandler creates a new OAuth handler
func NewOAuthHandler(googleOAuth google.OAuthServiceInterface, frontendURL string) *OAuthHandler {
	h := &OAuthHandler{
		googleOAuth: googleOAuth,
		stateStore:  make(map[string]time.Time),
		frontendURL: frontendURL,
	}

	// Start cleanup goroutine for expired states
	go h.cleanupExpiredStates()

	return h
}

// cleanupExpiredStates removes expired states from the store
func (h *OAuthHandler) cleanupExpiredStates() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		h.stateStoreMu.Lock()
		now := accelerated.GetCurrentTime()
		for state, expiry := range h.stateStore {
			if now.After(expiry) {
				delete(h.stateStore, state)
			}
		}
		h.stateStoreMu.Unlock()
	}
}

// storeState stores a state value for CSRF protection
func (h *OAuthHandler) storeState(state string) {
	h.stateStoreMu.Lock()
	defer h.stateStoreMu.Unlock()
	// State expires in 10 minutes
	h.stateStore[state] = accelerated.GetCurrentTime().Add(10 * time.Minute)
}

// validateState validates and removes a state value
func (h *OAuthHandler) validateState(state string) bool {
	h.stateStoreMu.Lock()
	defer h.stateStoreMu.Unlock()

	expiry, exists := h.stateStore[state]
	if !exists {
		return false
	}

	delete(h.stateStore, state)

	return !accelerated.GetCurrentTime().After(expiry)
}

// GetGoogleAuthURLResponse is the response for getting the auth URL
type GetGoogleAuthURLResponse struct {
	URL   string `json:"url"`
	State string `json:"state"`
}

// GoogleAccountResponse represents a connected Google account
type GoogleAccountResponse struct {
	ID          string   `json:"id"`
	AccountID   string   `json:"account_id"`
	AccountName *string  `json:"account_name,omitempty"`
	ExpiresAt   *string  `json:"expires_at,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// GetGoogleAuthURL returns the authorization URL for Google OAuth
// @Summary Get Google OAuth authorization URL
// @Description Get the URL to redirect user to for Google authorization
// @Tags auth
// @Produce json
// @Success 200 {object} api.APIResponse{data=GetGoogleAuthURLResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /auth/google [get]
func (h *OAuthHandler) GetGoogleAuthURL(c *gin.Context) {
	state, err := google.GenerateState()
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to generate state", err.Error())
		return
	}

	// Store state for CSRF validation
	h.storeState(state)

	url := h.googleOAuth.GetAuthURL(state)

	api.SendSuccess(c, http.StatusOK, GetGoogleAuthURLResponse{
		URL:   url,
		State: state,
	}, nil)
}

// GoogleCallback handles the OAuth callback from Google
// @Summary Google OAuth callback
// @Description Handle the OAuth callback from Google (redirects to frontend)
// @Tags auth
// @Param code query string false "Authorization code"
// @Param state query string false "State for CSRF protection"
// @Param error query string false "Error from Google"
// @Success 302 "Redirect to frontend"
// @Router /auth/google/callback [get]
func (h *OAuthHandler) GoogleCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")
	errorParam := c.Query("error")

	redirectBase := h.frontendURL + "/settings"

	// Handle errors from Google
	if errorParam != "" {
		c.Redirect(http.StatusFound, redirectBase+"?auth=error&provider=google&message="+errorParam)
		return
	}

	// Validate state (CSRF protection)
	if !h.validateState(state) {
		c.Redirect(http.StatusFound, redirectBase+"?auth=error&provider=google&message=invalid_state")
		return
	}

	// Exchange code for tokens
	_, err := h.googleOAuth.ExchangeCode(c.Request.Context(), code)
	if err != nil {
		c.Redirect(http.StatusFound, redirectBase+"?auth=error&provider=google&message=exchange_failed")
		return
	}

	c.Redirect(http.StatusFound, redirectBase+"?auth=success&provider=google")
}

// ListGoogleAccounts returns all connected Google accounts
// @Summary List connected Google accounts
// @Description Get list of all connected Google accounts
// @Tags auth
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]GoogleAccountResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /auth/google/accounts [get]
func (h *OAuthHandler) ListGoogleAccounts(c *gin.Context) {
	accounts, err := h.googleOAuth.ListAccounts(c.Request.Context())
	if err != nil {
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to list accounts", err.Error())
		return
	}

	responses := make([]GoogleAccountResponse, len(accounts))
	for i, acc := range accounts {
		responses[i] = GoogleAccountResponse{
			ID:          acc.ID.String(),
			AccountID:   acc.AccountID,
			AccountName: acc.AccountName,
			Scopes:      acc.Scopes,
			CreatedAt:   acc.CreatedAt.Format(time.RFC3339),
			UpdatedAt:   acc.UpdatedAt.Format(time.RFC3339),
		}
		if acc.ExpiresAt != nil {
			expiresStr := acc.ExpiresAt.Format(time.RFC3339)
			responses[i].ExpiresAt = &expiresStr
		}
	}

	api.SendSuccess(c, http.StatusOK, responses, nil)
}

// GetGoogleAccountStatus returns the status of a specific Google account
// @Summary Get Google account status
// @Description Get the status of a specific connected Google account
// @Tags auth
// @Produce json
// @Param id path string true "Account UUID"
// @Success 200 {object} api.APIResponse{data=GoogleAccountResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /auth/google/accounts/{id}/status [get]
func (h *OAuthHandler) GetGoogleAccountStatus(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid account ID", err.Error())
		return
	}

	status, err := h.googleOAuth.GetAccountStatus(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Account not found", "")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to get account status", err.Error())
		return
	}

	response := GoogleAccountResponse{
		ID:          status.ID.String(),
		AccountID:   status.AccountID,
		AccountName: status.AccountName,
		Scopes:      status.Scopes,
		CreatedAt:   status.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   status.UpdatedAt.Format(time.RFC3339),
	}
	if status.ExpiresAt != nil {
		expiresStr := status.ExpiresAt.Format(time.RFC3339)
		response.ExpiresAt = &expiresStr
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// RevokeGoogleAccount disconnects a Google account
// @Summary Revoke Google account
// @Description Disconnect a Google account and revoke OAuth tokens
// @Tags auth
// @Produce json
// @Param id path string true "Account UUID"
// @Success 200 {object} api.APIResponse{data=map[string]string}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /auth/google/accounts/{id}/revoke [post]
func (h *OAuthHandler) RevokeGoogleAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid account ID", err.Error())
		return
	}

	err = h.googleOAuth.RevokeAccount(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendError(c, http.StatusNotFound, api.ErrCodeNotFound, "Account not found", "")
			return
		}
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Failed to revoke account", err.Error())
		return
	}

	api.SendSuccess(c, http.StatusOK, map[string]string{
		"message": "Google account disconnected successfully",
	}, nil)
}
