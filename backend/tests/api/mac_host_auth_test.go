package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMacHost_Auth_WrongAuthOnDaemonRoute(t *testing.T) {

	env := setupMacHostEnv(t)

	// Daemon route with global API key (no X-Mac-Host-ID) → 401.
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+uuidNew()+"/heartbeat", map[string]string{
		"X-API-Key": env.apiKey,
	}, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusUnauthorized, w.Code, "global API key cannot auth daemon routes; body: %s", w.Body.String())
}

func TestMacHost_Auth_AmbiguousAuth_400(t *testing.T) {

	env := setupMacHostEnv(t)

	// Pair a host so we have a valid id/key for the daemon side.
	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "host", "0.1.0", 1)
	require.NoError(t, err)

	headers := map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
		"X-API-Key":     env.apiKey,
	}
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/heartbeat", headers, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMacHost_Auth_AdminRouteHostAuth_401(t *testing.T) {

	env := setupMacHostEnv(t)

	// Pair a host.
	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "host", "0.1.0", 1)
	require.NoError(t, err)

	// Try to hit the admin list with host auth → 401 because the v1
	// group requires the global API key.
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host", map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMacHost_Auth_IDParamMismatch_403(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "host", "0.1.0", 1)
	require.NoError(t, err)

	// Use the daemon's correct credentials but a different :id on the
	// URL → 403.
	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+uuidNew()+"/heartbeat", map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 1,
	})
	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

func TestMacHost_Auth_MinProtocolVersion_412(t *testing.T) {

	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(context.Background())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(context.Background(), plain, "host", "0.1.0", 1)
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodPost, "/api/v1/host/"+res.HostID.String()+"/heartbeat", map[string]string{
		"X-Mac-Host-ID": res.HostID.String(),
		"Authorization": "Bearer " + res.APIKey,
	}, map[string]any{
		"daemon_version":   "0.1.0",
		"protocol_version": 0, // below floor
	})
	require.Equal(t, http.StatusPreconditionFailed, w.Code, "body: %s", w.Body.String())
	var body map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "body: %v", body)
	require.Equal(t, "UPGRADE_REQUIRED", errObj["code"])
}

func uuidNew() string {
	return uuid.NewString()
}
