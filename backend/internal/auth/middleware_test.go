package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestAPIKeyMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	testConfig := &config.Config{
		External: config.ExternalConfig{
			APIKey: "test-api-key-12345",
		},
	}

	t.Run("valid API key in X-API-Key header", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/test", nil)
		c.Request.Header.Set("X-API-Key", "test-api-key-12345")

		middleware := APIKeyMiddleware(testConfig)
		middleware(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.False(t, c.IsAborted())
	})

	t.Run("valid API key in Authorization header", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/test", nil)
		c.Request.Header.Set("Authorization", "ApiKey test-api-key-12345")

		middleware := APIKeyMiddleware(testConfig)
		middleware(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.False(t, c.IsAborted())
	})

	t.Run("missing API key", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/test", nil)

		middleware := APIKeyMiddleware(testConfig)
		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.True(t, c.IsAborted())
		assert.Contains(t, w.Body.String(), "MISSING_API_KEY")
	})

	t.Run("invalid API key", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/test", nil)
		c.Request.Header.Set("X-API-Key", "wrong-key")

		middleware := APIKeyMiddleware(testConfig)
		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.True(t, c.IsAborted())
		assert.Contains(t, w.Body.String(), "INVALID_API_KEY")
	})

	t.Run("empty config API key allows nothing", func(t *testing.T) {
		emptyConfig := &config.Config{
			External: config.ExternalConfig{
				APIKey: "",
			},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/test", nil)
		c.Request.Header.Set("X-API-Key", "any-key")

		middleware := APIKeyMiddleware(emptyConfig)
		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.True(t, c.IsAborted())
	})
}
