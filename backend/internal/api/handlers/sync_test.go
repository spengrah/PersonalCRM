package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSyncService implements SyncService, returning configurable errors so the
// migrated RespondError / RespondInternal paths can be exercised without a DB.
type fakeSyncService struct {
	err error
}

func (f *fakeSyncService) TriggerSync(_ context.Context, _ string, _ *string) error {
	return f.err
}

func (f *fakeSyncService) GetAvailableProviders() []sync.SourceConfig {
	return nil
}

// fakeSyncStateStore implements SyncStateStore, returning configurable errors
// so the migrated RespondError / RespondInternal paths can be exercised
// without a DB.
type fakeSyncStateStore struct {
	err error
}

func (f *fakeSyncStateStore) ListSyncStates(_ context.Context) ([]repository.SyncState, error) {
	return nil, f.err
}

func (f *fakeSyncStateStore) GetSyncStateBySource(_ context.Context, _ string, _ *string) (*repository.SyncState, error) {
	return nil, f.err
}

func (f *fakeSyncStateStore) UpdateSyncStateEnabled(_ context.Context, _ uuid.UUID, _ bool) (*repository.SyncState, error) {
	return nil, f.err
}

func (f *fakeSyncStateStore) ListSyncLogsByState(_ context.Context, _ uuid.UUID, _, _ int32) ([]repository.SyncLog, error) {
	return nil, f.err
}

func (f *fakeSyncStateStore) CountSyncLogsByState(_ context.Context, _ uuid.UUID) (int64, error) {
	return 0, f.err
}

func (f *fakeSyncStateStore) ListRecentSyncLogs(_ context.Context, _ int32) ([]repository.SyncLog, error) {
	return nil, f.err
}

func newSyncTestRouter(svc SyncService, store SyncStateStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewSyncHandler(svc, store)
	r.GET("/sync/status", h.GetSyncStatus)
	r.GET("/sync/:source/status", h.GetSyncState)
	return r
}

// TestSyncHandler_GetSyncState_NotFound proves RespondError keeps the
// db.ErrNotFound → 404 mapping on a real migrated interface-backed handler.
func TestSyncHandler_GetSyncState_NotFound(t *testing.T) {
	r := newSyncTestRouter(&fakeSyncService{}, &fakeSyncStateStore{err: db.ErrNotFound})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/gmail/status", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Sync state not found")
}

// TestSyncHandler_GetSyncState_GenericError proves a non-not-found error still
// 500s (no drift to 404) with no raw cause leaking into the body.
func TestSyncHandler_GetSyncState_GenericError(t *testing.T) {
	r := newSyncTestRouter(&fakeSyncService{}, &fakeSyncStateStore{err: errors.New("connection reset secret")})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/gmail/status", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "connection reset secret")
	assert.NotContains(t, w.Body.String(), `"details"`)
}

func TestSyncHandler_GetSyncStatus_GenericError(t *testing.T) {
	r := newSyncTestRouter(&fakeSyncService{}, &fakeSyncStateStore{err: errors.New("db offline secret")})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync/status", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "db offline secret")
	assert.NotContains(t, w.Body.String(), `"details"`)
}
