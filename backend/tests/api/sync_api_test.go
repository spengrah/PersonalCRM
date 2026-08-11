//go:build integration_testdb

// Package api's sync_api_test.go covers all seven production SyncHandler
// endpoints (sync_routes.go) with an explicit per-endpoint citation
// decision: cite SET-014 (enable/disable contract), mint SET-034 (the
// trigger endpoint's synchronous account-scoped guard + acknowledgement
// body shape), and leave the five read endpoints tested-but-uncited (see
// the rationale block on TestSyncAPI_ReadEndpoints).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncNS builds a unique per-test source-name token so seeded sync states
// never collide across subtests/parallel runs, even though each subtest
// gets its own isolated DB clone (kept for pattern consistency with the
// rest of the suite).
func syncNS(t *testing.T) string {
	t.Helper()
	return "sync-" + uuid.NewString()[:8]
}

// fakeSyncProvider is a minimal sync.SyncProvider registry entry — not a
// service mock. It stands in for a real account-scoped provider (e.g.
// gmail) so the handler's account guard is genuinely exercised through the
// real registry.List()/Get() path. Sync/ValidateCredentials are never
// invoked on the paths this suite asserts.
type fakeSyncProvider struct {
	cfg sync.SourceConfig
}

func (f *fakeSyncProvider) Config() sync.SourceConfig { return f.cfg }

func (f *fakeSyncProvider) Sync(ctx context.Context, state *repository.SyncState, contacts []repository.Contact) (*sync.SyncResult, error) {
	return &sync.SyncResult{}, nil
}

func (f *fakeSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	return nil
}

// newSyncAPITest builds a fresh, empty isolated-River DB clone, wires the
// production sync route surface (RegisterSyncRoutes) over the real
// SyncHandler -> service.SyncService -> repository.SyncRepository stack,
// and returns the pieces subtests need to seed state and assert against the
// repository directly. Every subtest MUST call this as its first line:
// GetSyncStatus/GetRecentSyncLogs are DB-wide and unfiltered, so a shared
// clone would let one subtest's seeded rows leak into a sibling's
// exact-count/404 assertion, especially under -shuffle=on.
func newSyncAPITest(t *testing.T) (router *gin.Engine, syncRepo *repository.SyncRepository, registry *sync.ProviderRegistry, ctx context.Context) {
	t.Helper()
	ctx = context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	syncRepo = repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry = sync.NewProviderRegistry()

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	handler := handlers.NewSyncHandler(svc, syncRepo)

	router = gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterSyncRoutes(v1, handler)

	return router, syncRepo, registry, ctx
}

// doSyncRequest serves an HTTP request against the router, JSON-encoding
// body when non-nil, and returns the recorder.
func doSyncRequest(router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	router.ServeHTTP(w, req)
	return w
}

// syncStateResponse mirrors the JSON fields of repository.SyncState this
// suite asserts on.
type syncStateResponse struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
}

// decodeSyncState unwraps api.APIResponse and returns the sync-state
// payload.
func decodeSyncState(t *testing.T, w *httptest.ResponseRecorder) syncStateResponse {
	t.Helper()
	var envelope struct {
		Success bool              `json:"success"`
		Data    syncStateResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	return envelope.Data
}

// TestSyncAPI_EnableSync proves SET-014's full contract: the enabled flag
// is required and must parse as a bool, a malformed/unknown state id maps
// to 400/404, and toggling flips both the enabled flag and the derived
// status (disabled <-> idle).
// spec: SET-014
func TestSyncAPI_EnableSync(t *testing.T) {
	t.Parallel()

	t.Run("MissingEnabledParam_Returns400", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:  syncNS(t),
			Enabled: true,
		})
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/"+state.ID.String()+"/enable", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NonBooleanEnabled_Returns400", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:  syncNS(t),
			Enabled: true,
		})
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/"+state.ID.String()+"/enable?enabled=notabool", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("MalformedStateID_Returns400", func(t *testing.T) {
		router, _, _, _ := newSyncAPITest(t)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/not-a-uuid/enable?enabled=true", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("UnknownStateID_Returns404", func(t *testing.T) {
		router, _, _, _ := newSyncAPITest(t)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/"+uuid.NewString()+"/enable?enabled=true", nil)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Disable_SetsStatusDisabled", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:  syncNS(t),
			Enabled: true,
		})
		require.NoError(t, err)
		require.Equal(t, repository.SyncStatusIdle, state.Status)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/"+state.ID.String()+"/enable?enabled=false", nil)
		require.Equal(t, http.StatusOK, w.Code)

		got := decodeSyncState(t, w)
		assert.False(t, got.Enabled)
		assert.Equal(t, string(repository.SyncStatusDisabled), got.Status)
	})

	t.Run("Enable_SetsStatusIdle", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:  syncNS(t),
			Enabled: true,
		})
		require.NoError(t, err)

		_, err = syncRepo.UpdateSyncStateEnabled(ctx, state.ID, false)
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodPatch, "/api/v1/sync/states/"+state.ID.String()+"/enable?enabled=true", nil)
		require.Equal(t, http.StatusOK, w.Code)

		got := decodeSyncState(t, w)
		assert.True(t, got.Enabled)
		assert.Equal(t, string(repository.SyncStatusIdle), got.Status)
	})
}

// TestSyncAPI_TriggerSync proves SET-034: the synchronous account-scoped
// pre-flight guard rejects an account-scoped source triggered without an
// account (400, before any sync state is created) and a source the guard
// does not reject is acknowledged with a 202 whose body carries only an
// acknowledgement (message + source), no sync-result field.
//
// The async/non-await facet and the registered-source successful-
// enqueue/background-run path are intentionally NOT asserted here: the
// detached goroutine's DB work (for a *registered* source) races the
// per-subtest clone's teardown with no synchronization hook to await it,
// and a synchronous fast path would produce an identical 202 body, so
// non-await is unprovable at handler level without a seam. Those facets
// are owned by the sync service/worker integration tests; SET-034 item 2
// is scoped to the status+body-shape contract this subtest proves. The
// proven 202 instance is specifically the unknown-source/empty-body path
// (the only hermetic 202); the ack body is built by one unconditional
// SendSuccess call regardless of source, so proving the shape once locks
// it for all sources.
//
// SET-034's `notes` field mentions the fire-and-forget/detached-goroutine
// dispatch as background context for why item 2 is scoped so narrowly —
// that sentence is deliberately not part of `then` and is not asserted by
// either subtest below (the same non-load-bearing-notes pattern the corpus
// already uses, e.g. SET-030/SET-031's notes).
// spec: SET-034
func TestSyncAPI_TriggerSync(t *testing.T) {
	t.Parallel()

	t.Run("AccountRequiredSourceWithoutAccount_Returns400", func(t *testing.T) {
		router, syncRepo, registry, ctx := newSyncAPITest(t)
		ns := syncNS(t)
		registry.Register(&fakeSyncProvider{cfg: sync.SourceConfig{
			Name:            ns,
			RequiresAccount: true,
		}})

		w := doSyncRequest(router, http.MethodPost, "/api/v1/sync/"+ns+"/trigger", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, w.Code)

		// No sync state was bootstrapped: the pre-flight guard returns
		// before the accept path (and its detached goroutine) ever runs.
		_, err := syncRepo.GetSyncStateBySource(ctx, ns, nil)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("UnknownSource_AcksWith202", func(t *testing.T) {
		router, _, _, _ := newSyncAPITest(t)
		ns := syncNS(t) // deliberately never registered

		w := doSyncRequest(router, http.MethodPost, "/api/v1/sync/"+ns+"/trigger", nil)
		require.Equal(t, http.StatusAccepted, w.Code)

		var envelope struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)

		var ack map[string]string
		require.NoError(t, json.Unmarshal(envelope.Data, &ack))
		assert.Len(t, ack, 2, "ack body must carry only message + source, no sync-result field")
		assert.Equal(t, ns, ack["source"])
		assert.NotEmpty(t, ack["message"])
	})
}

// TestSyncAPI_ReadEndpoints exercises the five read-only sync endpoints —
// GET /sync/status, /sync/:source/status, /sync/providers, /sync/logs, and
// /sync/states/:id/logs. These are thin reflectors: they surface persisted
// sync-state/log rows or the in-memory provider registry, with generic
// GET-by-key 404 and malformed-id 400 framework contracts. Per
// spec/README.md ("a generic framework-level contract... that no behavior
// owns simply carries no marker") and the arc's leave-uncited sanction, no
// SET behavior is minted for this surface — the tests are still valuable
// (they lock the read shapes) but deliberately carry no spec citation.
func TestSyncAPI_ReadEndpoints(t *testing.T) {
	t.Parallel()

	t.Run("Status_ReturnsPersistedStates", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		nsA, nsB := syncNS(t), syncNS(t)
		_, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{Source: nsA, Enabled: true})
		require.NoError(t, err)
		_, err = syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{Source: nsB, Enabled: true})
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/status", nil)
		require.Equal(t, http.StatusOK, w.Code)

		var envelope struct {
			Success bool                `json:"success"`
			Data    []syncStateResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 2, "the isolated clone is empty except for the two seeded states")
		sources := []string{envelope.Data[0].Source, envelope.Data[1].Source}
		assert.Contains(t, sources, nsA)
		assert.Contains(t, sources, nsB)
	})

	t.Run("SourceStatus_ReturnsState", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		ns := syncNS(t)
		_, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{Source: ns, Enabled: true})
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/"+ns+"/status", nil)
		require.Equal(t, http.StatusOK, w.Code)

		got := decodeSyncState(t, w)
		assert.Equal(t, ns, got.Source)
	})

	t.Run("SourceStatus_UnknownSource_Returns404", func(t *testing.T) {
		router, _, _, _ := newSyncAPITest(t)
		ns := syncNS(t) // never seeded

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/"+ns+"/status", nil)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Providers_ListsRegistered", func(t *testing.T) {
		router, _, registry, _ := newSyncAPITest(t)
		ns := syncNS(t)
		registry.Register(&fakeSyncProvider{cfg: sync.SourceConfig{Name: ns}})

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/providers", nil)
		require.Equal(t, http.StatusOK, w.Code)

		var envelope struct {
			Success bool                `json:"success"`
			Data    []sync.SourceConfig `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1)
		assert.Equal(t, ns, envelope.Data[0].Name)
	})

	t.Run("RecentLogs_ReturnsLogs", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		ns := syncNS(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{Source: ns, Enabled: true})
		require.NoError(t, err)
		_, err = syncRepo.CreateSyncLog(ctx, state)
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/logs", nil)
		require.Equal(t, http.StatusOK, w.Code)

		var envelope struct {
			Success bool                 `json:"success"`
			Data    []repository.SyncLog `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1, "the isolated clone is empty except for the one seeded log")
		assert.Equal(t, ns, envelope.Data[0].Source)
	})

	t.Run("StateLogs_ReturnsLogsForState", func(t *testing.T) {
		router, syncRepo, _, ctx := newSyncAPITest(t)
		ns := syncNS(t)
		state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{Source: ns, Enabled: true})
		require.NoError(t, err)
		_, err = syncRepo.CreateSyncLog(ctx, state)
		require.NoError(t, err)

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/states/"+state.ID.String()+"/logs", nil)
		require.Equal(t, http.StatusOK, w.Code)

		var envelope struct {
			Success bool                 `json:"success"`
			Data    []repository.SyncLog `json:"data"`
			Meta    struct {
				Pagination *struct {
					Page  int   `json:"page"`
					Limit int   `json:"limit"`
					Total int64 `json:"total"`
					Pages int   `json:"pages"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1)
		require.NotNil(t, envelope.Meta.Pagination)
		assert.EqualValues(t, 1, envelope.Meta.Pagination.Total)
	})

	t.Run("StateLogs_MalformedID_Returns400", func(t *testing.T) {
		router, _, _, _ := newSyncAPITest(t)

		w := doSyncRequest(router, http.MethodGet, "/api/v1/sync/states/not-a-uuid/logs", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
