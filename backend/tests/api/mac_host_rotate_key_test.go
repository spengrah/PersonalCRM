package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

// pairFreshHost mints a token, pairs a daemon, and returns the
// resulting host id + plaintext api-key. Each call assumes the
// singleton mac_host row is empty (caller must run inside an env
// where setupMacHostEnv's t.Cleanup will hard-delete the row at
// teardown).
func pairFreshHost(t *testing.T, env *macHostTestEnv, hostname string) (uuid.UUID, string) {
	t.Helper()
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/pairing-token", map[string]string{
		"X-API-Key": env.apiKey,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "mint token: %s", w.Body.String())
	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	readData(t, w, &tokenResp)

	w = macHTTP(t, env, http.MethodPost, "/api/v1/host", nil, map[string]any{
		"pairing_token":    tokenResp.Token,
		"hostname":         hostname,
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, "pair: %s", w.Body.String())
	var pair struct {
		HostID      uuid.UUID `json:"host_id"`
		APIKey      string    `json:"api_key"`
		CursorEpoch int64     `json:"cursor_epoch"`
	}
	readData(t, w, &pair)
	return pair.HostID, pair.APIKey
}

// mintRotateToken returns a fresh pairing token (plaintext) via the
// admin mint endpoint. Tests use this to rotate against the same env.
func mintRotateToken(t *testing.T, env *macHostTestEnv) string {
	t.Helper()
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/pairing-token", map[string]string{
		"X-API-Key": env.apiKey,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "mint token: %s", w.Body.String())
	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	readData(t, w, &tokenResp)
	return tokenResp.Token
}

// hostAuthHeaders builds the bearer + host-id header pair the daemon
// endpoints expect.
func hostAuthHeaders(hostID uuid.UUID, apiKey string) map[string]string {
	return map[string]string{
		"X-Mac-Host-ID": hostID.String(),
		"Authorization": "Bearer " + apiKey,
	}
}

// spec: MAC-010[0]
func TestMacHostRotateKey_HappyPath(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-happy")
	token := mintRotateToken(t, env)

	// Snapshot pre-rotate host state so we can assert preservation.
	pre, err := env.hostRepo.GetHost(context.Background(), hostID)
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusOK, w.Code, "rotate: %s", w.Body.String())

	var res struct {
		APIKey          string    `json:"api_key"`
		APIKeyRotatedAt time.Time `json:"api_key_rotated_at"`
	}
	readData(t, w, &res)
	require.NotEmpty(t, res.APIKey, "new api_key must be non-empty")
	require.NotEqual(t, oldKey, res.APIKey, "new api_key must differ from old")
	require.False(t, res.APIKeyRotatedAt.IsZero(), "rotated_at must be set")

	// Heartbeat with NEW key succeeds.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/heartbeat",
		hostAuthHeaders(hostID, res.APIKey),
		map[string]any{
			"daemon_version":   "0.1.0",
			"protocol_version": 1,
		})
	require.Equal(t, http.StatusOK, w.Code, "heartbeat with new key: %s", w.Body.String())

	// Heartbeat with OLD key returns 401.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/heartbeat",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{
			"daemon_version":   "0.1.0",
			"protocol_version": 1,
		})
	require.Equal(t, http.StatusUnauthorized, w.Code, "old key must 401 immediately")

	// Identity preservation: id, cursor_epoch, hostname unchanged.
	post, err := env.hostRepo.GetHost(context.Background(), hostID)
	require.NoError(t, err)
	require.Equal(t, pre.ID, post.ID, "host_id preserved")
	require.Equal(t, pre.CursorEpoch, post.CursorEpoch, "cursor_epoch preserved")
	require.Equal(t, pre.Hostname, post.Hostname, "hostname preserved")
	require.NotNil(t, post.APIKeyRotatedAt, "api_key_rotated_at written")
}

// spec: MAC-010[1]
func TestMacHostRotateKey_TokenAlreadyUsed(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-reuse")
	token := mintRotateToken(t, env)

	// First rotation succeeds.
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusOK, w.Code, "first rotate: %s", w.Body.String())
	var res struct {
		APIKey string `json:"api_key"`
	}
	readData(t, w, &res)
	newKey := res.APIKey

	// Second attempt with the SAME token. Use the NEW key (the old one
	// would 401 in middleware before reaching the handler — which would
	// hide whether the token-consume invariant actually holds).
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, newKey),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusBadRequest, w.Code, "reused token: %s", w.Body.String())
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errBody))
	require.Equal(t, "TOKEN_ALREADY_USED", errBody.Error.Code)
}

func TestMacHostRotateKey_InvalidPairingToken(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-invalid")

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": "not-a-real-token"})
	require.Equal(t, http.StatusBadRequest, w.Code, "invalid token: %s", w.Body.String())
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errBody))
	require.Equal(t, "INVALID_PAIRING_TOKEN", errBody.Error.Code)
}

func TestMacHostRotateKey_TokenExpired(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-expired")

	// Seed an expired token directly. We can't mint via the real
	// CreatePairingToken because the service enforces forward-only TTL.
	plaintext := "expired-rotate-token-xyz"
	hash := sha256.Sum256([]byte(plaintext))
	_, err := env.database.Queries.SeedPairingToken(context.Background(), db.SeedPairingTokenParams{
		TokenHash: hex.EncodeToString(hash[:]),
		ExpiresAt: pgtype.Timestamptz{Time: accelerated.GetCurrentTime().Add(-1 * time.Hour), Valid: true},
	})
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": plaintext})
	require.Equal(t, http.StatusBadRequest, w.Code, "expired token: %s", w.Body.String())
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errBody))
	require.Equal(t, "TOKEN_EXPIRED", errBody.Error.Code)
}

func TestMacHostRotateKey_HostNotFound_MiddlewareCatches(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-not-found")
	token := mintRotateToken(t, env)

	// Use a random non-existent UUID in the URL path. The middleware
	// looks up the X-Mac-Host-ID first; mismatched or unknown host
	// returns 401 before the handler runs.
	unknown := uuid.New()
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+unknown.String()+"/rotate-key",
		map[string]string{
			"X-Mac-Host-ID": unknown.String(),
			"Authorization": "Bearer " + oldKey,
		},
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusUnauthorized, w.Code, "unknown host: %s", w.Body.String())

	// Pairing token NOT consumed — verify via janitor count by trying
	// a real rotation with the same token.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusOK, w.Code, "token should still be usable after middleware-rejected attempt: %s", w.Body.String())
}

// spec: MAC-007[3]
func TestMacHostRotateKey_RevokedHostRotation(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-revoked")

	// Revoke the host.
	require.NoError(t, env.macService.RevokeHost(context.Background(), hostID))

	// Mint a fresh token, attempt to rotate the revoked host.
	token := mintRotateToken(t, env)
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": token})
	// MacHostAuthMiddleware filters revoked hosts upstream → 401.
	require.Equal(t, http.StatusUnauthorized, w.Code, "revoked host: %s", w.Body.String())
}

func TestMacHostRotateKey_WrongCurrentKey(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, _ := pairFreshHost(t, env, "rotate-wrong-key")
	token := mintRotateToken(t, env)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, "definitely-not-the-real-key"),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusUnauthorized, w.Code, "wrong key: %s", w.Body.String())

	// Pairing token must still be unconsumed (middleware rejected
	// before the handler ran).
	hash := sha256.Sum256([]byte(token))
	tx, err := env.database.Pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := env.tokenRepo.GetTokenByHashForUpdateTx(context.Background(), tx, hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	require.Nil(t, row.ConsumedAt, "token must not be consumed by rejected rotate")
}

// callRotateAPIKeyDirect calls the service layer directly with the
// pre-rotation authenticated hash already captured. This bypasses
// middleware so we can deterministically exercise the CAS race: both
// callers prove they "saw" the same starting hash, exactly as
// happens when their HTTP requests both reach middleware before the
// first commit. Avoids the flaky HTTP-level race where the second
// request can be ordered AFTER the first commit at the middleware
// layer (and then correctly fail with INVALID_KEY rather than
// STALE_AUTH).
func callRotateAPIKeyDirect(
	t *testing.T,
	env *macHostTestEnv,
	hostID uuid.UUID,
	startingHash, token string,
) (*service.RotateAPIKeyResult, error) {
	t.Helper()
	return env.macService.RotateAPIKey(context.Background(), hostID, startingHash, token)
}

func TestMacHostRotateKey_ConcurrentRotation_DifferentTokens(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, _ := pairFreshHost(t, env, "rotate-concurrent-diff")
	tokenA := mintRotateToken(t, env)
	tokenB := mintRotateToken(t, env)

	// Snapshot the pre-rotation hash. Both goroutines pass THIS hash
	// as the expectedCurrentHash, modelling the in-flight HTTP case
	// where both requests' middleware saw the same starting state
	// before either committed. Bypassing middleware here is what
	// makes the test deterministic — if we used HTTP, the second
	// request could legitimately race past the first commit and get
	// INVALID_KEY at the middleware layer (a different valid
	// outcome) instead of STALE_AUTH at the service CAS check.
	pre, err := env.hostRepo.GetHost(context.Background(), hostID)
	require.NoError(t, err)
	startingHash := pre.APIKeyHash

	type rotateOutcome struct {
		res *service.RotateAPIKeyResult
		err error
	}
	results := make(chan rotateOutcome, 2)
	var wg sync.WaitGroup
	for _, tok := range []string{tokenA, tokenB} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			res, err := callRotateAPIKeyDirect(t, env, hostID, startingHash, token)
			results <- rotateOutcome{res: res, err: err}
		}(tok)
	}
	wg.Wait()
	close(results)

	successes := 0
	staleAuth := 0
	for r := range results {
		if r.err == nil {
			successes++
			require.NotNil(t, r.res)
			continue
		}
		require.ErrorIs(t, r.err, service.ErrAPIKeyStaleAuth,
			"loser must fail CAS (ErrAPIKeyStaleAuth), got %v", r.err)
		staleAuth++
	}
	require.Equal(t, 1, successes, "exactly one rotation must succeed")
	require.Equal(t, 1, staleAuth, "exactly one rotation must fail STALE_AUTH")

	// Verify the loser's token is NOT consumed by inspecting the DB.
	consumedCount := 0
	for _, tok := range []string{tokenA, tokenB} {
		hash := sha256.Sum256([]byte(tok))
		tx, err := env.database.Pool.Begin(context.Background())
		require.NoError(t, err)
		row, err := env.tokenRepo.GetTokenByHashForUpdateTx(context.Background(), tx, hex.EncodeToString(hash[:]))
		require.NoError(t, err)
		if row.ConsumedAt != nil {
			consumedCount++
		}
		require.NoError(t, tx.Rollback(context.Background()))
	}
	require.Equal(t, 1, consumedCount, "exactly one token must be consumed")
}

func TestMacHostRotateKey_ConcurrentRotation_SameToken(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, _ := pairFreshHost(t, env, "rotate-concurrent-same")
	token := mintRotateToken(t, env)

	// Two parallel rotations with the SAME single token. The service
	// orders row-lock → CAS check → token check. The loser ALWAYS
	// fails CAS first, so the rejection is ErrAPIKeyStaleAuth (NOT
	// ErrPairingTokenAlreadyUsed). Asserting the exact error locks
	// down the ordering invariant. See callRotateAPIKeyDirect for
	// why this test bypasses HTTP.
	pre, err := env.hostRepo.GetHost(context.Background(), hostID)
	require.NoError(t, err)
	startingHash := pre.APIKeyHash

	type rotateOutcome struct {
		res *service.RotateAPIKeyResult
		err error
	}
	results := make(chan rotateOutcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := callRotateAPIKeyDirect(t, env, hostID, startingHash, token)
			results <- rotateOutcome{res: res, err: err}
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	staleAuth := 0
	for r := range results {
		if r.err == nil {
			successes++
			continue
		}
		require.ErrorIs(t, r.err, service.ErrAPIKeyStaleAuth,
			"loser must fail CAS (ErrAPIKeyStaleAuth) before reaching token-consume, got %v", r.err)
		staleAuth++
	}
	require.Equal(t, 1, successes, "exactly one rotation must succeed")
	require.Equal(t, 1, staleAuth, "exactly one rotation must fail STALE_AUTH")

	// The token IS consumed (by the winner, before the loser's CAS).
	hash := sha256.Sum256([]byte(token))
	tx, err := env.database.Pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := env.tokenRepo.GetTokenByHashForUpdateTx(context.Background(), tx, hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	require.NotNil(t, row.ConsumedAt, "token must be consumed by winner")
}

func TestMacHostRotateKey_OldKeyImmediatelyInvalid(t *testing.T) {

	env := setupMacHostEnv(t)
	hostID, oldKey := pairFreshHost(t, env, "rotate-old-invalid")
	token := mintRotateToken(t, env)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/rotate-key",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{"pairing_token": token})
	require.Equal(t, http.StatusOK, w.Code, "rotate: %s", w.Body.String())

	// Immediately heartbeat with the OLD key — no sleep, no
	// middleware-cache refresh. Must 401.
	w = macHTTP(t, env, http.MethodPost, "/api/v1/host/"+hostID.String()+"/heartbeat",
		hostAuthHeaders(hostID, oldKey),
		map[string]any{
			"daemon_version":   "0.1.0",
			"protocol_version": 1,
		})
	require.Equal(t, http.StatusUnauthorized, w.Code,
		"old key must be invalid AT COMMIT, not 'eventually'; body=%s", w.Body.String())
}
