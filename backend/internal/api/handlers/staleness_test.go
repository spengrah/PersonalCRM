package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStalenessReader struct {
	breaches []repository.StalenessBreach
	err      error
}

func (f *fakeStalenessReader) ListActiveBreaches(_ context.Context) ([]repository.StalenessBreach, error) {
	return f.breaches, f.err
}

func newStalenessTestRouter(reader StalenessReader) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewStalenessHandler(reader)
	r.GET("/sync/staleness", h.GetActiveBreaches)
	return r
}

func TestStalenessHandler_GetActiveBreaches_ReturnsBreaches(t *testing.T) {
	now := accelerated.GetCurrentTime()
	reader := &fakeStalenessReader{breaches: []repository.StalenessBreach{{
		ID:               uuid.New(),
		Source:           "messages",
		AccountID:        "host-uuid",
		BreachType:       repository.BreachTypePushStale,
		StaleSince:       now.Add(-50 * time.Hour),
		ThresholdSeconds: 172800,
		Details:          "no push for 2d2h (threshold 48h)",
		DetectedAt:       now.Add(-1 * time.Hour),
		LastObservedAt:   now,
	}}}
	r := newStalenessTestRouter(reader)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/staleness", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Success bool                         `json:"success"`
		Data    []repository.StalenessBreach `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	require.Len(t, resp.Data, 1)
	assert.Equal(t, "messages", resp.Data[0].Source)
	assert.Equal(t, repository.BreachTypePushStale, resp.Data[0].BreachType)
}

func TestStalenessHandler_GetActiveBreaches_EmptyIsArrayNotNull(t *testing.T) {
	reader := &fakeStalenessReader{breaches: []repository.StalenessBreach{}}
	r := newStalenessTestRouter(reader)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/staleness", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// The data field must serialize as [] (empty array), never null, so the
	// frontend can iterate without a nil guard.
	assert.Contains(t, w.Body.String(), `"data":[]`)
}

func TestStalenessHandler_GetActiveBreaches_ErrorReturns500(t *testing.T) {
	reader := &fakeStalenessReader{err: errors.New("db down secret detail")}
	r := newStalenessTestRouter(reader)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/staleness", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), `"success":false`)
	// The raw cause must not leak into the 500 body.
	assert.NotContains(t, w.Body.String(), "db down secret detail")
	assert.NotContains(t, w.Body.String(), `"details"`)
}

// TestStalenessHandler_GetActiveBreaches_LogsCauseThroughMiddleware proves the
// end-to-end acceptance criterion: a 500 logs its cause with request_id, and the
// revived c.Error revives the LoggingMiddleware access-log error branch. It is SERIAL
// (no t.Parallel) because it mutates the process-global logger via logger.SetOutput.
func TestStalenessHandler_GetActiveBreaches_LogsCauseThroughMiddleware(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	defer restore()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(api.RequestIDMiddleware(), api.LoggingMiddleware())
	h := NewStalenessHandler(&fakeStalenessReader{err: errors.New("upstream exploded")})
	r.GET("/sync/staleness", h.GetActiveBreaches)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/staleness", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	logOutput := buf.String()
	// The explicit handler-error log carries the cause...
	assert.Contains(t, logOutput, "upstream exploded")
	// ...and the access-log line now carries the error field (c.Error revived the branch).
	assert.Contains(t, logOutput, `"error"`)
}
