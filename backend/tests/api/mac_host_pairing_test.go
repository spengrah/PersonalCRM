package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// macHostTestKey is the global API-key used to call admin routes in
// these tests. Distinct value so a stray request that uses the wrong
// auth surfaces as 401 rather than masquerading as a valid call.
const macHostTestKey = "test-mac-host-admin-key"

// macHostTestEnv bundles the wired stack for mac_host integration tests.
// Each TestXxx builds its own env. Mac host tests run serially because the
// mac_host table intentionally has singleton semantics and cleanup hard-deletes
// those shared rows from the package clone.
type macHostTestEnv struct {
	router     *gin.Engine
	apiKey     string
	database   *db.Database
	hostRepo   *repository.MacHostRepository
	tokenRepo  *repository.MacHostPairingTokenRepository
	syncRepo   *repository.SyncRepository
	macService *service.MacHostService
	macHandler *handlers.MacHostHandler
}

func setupMacHostEnv(t *testing.T) *macHostTestEnv {
	t.Helper()

	ctx := context.Background()

	database, cfg := newAPISharedTestDB(t, ctx)
	cfg.External.APIKey = macHostTestKey

	hostRepo := repository.NewMacHostRepository(database.Queries)
	tokenRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	externalRepo := repository.NewExternalContactRepository(database.Queries)

	// bcrypt cost 4 is the lowest bcrypt accepts; the speed makes
	// integration test execution tolerable while still exercising the
	// real bcrypt path.
	macService := service.NewMacHostService(hostRepo, tokenRepo, syncRepo, nil, externalRepo, nil, database.Pool, 4)
	limiter := auth.NewPairingIPRateLimiter()
	macHandler := handlers.NewMacHostHandler(macService, limiter)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))

	// Use the same route-registration helpers that main.go calls so the
	// tests cover the production routing surface (route ordering +
	// middleware splits + path-trie conflicts) rather than a parallel
	// hand-built router that can drift.
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    hostRepo,
		Handler:     macHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	handlers.RegisterMacHostAdminRoutes(v1, macHandler)

	env := &macHostTestEnv{
		router:     router,
		apiKey:     cfg.External.APIKey,
		database:   database,
		hostRepo:   hostRepo,
		tokenRepo:  tokenRepo,
		syncRepo:   syncRepo,
		macService: macService,
		macHandler: macHandler,
	}

	t.Cleanup(func() {
		// Hard-delete rows so the singleton index is empty for the next
		// test. mac_host has no deleted_at column. We also sweep any
		// strategy='push' external_sync_state rows the cursor-commit
		// flow has created — without this, the migration test's
		// 048-down guard fires on leftover push rows from earlier
		// tests in the same `go test` invocation.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Look up sync states, drop any with strategy='push'. (No
		// dedicated test-only delete-by-strategy query; the list
		// iteration is bounded by table size and only runs at
		// teardown.)
		states, _ := database.Queries.ListSyncStates(cleanCtx)
		for _, s := range states {
			if s.Strategy == "push" {
				_ = database.Queries.DeleteSyncState(cleanCtx, s.ID)
			}
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
	})

	return env
}

// macHTTP does a JSON request against env.router and returns the recorder.
func macHTTP(t *testing.T, env *macHostTestEnv, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// readData parses the standard {success, data: T} envelope into out.
func readData(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&envelope), "body: %s", w.Body.String())
	require.True(t, envelope.Success, "non-success envelope: %s", w.Body.String())
	require.NoError(t, json.Unmarshal(envelope.Data, out))
}

// cursorConflictBody mirrors the 409 response shape — error.code +
// data.current_cursor + data.current_epoch are all asserted.
type cursorConflictBody struct {
	Code          string
	CurrentCursor *string
	CurrentEpoch  *int64
}

func parseConflict(t *testing.T, w *httptest.ResponseRecorder) cursorConflictBody {
	t.Helper()
	var raw struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Data struct {
			CurrentCursor *string `json:"current_cursor,omitempty"`
			CurrentEpoch  *int64  `json:"current_epoch,omitempty"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&raw), "decode conflict body")
	return cursorConflictBody{
		Code:          raw.Error.Code,
		CurrentCursor: raw.Data.CurrentCursor,
		CurrentEpoch:  raw.Data.CurrentEpoch,
	}
}

func TestMacHost_FullPairingFlow(t *testing.T) {

	env := setupMacHostEnv(t)

	// 1. Admin requests a pairing token.
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/pairing-token", map[string]string{
		"X-API-Key": env.apiKey,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	readData(t, w, &tokenResp)
	require.NotEmpty(t, tokenResp.Token)
	require.True(t, tokenResp.ExpiresAt.After(accelerated.GetCurrentTime()))

	// 2. Daemon pairs with the token.
	// spec: MAC-003[0]
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    tokenResp.Token,
		"hostname":         "macbook-test-01",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var pair struct {
		HostID      uuid.UUID `json:"host_id"`
		APIKey      string    `json:"api_key"`
		CursorEpoch int64     `json:"cursor_epoch"`
	}
	readData(t, w, &pair)
	require.NotEqual(t, uuid.Nil, pair.HostID)
	require.NotEmpty(t, pair.APIKey)
	require.Equal(t, int64(1), pair.CursorEpoch)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": pair.HostID.String(),
		"Authorization": "Bearer " + pair.APIKey,
	}

	// 3. Heartbeat as the daemon.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
		"permissions":      json.RawMessage(`{"fda":true}`),
		"source_health":    json.RawMessage(`{"messages":{"ok":true}}`),
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// 4. Commit cursor with bad epoch → 409 with BOTH current_cursor and
	// current_epoch in the body so the daemon can reconcile fully.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":       "abc",
		"base_cursor":  "",
		"cursor_epoch": 999,
	})
	require.Equal(t, http.StatusConflict, w.Code, "expected 409 for bad epoch; body: %s", w.Body.String())
	epochConflict := parseConflict(t, w)
	require.Equal(t, "EPOCH_MISMATCH", epochConflict.Code)
	require.NotNil(t, epochConflict.CurrentEpoch)
	require.Equal(t, pair.CursorEpoch, *epochConflict.CurrentEpoch)
	require.NotNil(t, epochConflict.CurrentCursor, "current_cursor must be present even on epoch mismatch")

	// 5. Commit cursor with bad base → 409 with BOTH current_cursor and
	// current_epoch in the body.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":       "abc",
		"base_cursor":  "wrong-base",
		"cursor_epoch": pair.CursorEpoch,
	})
	require.Equal(t, http.StatusConflict, w.Code, "expected 409 for bad base; body: %s", w.Body.String())
	baseConflict := parseConflict(t, w)
	require.Equal(t, "BASE_CURSOR_MISMATCH", baseConflict.Code)
	require.NotNil(t, baseConflict.CurrentEpoch)
	require.Equal(t, pair.CursorEpoch, *baseConflict.CurrentEpoch)
	require.NotNil(t, baseConflict.CurrentCursor)

	// 6. Commit cursor with good base + backfill_complete=true → 200.
	// spec: MAC-015[0]
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":            "cursor-v1",
		"base_cursor":       "",
		"cursor_epoch":      pair.CursorEpoch,
		"backfill_complete": true,
	})
	require.Equal(t, http.StatusOK, w.Code, "expected 200; body: %s", w.Body.String())

	// 7. GET cursor → returns committed value AND backfill_complete.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var cur struct {
		Cursor           string `json:"cursor"`
		CursorEpoch      int64  `json:"cursor_epoch"`
		BackfillComplete bool   `json:"backfill_complete"`
	}
	readData(t, w, &cur)
	require.Equal(t, "cursor-v1", cur.Cursor)
	require.Equal(t, pair.CursorEpoch, cur.CursorEpoch)
	require.True(t, cur.BackfillComplete, "backfill_complete must echo the daemon's commit value")

	// 8. Fast-forward: commit again with the previous cursor as base.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":       "cursor-v2",
		"base_cursor":  "cursor-v1",
		"cursor_epoch": pair.CursorEpoch,
	})
	require.Equal(t, http.StatusOK, w.Code, "expected 200; body: %s", w.Body.String())

	// 9. KnownIDs returns {ids: []} on a fresh host. Source 'messages'
	// has no external_contact rows, so the response is always empty.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/known-ids", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var ids struct {
		IDs []struct {
			SourceID        string  `json:"source_id"`
			LastContentHash *string `json:"last_content_hash"`
		} `json:"ids"`
	}
	readData(t, w, &ids)
	require.NotNil(t, ids.IDs)
	require.Len(t, ids.IDs, 0)

	// 10. Admin DELETE → revokes + cascades.
	w = macHTTP(t, env, http.MethodDelete, "/api/v1/host/"+pair.HostID.String(), map[string]string{
		"X-API-Key": env.apiKey,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// External_sync_state rows for the host should now be gone.
	cursorRow, cursorErr := env.syncRepo.GetMacHostSyncCursor(context.Background(), "messages", pair.HostID)
	require.ErrorIs(t, cursorErr, db.ErrNotFound, "cursor rows must be deleted on revoke; got cursor=%v err=%v", cursorRow, cursorErr)

	// 11. Daemon's next heartbeat → 401.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 12. Pairing token reuse → 410.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    tokenResp.Token,
		"hostname":         "macbook-test-02",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusGone, w.Code, "consumed token must surface 410")
}

// spec: MAC-003[1]
func TestMacHost_PairingToken_Unknown(t *testing.T) {

	env := setupMacHostEnv(t)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    "not-a-real-token",
		"hostname":         "x",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusGone, w.Code, "unknown token must be 410, body: %s", w.Body.String())
}

// spec: MAC-003[3]
func TestMacHost_Pair_MissingHostname_400(t *testing.T) {

	env := setupMacHostEnv(t)

	// Mint a real token via the service so we know the only validation
	// gate left is the hostname check.
	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    plain,
		"hostname":         "",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "empty hostname must surface 400, body: %s", w.Body.String())
}

func TestMacHost_Pair_MissingToken_400(t *testing.T) {

	env := setupMacHostEnv(t)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    "",
		"hostname":         "host",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "empty token must surface 400, body: %s", w.Body.String())
}

// spec: MAC-003[2]
func TestMacHost_Singleton_SecondPairBlocked(t *testing.T) {

	env := setupMacHostEnv(t)

	// First pair.
	plain1, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    plain1,
		"hostname":         "first",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, "first pair: %s", w.Body.String())

	// Second pair attempt with a different token.
	plain2, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    plain2,
		"hostname":         "second",
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusConflict, w.Code, "second pair must be 409 from singleton; body: %s", w.Body.String())
}

func TestMacHost_CursorFirstWriteRace(t *testing.T) {

	env := setupMacHostEnv(t)

	// Pair a host directly via the service to bypass HTTP.
	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "racer", "0.1.0", 1)
	require.NoError(t, err)

	// Spawn N goroutines, each trying to commit a different cursor with
	// base_cursor="" at the same epoch. Exactly one must succeed.
	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := env.macService.CommitCursor(context.Background(), repository.CommitMacHostCursorParams{
				HostID:       res.HostID,
				Source:       "messages",
				BaseCursor:   "",
				NewCursor:    fmt.Sprintf("racer-%d", i),
				ClaimedEpoch: res.CursorEpoch,
			})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var success, conflict int
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var baseErr *repository.ErrCursorBaseMismatch
		require.Truef(t, errors.As(err, &baseErr), "unexpected error type: %v", err)
		conflict++
	}
	require.Equal(t, 1, success, "exactly one commit must win")
	require.Equal(t, N-1, conflict, "all other commits surface ErrCursorBaseMismatch")
}
