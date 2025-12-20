package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/health"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDatabaseChecker implements health.DatabaseChecker for testing
type mockDatabaseChecker struct {
	shouldError bool
	err         error
}

func (m *mockDatabaseChecker) HealthCheck(ctx context.Context) error {
	if m.shouldError {
		if m.err != nil {
			return m.err
		}
		return errors.New("database connection failed")
	}
	return nil
}

func TestHealthEndpoint_Healthy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockDB := &mockDatabaseChecker{shouldError: false}
	healthChecker := health.NewHealthChecker(mockDB)

	router := gin.New()
	router.GET("/health", healthChecker.Handler)

	req, err := http.NewRequest("GET", "/health", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 200 when healthy
	assert.Equal(t, http.StatusOK, w.Code)

	var response health.HealthResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify status
	assert.Equal(t, "healthy", response.Status)

	// Verify timestamp is present
	assert.NotEmpty(t, response.Timestamp)

	// Verify version info
	assert.NotEmpty(t, response.Version.Version)

	// Verify database component is healthy
	dbStatus, ok := response.Components["database"]
	assert.True(t, ok, "database component should be present")
	assert.Equal(t, "healthy", dbStatus.Status)
	assert.NotNil(t, dbStatus.ResponseTime)
	assert.Nil(t, dbStatus.Error)

	// Verify system info is present
	assert.NotNil(t, response.System)
	assert.NotEmpty(t, response.System.Uptime)
	assert.NotEmpty(t, response.System.GoVersion)
	assert.Greater(t, response.System.NumGoroutine, 0)
	assert.NotEmpty(t, response.System.MemoryAlloc)

	// Check content type
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
}

func TestHealthEndpoint_Degraded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockDB := &mockDatabaseChecker{
		shouldError: true,
		err:         errors.New("connection refused"),
	}
	healthChecker := health.NewHealthChecker(mockDB)

	router := gin.New()
	router.GET("/health", healthChecker.Handler)

	req, err := http.NewRequest("GET", "/health", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 503 when degraded
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var response health.HealthResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify status is degraded
	assert.Equal(t, "degraded", response.Status)

	// Verify database component is unhealthy
	dbStatus, ok := response.Components["database"]
	assert.True(t, ok, "database component should be present")
	assert.Equal(t, "unhealthy", dbStatus.Status)
	assert.NotNil(t, dbStatus.Error)
	assert.Contains(t, *dbStatus.Error, "connection refused")
}

func TestHealthEndpoint_NoDatabaseConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Pass nil database
	healthChecker := health.NewHealthChecker(nil)

	router := gin.New()
	router.GET("/health", healthChecker.Handler)

	req, err := http.NewRequest("GET", "/health", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 503 when no database
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var response health.HealthResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify status is degraded
	assert.Equal(t, "degraded", response.Status)

	// Verify database component shows not configured
	dbStatus, ok := response.Components["database"]
	assert.True(t, ok, "database component should be present")
	assert.Equal(t, "unhealthy", dbStatus.Status)
	assert.NotNil(t, dbStatus.Error)
	assert.Contains(t, *dbStatus.Error, "not configured")
}

func TestHealthEndpoint_LegacyHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/health", health.HealthHandler)

	req, err := http.NewRequest("GET", "/health", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Legacy handler should always return 200
	assert.Equal(t, http.StatusOK, w.Code)

	var response health.HealthResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify status is ok (legacy)
	assert.Equal(t, "ok", response.Status)

	// Verify timestamp is present
	assert.NotEmpty(t, response.Timestamp)

	// Verify version info is present
	assert.NotEmpty(t, response.Version.Version)
}

func TestHealthResponse_JSONFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockDB := &mockDatabaseChecker{shouldError: false}
	healthChecker := health.NewHealthChecker(mockDB)

	router := gin.New()
	router.GET("/health", healthChecker.Handler)

	req, err := http.NewRequest("GET", "/health", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Parse as generic map to verify JSON structure
	var rawResponse map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &rawResponse)
	require.NoError(t, err)

	// Verify top-level fields exist
	assert.Contains(t, rawResponse, "status")
	assert.Contains(t, rawResponse, "timestamp")
	assert.Contains(t, rawResponse, "version")
	assert.Contains(t, rawResponse, "components")
	assert.Contains(t, rawResponse, "system")

	// Verify version structure
	version, ok := rawResponse["version"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, version, "version")
	assert.Contains(t, version, "build_time")
	assert.Contains(t, version, "git_commit")

	// Verify components structure
	components, ok := rawResponse["components"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, components, "database")

	// Verify database component structure
	dbComponent, ok := components["database"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, dbComponent, "status")
}
