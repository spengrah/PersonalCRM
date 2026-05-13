package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// countingMW returns a gin.HandlerFunc that increments *calls each time
// it's invoked and aborts the request with the configured status code.
// Used by the composite-dispatch tests to verify which branch fired.
func countingMW(calls *int, abortStatus int, label string) gin.HandlerFunc {
	return func(c *gin.Context) {
		*calls++
		c.AbortWithStatusJSON(abortStatus, gin.H{"branch": label})
	}
}

func TestIngestAuthMiddleware_HostHeaderPresent_RunsHostBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var apiCalls, hostCalls int

	router := gin.New()
	router.POST("/x",
		IngestAuthMiddleware(
			countingMW(&apiCalls, http.StatusUnauthorized, "api"),
			countingMW(&hostCalls, http.StatusForbidden, "host"),
		),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) },
	)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Mac-Host-ID", "00000000-0000-0000-0000-000000000001")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, 0, apiCalls, "API-key branch must NOT fire when host header is present")
	require.Equal(t, 1, hostCalls, "host branch must fire exactly once")
	require.Equal(t, http.StatusForbidden, w.Code, "response status should come from host middleware")
}

func TestIngestAuthMiddleware_HostHeaderAbsent_RunsAPIKeyBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var apiCalls, hostCalls int

	router := gin.New()
	router.POST("/x",
		IngestAuthMiddleware(
			countingMW(&apiCalls, http.StatusUnauthorized, "api"),
			countingMW(&hostCalls, http.StatusForbidden, "host"),
		),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) },
	)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	// No X-Mac-Host-ID; do include an arbitrary API key value so the
	// stub branch can be observed.
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, 1, apiCalls, "API-key branch must fire exactly once")
	require.Equal(t, 0, hostCalls, "host branch must NOT fire when host header is absent")
	require.Equal(t, http.StatusUnauthorized, w.Code, "response status should come from API-key middleware")
}

func TestIngestAuthMiddleware_EmptyHostHeader_TreatedAsAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var apiCalls, hostCalls int

	router := gin.New()
	router.POST("/x",
		IngestAuthMiddleware(
			countingMW(&apiCalls, http.StatusUnauthorized, "api"),
			countingMW(&hostCalls, http.StatusForbidden, "host"),
		),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) },
	)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Mac-Host-ID", "") // explicit empty value
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, 1, apiCalls, "empty header value must fall back to API-key branch")
	require.Equal(t, 0, hostCalls)
}
