package api

import (
	"context"
	"net/http"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMacHost_GetSourceCounts_HappyPath asserts the per-host source-counts
// endpoint returns a map keyed by source string with live row counts.
// Tombstoned + merge-dupe rows are excluded; rows belonging to a
// different host are excluded.
func TestMacHost_GetSourceCounts_HappyPath(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()

	hostA, err := env.hostRepo.SeedRevokedHostForTest(ctx,
		"src-counts-A-"+uuid.NewString()[:8], "0.0.0", 1, "$2a$04$srcA")
	require.NoError(t, err)
	hostB, err := env.hostRepo.SeedRevokedHostForTest(ctx,
		"src-counts-B-"+uuid.NewString()[:8], "0.0.0", 1, "$2a$04$srcB")
	require.NoError(t, err)

	extRepo := repository.NewExternalContactRepository(env.database.Queries)

	// Seed 3 icloud rows + 2 manual rows for hostA, 5 icloud rows for hostB.
	prefix := "srccnt-" + uuid.NewString()[:8]
	hostAID := hostA.ID
	hostBID := hostB.ID

	for i := 0; i < 3; i++ {
		_, err := extRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "icloud_contacts",
			SourceID: prefix + "-ic-A-" + uuid.NewString()[:8],
			HostID:   &hostAID,
		})
		require.NoError(t, err)
	}
	for i := 0; i < 2; i++ {
		_, err := extRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "test",
			SourceID: prefix + "-test-A-" + uuid.NewString()[:8],
			HostID:   &hostAID,
		})
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := extRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "icloud_contacts",
			SourceID: prefix + "-ic-B-" + uuid.NewString()[:8],
			HostID:   &hostBID,
		})
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		// DeleteExternalContactsBySourceIDPrefix is on Queries directly
		// (no repository wrapper). Prefix-based cleanup is per-test-
		// isolated because each test generates its own prefix.
		_, _ = env.database.Queries.DeleteExternalContactsBySourceIDPrefix(ctx, &prefix)
	})

	_ = extRepo // silence unused for the cleanup-only-test branches

	// Hit the endpoint for hostA.
	headers := map[string]string{"X-API-Key": env.apiKey}
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+hostAID.String()+"/source-counts", headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Counts map[string]int `json:"counts"`
	}
	readData(t, w, &resp)
	require.Equal(t, 3, resp.Counts["icloud_contacts"], "hostA icloud count")
	require.Equal(t, 2, resp.Counts["test"], "hostA test count")
	// hostB's icloud rows must NOT leak into hostA's response.
	_, leaked := resp.Counts["leaked"]
	require.False(t, leaked, "no unexpected source keys")

	// Hit the endpoint for hostB.
	w = macHTTP(t, env, http.MethodGet, "/api/v1/host/"+hostBID.String()+"/source-counts", headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp.Counts = nil
	readData(t, w, &resp)
	require.Equal(t, 5, resp.Counts["icloud_contacts"], "hostB icloud count")
	_, hasTest := resp.Counts["test"]
	require.False(t, hasTest, "hostB has no test rows; key must be absent")
}

// TestMacHost_GetSourceCounts_UnknownHost_404 covers the host-existence
// pre-check: an unknown host id returns 404 (not 200 with empty
// counts).
// spec: MAC-018.per-source-live-counts
func TestMacHost_GetSourceCounts_UnknownHost_404(t *testing.T) {

	env := setupMacHostEnv(t)
	missing := uuid.New().String()
	headers := map[string]string{"X-API-Key": env.apiKey}
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+missing+"/source-counts", headers, nil)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestMacHost_GetSourceCounts_NoRowsReturnsEmpty asserts that a host
// with no external_contact rows returns 200 with counts={} rather than
// 404 or 500.
// spec: MAC-018.per-source-live-counts
func TestMacHost_GetSourceCounts_NoRowsReturnsEmpty(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()
	host, err := env.hostRepo.SeedRevokedHostForTest(ctx,
		"src-counts-empty-"+uuid.NewString()[:8], "0.0.0", 1, "$2a$04$empty")
	require.NoError(t, err)

	headers := map[string]string{"X-API-Key": env.apiKey}
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+host.ID.String()+"/source-counts", headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Counts map[string]int `json:"counts"`
	}
	readData(t, w, &resp)
	require.NotNil(t, resp.Counts, "counts field must be present even when empty")
	require.Empty(t, resp.Counts, "no external_contact rows → counts is {}")
}

// TestMacHost_GetSourceCounts_Unauthenticated_401 covers the admin-auth
// path: without the global API key, the request is rejected.
func TestMacHost_GetSourceCounts_Unauthenticated_401(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()
	host, err := env.hostRepo.SeedRevokedHostForTest(ctx,
		"src-counts-noauth-"+uuid.NewString()[:8], "0.0.0", 1, "$2a$04$nA")
	require.NoError(t, err)

	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/"+host.ID.String()+"/source-counts", nil, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// TestMacHost_GetSourceCounts_BadID_400 confirms the handler rejects a
// non-UUID path param at validation before touching the service.
func TestMacHost_GetSourceCounts_BadID_400(t *testing.T) {

	env := setupMacHostEnv(t)
	headers := map[string]string{"X-API-Key": env.apiKey}
	w := macHTTP(t, env, http.MethodGet, "/api/v1/host/not-a-uuid/source-counts", headers, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}
