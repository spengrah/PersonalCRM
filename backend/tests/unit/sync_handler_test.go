package unit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	psync "personal-crm/backend/internal/sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSyncService implements handlers.SyncService for testing
type mockSyncService struct {
	triggerSyncCalled  atomic.Bool
	triggerSyncDelay   time.Duration
	triggerSyncStarted chan struct{}
	triggerSyncDone    chan struct{}
}

// Verify mockSyncService implements handlers.SyncService
var _ handlers.SyncService = (*mockSyncService)(nil)

func newMockSyncService() *mockSyncService {
	return &mockSyncService{
		triggerSyncStarted: make(chan struct{}, 1),
		triggerSyncDone:    make(chan struct{}, 1),
	}
}

func (m *mockSyncService) TriggerSync(ctx context.Context, source string, accountID *string) error {
	m.triggerSyncCalled.Store(true)
	if m.triggerSyncStarted != nil {
		select {
		case m.triggerSyncStarted <- struct{}{}:
		default:
		}
	}

	if m.triggerSyncDelay > 0 {
		select {
		case <-time.After(m.triggerSyncDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if m.triggerSyncDone != nil {
		select {
		case m.triggerSyncDone <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *mockSyncService) GetSyncStatus(ctx context.Context) ([]repository.SyncState, error) {
	return nil, nil
}

func (m *mockSyncService) GetSyncStateBySource(ctx context.Context, source string, accountID *string) (*repository.SyncState, error) {
	return nil, nil
}

func (m *mockSyncService) EnableSync(ctx context.Context, id uuid.UUID, enabled bool) (*repository.SyncState, error) {
	return nil, nil
}

func (m *mockSyncService) GetSyncLogs(ctx context.Context, syncStateID uuid.UUID, limit, offset int32) ([]repository.SyncLog, error) {
	return nil, nil
}

func (m *mockSyncService) CountSyncLogs(ctx context.Context, syncStateID uuid.UUID) (int64, error) {
	return 0, nil
}

func (m *mockSyncService) GetRecentSyncLogs(ctx context.Context, limit int32) ([]repository.SyncLog, error) {
	return nil, nil
}

func (m *mockSyncService) GetAvailableProviders() []psync.SourceConfig {
	return nil
}

func setupSyncHandlerTestRouter(mockService *mockSyncService) *gin.Engine {
	gin.SetMode(gin.TestMode)

	// Create handler with mock service
	handler := handlers.NewSyncHandler(mockService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())

	v1 := router.Group("/api/v1")
	sync := v1.Group("/sync")
	{
		sync.POST("/:source/trigger", handler.TriggerSync)
	}

	return router
}

// TestTriggerSync_Returns202Immediately verifies that the TriggerSync handler
// returns 202 Accepted immediately without waiting for the sync to complete.
func TestTriggerSync_Returns202Immediately(t *testing.T) {
	mockService := newMockSyncService()
	// Simulate a slow sync operation
	mockService.triggerSyncDelay = 5 * time.Second

	router := setupSyncHandlerTestRouter(mockService)

	req, _ := http.NewRequest("POST", "/api/v1/sync/todoist/trigger", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Track timing
	start := accelerated.GetCurrentTime()
	router.ServeHTTP(w, req)
	elapsed := accelerated.GetCurrentTime().Sub(start)

	// Should return immediately (well under the 5s sync delay)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"Handler should return immediately, not wait for sync to complete")

	// Should return 202 Accepted
	if w.Code != http.StatusAccepted {
		t.Logf("Response body: %s", w.Body.String())
	}
	assert.Equal(t, http.StatusAccepted, w.Code)

	// Verify response body
	var response api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.True(t, response.Success)

	// Wait for background goroutine to start
	select {
	case <-mockService.triggerSyncStarted:
		// Good, sync started in background
	case <-time.After(1 * time.Second):
		t.Fatal("Sync should have started in background")
	}
}

// TestTriggerSync_BackgroundSyncRuns verifies that the sync actually runs
// in the background after the handler returns.
func TestTriggerSync_BackgroundSyncRuns(t *testing.T) {
	mockService := newMockSyncService()
	mockService.triggerSyncDelay = 100 * time.Millisecond

	router := setupSyncHandlerTestRouter(mockService)

	req, _ := http.NewRequest("POST", "/api/v1/sync/todoist/trigger", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	// Wait for background sync to complete
	select {
	case <-mockService.triggerSyncDone:
		assert.True(t, mockService.triggerSyncCalled.Load(),
			"TriggerSync should have been called")
	case <-time.After(2 * time.Second):
		t.Fatal("Background sync should have completed")
	}
}

// TestTriggerSync_RequiresSource verifies validation of source parameter.
func TestTriggerSync_RequiresSource(t *testing.T) {
	mockService := newMockSyncService()
	router := setupSyncHandlerTestRouter(mockService)

	// Empty source path segment still matches the route but fails validation
	req, _ := http.NewRequest("POST", "/api/v1/sync//trigger", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Handler returns 400 with "Source is required" validation error
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
