package api

import (
	"net/http"

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
	Pagination *PaginationMeta `json:"pagination,omitempty"`
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
