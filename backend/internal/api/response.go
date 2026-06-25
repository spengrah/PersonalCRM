package api

import (
	"errors"
	"net/http"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

	"github.com/gin-gonic/gin"
)

// APIResponse represents a standard API response
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   *APIError   `json:"error,omitempty"`
	Meta    *Meta       `json:"meta,omitempty"`
}

// APIError represents an error response
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Meta represents metadata for responses (pagination, etc.)
type Meta struct {
	Pagination                    *PaginationMeta `json:"pagination,omitempty"`
	HiddenUnresolvedTelegramCount int64           `json:"hidden_unresolved_telegram_count,omitempty"`
}

// PaginationMeta represents pagination metadata
type PaginationMeta struct {
	Page  int   `json:"page"`
	Limit int   `json:"limit"`
	Total int64 `json:"total"`
	Pages int   `json:"pages"`
}

// Standard error codes
const (
	ErrCodeValidation   = "VALIDATION_ERROR"
	ErrCodeNotFound     = "NOT_FOUND"
	ErrCodeUnauthorized = "UNAUTHORIZED"
	ErrCodeForbidden    = "FORBIDDEN"
	ErrCodeInternal     = "INTERNAL_ERROR"
	ErrCodeConflict     = "CONFLICT"
	ErrCodeBadRequest   = "BAD_REQUEST"
)

// SendSuccess sends a successful response
func SendSuccess(c *gin.Context, statusCode int, data interface{}, meta *Meta) {
	response := APIResponse{
		Success: true,
		Data:    data,
		Meta:    meta,
	}
	c.JSON(statusCode, response)
}

// SendError sends an error response
func SendError(c *gin.Context, statusCode int, code, message, details string) {
	response := APIResponse{
		Success: false,
		Error: &APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}
	c.JSON(statusCode, response)
}

// Convenience methods for common responses

func SendValidationError(c *gin.Context, message, details string) {
	SendError(c, http.StatusBadRequest, ErrCodeValidation, message, details)
}

func SendNotFound(c *gin.Context, resource string) {
	SendError(c, http.StatusNotFound, ErrCodeNotFound, resource+" not found", "")
}

func SendInternalError(c *gin.Context, message string) {
	SendError(c, http.StatusInternalServerError, ErrCodeInternal, "Internal server error", message)
}

func SendConflict(c *gin.Context, message string) {
	SendError(c, http.StatusConflict, ErrCodeConflict, message, "")
}

func SendBadRequest(c *gin.Context, message string) {
	SendError(c, http.StatusBadRequest, ErrCodeBadRequest, message, "")
}

// LogServerError logs an unexpected server-fault cause correlated by request_id and
// calls c.Error(err) so the LoggingMiddleware access-log branch fires. It does NOT write
// a response — callers that must preserve an existing non-standard body (e.g. system.go's
// raw c.JSON shapes) call this, then write their own response.
func LogServerError(c *gin.Context, err error) {
	if err == nil {
		return // gin panics on c.Error(nil); nothing to log either
	}
	requestID := "unknown"
	if id, ok := c.Get("request_id"); ok {
		if s, ok := id.(string); ok {
			requestID = s
		}
	}
	logger.Error().Str("request_id", requestID).Err(err).Msg("handler error")
	_ = c.Error(err)
}

// RespondInternal always responds 500 with a generic body (no err.Error() in Details),
// after LogServerError. Use at every site that currently 500s unconditionally. NEVER
// introduces a 404, so it cannot drift a current 500 into a 4xx.
func RespondInternal(c *gin.Context, err error) {
	LogServerError(c, err)
	SendInternalError(c, "") // generic body, empty Details, no leak
}

// RespondError handles sites that currently map db.ErrNotFound → 404 via SendNotFound:
//
//	db.ErrNotFound (incl. wrapped) → 404 "<resource> not found"  (expected; no error log)
//	anything else                  → RespondInternal (500 + log + c.Error)
//
// Use ONLY where the existing block is `if errors.Is(err, db.ErrNotFound) { SendNotFound }
// else { 500 }`, so the mapping is provably identical.
func RespondError(c *gin.Context, err error, resource string) {
	if err != nil && errors.Is(err, db.ErrNotFound) {
		SendNotFound(c, resource)
		return
	}
	RespondInternal(c, err)
}
