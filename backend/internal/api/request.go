package api

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ParseUUIDParam parses a path parameter as a UUID. On failure it writes a
// 400 validation error with a normalized message and returns ok=false, so the
// caller can `if !ok { return }`. The status (400) matches every prior hand-rolled
// path-param parse; only the message text is normalized, and no err.Error() leaks.
func ParseUUIDParam(c *gin.Context, param, resource string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		SendValidationError(c, "Invalid "+resource+" ID", "ID must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}
