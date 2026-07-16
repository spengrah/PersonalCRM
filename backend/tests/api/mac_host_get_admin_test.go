package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// hostDetailView mirrors the MacHostView fields this test asserts on.
type hostDetailView struct {
	ID              uuid.UUID  `json:"id"`
	Hostname        string     `json:"hostname"`
	APIKeyRevokedAt *time.Time `json:"api_key_revoked_at,omitempty"`
}

// TestMacHost_GetAdmin_DetailView covers the admin host-detail read:
// a live host is returned, a revoked host is still returned (the admin
// view does not filter revocation), and an unknown id is 404.
// spec: MAC-018[1]
func TestMacHost_GetAdmin_DetailView(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()
	headers := map[string]string{"X-API-Key": env.apiKey}

	// Live host (the singleton index allows one live host at a time).
	liveName := "get-admin-live-" + uuid.NewString()[:8]
	live, err := env.hostRepo.SeedHostForTest(ctx, liveName, "0.1.0", 1, "$2a$04$live", nil, nil)
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+live.ID.String(), headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var view hostDetailView
	readData(t, w, &view)
	require.Equal(t, live.ID, view.ID)
	require.Equal(t, liveName, view.Hostname)
	require.Nil(t, view.APIKeyRevokedAt, "live host must not carry a revocation timestamp")

	// Revoked host: the detail view returns it rather than 404ing.
	revokedName := "get-admin-revoked-" + uuid.NewString()[:8]
	revoked, err := env.hostRepo.SeedRevokedHostForTest(ctx, revokedName, "0.1.0", 1, "$2a$04$rvkd")
	require.NoError(t, err)

	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+revoked.ID.String(), headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	view = hostDetailView{}
	readData(t, w, &view)
	require.Equal(t, revoked.ID, view.ID)
	require.Equal(t, revokedName, view.Hostname)
	require.NotNil(t, view.APIKeyRevokedAt, "revoked host must carry its revocation timestamp")

	// Unknown id is not-found.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+uuid.New().String(), headers, nil)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestMacHost_GetAdmin_BadID_400 confirms the handler rejects a
// non-UUID path param at validation before touching the service.
func TestMacHost_GetAdmin_BadID_400(t *testing.T) {

	env := setupMacHostEnv(t)
	headers := map[string]string{"X-API-Key": env.apiKey}
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/not-a-uuid", headers, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// TestMacHost_GetAdmin_Unauthenticated_401 covers the admin-auth path:
// without the global API key, the detail read is rejected.
func TestMacHost_GetAdmin_Unauthenticated_401(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()
	host, err := env.hostRepo.SeedRevokedHostForTest(ctx,
		"get-admin-noauth-"+uuid.NewString()[:8], "0.0.0", 1, "$2a$04$nA")
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+host.ID.String(), nil, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}
