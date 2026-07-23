package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/jackc/pgx/v5/pgtype"
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
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":            "cursor-v1",
		"base_cursor":       "",
		"cursor_epoch":      pair.CursorEpoch,
		"backfill_complete": true,
	})
	require.Equal(t, http.StatusOK, w.Code, "expected 200; body: %s", w.Body.String())

	// 7. GET cursor → returns committed value AND backfill_complete.
	// spec: MAC-015[0]
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

// spec: MAC-003[3]
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

// TestMacHost_Heartbeat_PersistsFieldsAndEchoesProtocolState pairs a
// host, sends a heartbeat carrying distinct values for every
// daemon-supplied field, then reads the host back through the admin
// GET route to confirm each field was actually persisted — and
// separately decodes the heartbeat response body for the literal
// cursor_epoch/protocol_version/min_protocol_version keys.
func TestMacHost_Heartbeat_PersistsFieldsAndEchoesProtocolState(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "heartbeat-fields-host", "0.1.0", 1)
	require.NoError(t, err)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}

	before := accelerated.GetCurrentTime()

	// Distinct values on every daemon-supplied field. protocol_version
	// (2) and cursor_epoch (1, from pairing) are pairwise-distinct
	// numerics; the JSON blobs are distinct from each other and from
	// the empty defaults the handler substitutes when absent.
	const heartbeatDaemonVersion = "9.9.9-heartbeat-test"
	const heartbeatProtocolVersion = 2
	permissions := json.RawMessage(`{"fda":true,"screen_recording":false}`)
	sourceHealth := json.RawMessage(`{"messages":{"ok":true},"phone_calls":{"ok":false}}`)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   heartbeatDaemonVersion,
		"protocol_version": heartbeatProtocolVersion,
		"permissions":      permissions,
		"source_health":    sourceHealth,
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// spec: MAC-011[1]
	// spec: MAC-011[2]
	// Literal wire keys on the heartbeat response body — decoded via a
	// local struct with matching json tags, not the handler's private
	// response type.
	var hbResp struct {
		OK                 bool  `json:"ok"`
		CursorEpoch        int64 `json:"cursor_epoch"`
		ProtocolVersion    int32 `json:"protocol_version"`
		MinProtocolVersion int32 `json:"min_protocol_version"`
	}
	readData(t, w, &hbResp)
	require.True(t, hbResp.OK)
	require.Equal(t, res.CursorEpoch, hbResp.CursorEpoch, "cursor_epoch must echo the host's current epoch")
	require.EqualValues(t, 2, hbResp.ProtocolVersion, "protocol_version must be the server's current version (mac.ProtocolVersion)")
	require.EqualValues(t, 1, hbResp.MinProtocolVersion, "min_protocol_version must be the server's floor (mac.MinProtocolVersion)")

	// spec: MAC-011[0]
	// Read the host back via the admin GET route and assert every
	// heartbeat-supplied field was actually persisted.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String(), map[string]string{
		"X-API-Key": env.apiKey,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var hostView struct {
		DaemonVersion   string          `json:"daemon_version"`
		ProtocolVersion int32           `json:"protocol_version"`
		LastHeartbeatAt *time.Time      `json:"last_heartbeat_at"`
		Permissions     json.RawMessage `json:"permissions"`
		SourceHealth    json.RawMessage `json:"source_health"`
	}
	readData(t, w, &hostView)

	require.Equal(t, heartbeatDaemonVersion, hostView.DaemonVersion, "daemon_version must be persisted")
	require.EqualValues(t, heartbeatProtocolVersion, hostView.ProtocolVersion, "protocol_version must be persisted")
	require.NotNil(t, hostView.LastHeartbeatAt, "last_heartbeat_at must be recorded")
	require.False(t, hostView.LastHeartbeatAt.Before(before.Add(-time.Second)), "last_heartbeat_at must reflect the heartbeat, not a stale value")
	require.JSONEq(t, string(permissions), string(hostView.Permissions), "permissions must be persisted verbatim")
	require.JSONEq(t, string(sourceHealth), string(hostView.SourceHealth), "source_health must be persisted verbatim")
}

// TestMacHost_GetCursor_PreCommit_EmptyBaseline covers reading a
// freshly-paired host's cursor before any commit has happened: the
// cursor must be empty and the epoch must be the host's current
// (post-pairing) epoch, never a fabricated non-empty value.
func TestMacHost_GetCursor_PreCommit_EmptyBaseline(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "precommit-cursor-host", "0.1.0", 1)
	require.NoError(t, err)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}

	// spec: MAC-015[1]
	// No commit has happened yet for this host+source.
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/messages/cursor", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var cur struct {
		Cursor           string `json:"cursor"`
		CursorEpoch      int64  `json:"cursor_epoch"`
		BackfillComplete bool   `json:"backfill_complete"`
	}
	readData(t, w, &cur)
	require.Equal(t, "", cur.Cursor, "cursor must be empty before any commit")
	require.Equal(t, res.CursorEpoch, cur.CursorEpoch, "cursor_epoch must be the host's current epoch")
	require.False(t, cur.BackfillComplete, "backfill_complete must default to false before any commit")
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

// TestMacHost_Pair_PlaintextKeyNeverLeaksInSubsequentReads pairs a host
// and sweeps every subsequent read surface (heartbeat response, admin
// ListHosts, admin GetHostAdmin) for the plaintext api key, asserting
// on the raw response body string rather than any decoded struct —
// a struct round-trip could not catch a field the production DTO
// doesn't declare but the handler still serializes ad hoc.
func TestMacHost_Pair_PlaintextKeyNeverLeaksInSubsequentReads(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)

	// spec: MAC-003[0]
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    plain,
		"hostname":         "plaintext-sweep-" + uuid.NewString()[:8],
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, "pair: %s", w.Body.String())
	require.Contains(t, w.Body.String(), `"api_key"`, "sanity: pair response must actually carry the key field")

	var pair struct {
		HostID uuid.UUID `json:"host_id"`
		APIKey string    `json:"api_key"`
	}
	readData(t, w, &pair)
	require.NotEmpty(t, pair.APIKey)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": pair.HostID.String(),
		"Authorization": "Bearer " + pair.APIKey,
	}

	// Heartbeat response must never echo the plaintext key back.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+pair.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, "heartbeat: %s", w.Body.String())
	require.NotContains(t, w.Body.String(), pair.APIKey, "heartbeat response must never contain the plaintext api key")
	require.NotContains(t, w.Body.String(), `"api_key"`, "heartbeat response must never carry an api_key wire key at all")

	// Admin ListHosts.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host", map[string]string{"X-API-Key": env.apiKey}, nil)
	require.Equal(t, http.StatusOK, w.Code, "list hosts: %s", w.Body.String())
	require.NotContains(t, w.Body.String(), pair.APIKey, "ListHosts response must never contain the plaintext api key")
	require.NotContains(t, w.Body.String(), `"api_key"`, "ListHosts response must never carry an api_key wire key at all (not even hashed)")

	// Admin GetHostAdmin.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+pair.HostID.String(), map[string]string{"X-API-Key": env.apiKey}, nil)
	require.Equal(t, http.StatusOK, w.Code, "get admin: %s", w.Body.String())
	require.NotContains(t, w.Body.String(), pair.APIKey, "GetHostAdmin response must never contain the plaintext api key")
	require.NotContains(t, w.Body.String(), `"api_key"`, "GetHostAdmin response must never carry an api_key wire key at all (not even hashed)")
}

// spec: MAC-003[1]
// TestMacHost_Pair_ExpiredUnconsumedToken_410 seeds an expired-but-never-
// consumed pairing token directly (SeedPairingToken accepts an explicit
// expires_at, the same helper the rotate-key sibling test uses for its
// TOKEN_EXPIRED case) and drives it through the actual pairing endpoint,
// asserting the opaque 410 the pairing contract collapses all token-state
// failures to.
func TestMacHost_Pair_ExpiredUnconsumedToken_410(t *testing.T) {

	env := setupMacHostEnv(t)

	plaintext := "expired-pair-token-" + uuid.NewString()[:8]
	hash := sha256.Sum256([]byte(plaintext))
	_, err := env.database.Queries.SeedPairingToken(context.Background(), db.SeedPairingTokenParams{
		TokenHash: hex.EncodeToString(hash[:]),
		ExpiresAt: pgtype.Timestamptz{Time: accelerated.GetCurrentTime().Add(-1 * time.Hour), Valid: true},
	})
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    plaintext,
		"hostname":         "expired-token-host-" + uuid.NewString()[:8],
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusGone, w.Code, "expired unconsumed token must surface 410, body: %s", w.Body.String())
}

// TestMacHost_Heartbeat_TooOldProtocol_NoWriteBefore412 pairs a host,
// seeds distinguishable state via one accepted heartbeat, then sends a
// below-floor-protocol heartbeat carrying DIFFERENT values on every
// mutable field and asserts the 412 rejection AND that none of those
// fields moved — proving the protocol gate runs before any database
// write, not merely before the response is sent.
// spec: MAC-012[0]
func TestMacHost_Heartbeat_TooOldProtocol_NoWriteBefore412(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "protocol-gate-host-"+uuid.NewString()[:8], "0.1.0", 1)
	require.NoError(t, err)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}

	// Seed real, accepted state first so there is something concrete
	// to prove is untouched by the rejected heartbeat below.
	seedPermissions := json.RawMessage(`{"fda":true,"screen_recording":false}`)
	seedSourceHealth := json.RawMessage(`{"messages":{"ok":true}}`)
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   "1.2.3-seed",
		"protocol_version": 1,
		"permissions":      seedPermissions,
		"source_health":    seedSourceHealth,
	})
	require.Equal(t, http.StatusOK, w.Code, "seed heartbeat: %s", w.Body.String())

	pre, err := env.hostRepo.GetHost(context.Background(), res.HostID)
	require.NoError(t, err)
	require.NotNil(t, pre.LastHeartbeatAt, "seed heartbeat must have recorded last_heartbeat_at")

	// Below-floor protocol_version (server minimum is 1) carrying
	// DISTINCT values on every mutable field vs. the seed above.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/heartbeat", hostHeaders, map[string]any{
		"daemon_version":   "9.9.9-rejected",
		"protocol_version": 0,
		"permissions":      json.RawMessage(`{"fda":false,"screen_recording":true}`),
		"source_health":    json.RawMessage(`{"messages":{"ok":false},"phone_calls":{"ok":true}}`),
	})
	require.Equal(t, http.StatusPreconditionFailed, w.Code, "body: %s", w.Body.String())

	post, err := env.hostRepo.GetHost(context.Background(), res.HostID)
	require.NoError(t, err)

	require.Equal(t, pre.LastHeartbeatAt, post.LastHeartbeatAt, "last_heartbeat_at must be untouched by a 412-rejected heartbeat")
	require.Equal(t, pre.DaemonVersion, post.DaemonVersion, "daemon_version must be untouched by a 412-rejected heartbeat")
	require.Equal(t, pre.ProtocolVersion, post.ProtocolVersion, "protocol_version must be untouched by a 412-rejected heartbeat")
	require.JSONEq(t, string(pre.Permissions), string(post.Permissions), "permissions must be untouched by a 412-rejected heartbeat")
	require.JSONEq(t, string(pre.SourceHealth), string(post.SourceHealth), "source_health must be untouched by a 412-rejected heartbeat")
}

// TestMacHost_Cursor_BackfillComplete_DefaultsFalseWhenAbsent commits a
// cursor whose request body omits the backfill_complete key entirely
// (not merely sets it false) and asserts the read-back flag is false.
// spec: MAC-015[2]
func TestMacHost_Cursor_BackfillComplete_DefaultsFalseWhenAbsent(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "backfill-default-host-"+uuid.NewString()[:8], "0.1.0", 1)
	require.NoError(t, err)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}

	// Deliberately no "backfill_complete" key in this map at all.
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/sync/messages/cursor", hostHeaders, map[string]any{
		"cursor":       "cursor-no-backfill-key",
		"base_cursor":  "",
		"cursor_epoch": res.CursorEpoch,
	})
	require.Equal(t, http.StatusOK, w.Code, "commit: %s", w.Body.String())

	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/messages/cursor", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code, "get cursor: %s", w.Body.String())
	var cur struct {
		Cursor           string `json:"cursor"`
		BackfillComplete bool   `json:"backfill_complete"`
	}
	readData(t, w, &cur)
	require.Equal(t, "cursor-no-backfill-key", cur.Cursor)
	require.False(t, cur.BackfillComplete, "backfill_complete must default to false when the commit body omits the key")
}

// TestMacHost_PushSourceAllowlist_HTTP drives the cursor get/commit and
// known-ids endpoints with an unknown push source and asserts the
// validation error, then drives one allowed source as acceptance —
// exercising the handler-level allowlist gate (mac.IsAllowedPushSource)
// over HTTP rather than only at the unit level.
// spec: MAC-013
func TestMacHost_PushSourceAllowlist_HTTP(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "allowlist-host-"+uuid.NewString()[:8], "0.1.0", 1)
	require.NoError(t, err)

	hostHeaders := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}

	const unknownSource = "not_a_real_source"

	assertValidationError := func(w *httptest.ResponseRecorder, label string) {
		require.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", label, w.Body.String())
		var errBody struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&errBody))
		require.Equal(t, "VALIDATION_ERROR", errBody.Error.Code, "%s error code", label)
	}

	// GET cursor rejects unknown source.
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/"+unknownSource+"/cursor", hostHeaders, nil)
	assertValidationError(w, "GetCursor unknown source")

	// POST cursor (commit) rejects unknown source.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/sync/"+unknownSource+"/cursor", hostHeaders, map[string]any{
		"cursor":       "x",
		"base_cursor":  "",
		"cursor_epoch": res.CursorEpoch,
	})
	assertValidationError(w, "CommitCursor unknown source")

	// GET known-ids rejects unknown source.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/"+unknownSource+"/known-ids", hostHeaders, nil)
	assertValidationError(w, "KnownIDs unknown source")

	// Acceptance: a genuinely allowed source succeeds on all three.
	const allowedSource = "phone_calls"

	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/"+allowedSource+"/cursor", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code, "GetCursor allowed source: %s", w.Body.String())

	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/sync/"+allowedSource+"/cursor", hostHeaders, map[string]any{
		"cursor":       "phone-cursor-1",
		"base_cursor":  "",
		"cursor_epoch": res.CursorEpoch,
	})
	require.Equal(t, http.StatusOK, w.Code, "CommitCursor allowed source: %s", w.Body.String())

	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+res.HostID.String()+"/sync/"+allowedSource+"/known-ids", hostHeaders, nil)
	require.Equal(t, http.StatusOK, w.Code, "KnownIDs allowed source: %s", w.Body.String())
}
