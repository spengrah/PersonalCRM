package handlers

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"personal-crm/backend/internal/api"
	tg "personal-crm/backend/internal/telegram"

	"github.com/gin-gonic/gin"
)

var phoneRegex = regexp.MustCompile(`^\+[0-9]{7,15}$`)

// TelegramHandler handles Telegram auth API endpoints.
type TelegramHandler struct {
	manager *tg.TelegramManager
}

// NewTelegramHandler creates a new Telegram handler.
func NewTelegramHandler(manager *tg.TelegramManager) *TelegramHandler {
	return &TelegramHandler{manager: manager}
}

type startAuthRequest struct {
	PhoneNumber string `json:"phone_number"`
}

type verifyCodeRequest struct {
	AuthToken string `json:"auth_token"`
	Code      string `json:"code"`
}

type verifyPasswordRequest struct {
	AuthToken string `json:"auth_token"`
	Password  string `json:"password"`
}

// StartAuth handles POST /api/v1/telegram/auth/start
func (h *TelegramHandler) StartAuth(c *gin.Context) {
	var req startAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	phone := strings.TrimSpace(req.PhoneNumber)
	if phone == "" {
		api.SendValidationError(c, "Phone number is required", "phone_number must not be empty")
		return
	}
	if !phoneRegex.MatchString(phone) {
		api.SendValidationError(c, "Invalid phone number format", "phone_number must start with + followed by 7-15 digits")
		return
	}

	token, result, err := h.manager.AuthManager().StartAuth(c.Request.Context(), phone)
	if err != nil {
		if errors.Is(err, tg.ErrAuthInProgress) {
			api.SendConflict(c, "Auth already in progress")
			return
		}
		if errors.Is(err, tg.ErrAlreadyConnected) {
			api.SendConflict(c, "Already connected — disconnect first")
			return
		}
		api.SendInternalError(c, "Failed to start auth")
		return
	}

	api.SendSuccess(c, http.StatusOK, gin.H{
		"auth_token": token,
		"status":     result.Status,
		"code_type":  result.CodeType,
		"expires_in": 300, // 5 minutes
	}, nil)
}

// VerifyCode handles POST /api/v1/telegram/auth/verify-code
func (h *TelegramHandler) VerifyCode(c *gin.Context) {
	var req verifyCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	if req.AuthToken == "" {
		api.SendValidationError(c, "Auth token is required", "auth_token must not be empty")
		return
	}
	if req.Code == "" {
		api.SendValidationError(c, "Code is required", "code must not be empty")
		return
	}

	result, err := h.manager.AuthManager().VerifyCode(req.AuthToken, req.Code)
	if err != nil {
		if errors.Is(err, tg.ErrAuthTokenInvalid) {
			api.SendBadRequest(c, "Invalid auth token")
			return
		}
		if errors.Is(err, tg.ErrAuthTokenExpired) {
			api.SendError(c, http.StatusGone, "GONE", "Auth token expired", "")
			return
		}
		if errors.Is(err, tg.ErrAuthInternal) {
			api.SendInternalError(c, "Failed to verify code")
			return
		}
		api.SendError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid verification code", "")
		return
	}

	response := gin.H{"status": result.Status}
	if result.Username != "" {
		response["username"] = result.Username
	}
	if result.UserID != 0 {
		response["user_id"] = result.UserID
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// VerifyPassword handles POST /api/v1/telegram/auth/verify-password
func (h *TelegramHandler) VerifyPassword(c *gin.Context) {
	var req verifyPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	if req.AuthToken == "" {
		api.SendValidationError(c, "Auth token is required", "auth_token must not be empty")
		return
	}
	if req.Password == "" {
		api.SendValidationError(c, "Password is required", "password must not be empty")
		return
	}

	result, err := h.manager.AuthManager().VerifyPassword(req.AuthToken, req.Password)
	if err != nil {
		if errors.Is(err, tg.ErrAuthTokenInvalid) {
			api.SendBadRequest(c, "Invalid auth token")
			return
		}
		if errors.Is(err, tg.ErrAuthTokenExpired) {
			api.SendError(c, http.StatusGone, "GONE", "Auth token expired", "")
			return
		}
		if errors.Is(err, tg.ErrAuthInternal) {
			api.SendInternalError(c, "Failed to verify password")
			return
		}
		api.SendError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid password", "")
		return
	}

	response := gin.H{"status": result.Status}
	if result.Username != "" {
		response["username"] = result.Username
	}
	if result.UserID != 0 {
		response["user_id"] = result.UserID
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// CancelAuth handles POST /api/v1/telegram/auth/cancel
func (h *TelegramHandler) CancelAuth(c *gin.Context) {
	h.manager.AuthManager().CancelAuth()
	api.SendSuccess(c, http.StatusOK, gin.H{"status": "cancelled"}, nil)
}

// Disconnect handles DELETE /api/v1/telegram/auth
func (h *TelegramHandler) Disconnect(c *gin.Context) {
	if err := h.manager.Disconnect(c.Request.Context()); err != nil {
		api.SendInternalError(c, "Failed to disconnect")
		return
	}
	api.SendSuccess(c, http.StatusOK, gin.H{"status": "disconnected"}, nil)
}

// GetStatus handles GET /api/v1/telegram/auth/status
func (h *TelegramHandler) GetStatus(c *gin.Context) {
	status := h.manager.Status()

	response := gin.H{
		"connected": status.Connected,
	}
	if status.Connected {
		response["status"] = "connected"
	} else {
		response["status"] = "disconnected"
	}
	if status.Username != nil {
		response["username"] = *status.Username
	}
	if status.PhoneNumber != nil {
		response["phone_number"] = *status.PhoneNumber
	}
	if status.LastSyncAt != nil {
		response["last_sync_at"] = status.LastSyncAt
	}
	if status.ConnectedAt != nil {
		response["connected_at"] = status.ConnectedAt
	}
	if status.Error != nil {
		response["error"] = *status.Error
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}
