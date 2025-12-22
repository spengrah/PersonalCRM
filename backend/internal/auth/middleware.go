package auth

import (
	"net/http"
	"strings"

	"personal-crm/backend/internal/config"

	"github.com/gin-gonic/gin"
)

// APIKeyMiddleware validates API key from request headers
func APIKeyMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check X-API-Key header first (primary method)
		apiKey := c.GetHeader("X-API-Key")

		// Fallback to Authorization header
		if apiKey == "" {
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "ApiKey ") {
				apiKey = strings.TrimPrefix(authHeader, "ApiKey ")
			}
		}

		// Validate API key
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "MISSING_API_KEY",
					"message": "API key is required. Provide X-API-Key header or Authorization: ApiKey <key>",
				},
			})
			return
		}

		if apiKey != cfg.External.APIKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_API_KEY",
					"message": "Invalid API key provided",
				},
			})
			return
		}

		// API key is valid, continue
		c.Next()
	}
}
