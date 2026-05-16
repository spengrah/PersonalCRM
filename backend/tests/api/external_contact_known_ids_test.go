package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// GET /api/v1/host/:id/sync/:source/known-ids integration tests
//
// These exercise the real router → middleware → service → repository
// chain against a live Postgres so the wire shape, auth contract, and
// host-scoped filter are all locked.
// ----------------------------------------------------------------------------

type knownIDsByHostEnv struct {
	router        *gin.Engine
	database      *db.Database
	macService    *service.MacHostService
	externalRepo  *repository.ExternalContactRepository
	pairedHostID  uuid.UUID
	pairedHostKey string
	// sourceIDPrefix scopes per-test cleanup so parallel runs don't
	// stomp each other under source='icloud_contacts'.
	sourceIDPrefix string
}

func setupKnownIDsByHostEnv(t *testing.T) *knownIDsByHostEnv {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.External.APIKey = macHostTestKey

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, externalRepo, database.Pool, 4)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))

	limiter := auth.NewPairingIPRateLimiter()
	macHandler := handlers.NewMacHostHandler(macService, limiter)
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    hostRepo,
		Handler:     macHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})

	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "known-ids-by-host-test", "0.1.0", 1)
	require.NoError(t, err)

	suffix := uuid.NewString()[:8]
	env := &knownIDsByHostEnv{
		router:         router,
		database:       database,
		macService:     macService,
		externalRepo:   externalRepo,
		pairedHostID:   pair.HostID,
		pairedHostKey:  pair.APIKey,
		sourceIDPrefix: "known-ids-" + suffix + "-",
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Hard-delete every external_contact row seeded under our
		// per-test source_id prefix (covers live and tombstoned).
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, env.sourceIDPrefix)
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		database.Close()
	})
	return env
}

// seedExternalContact inserts a live external_contact row via the
// repository's UpsertTx so the new columns (host_id, last_content_hash)
// are populated. Returns the seeded row's source_id.
func (e *knownIDsByHostEnv) seedExternalContact(
	t *testing.T, hostID uuid.UUID, source, entityID string, hash *string,
) string {
	t.Helper()
	ctx := context.Background()
	sourceID := e.sourceIDPrefix + entityID
	syncedAt := accelerated.GetCurrentTime()
	tx, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = e.externalRepo.UpsertTx(ctx, tx, repository.UpsertExternalContactRequest{
		Source:          source,
		SourceID:        sourceID,
		HostID:          &hostID,
		LastContentHash: hash,
		SyncedAt:        &syncedAt,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	return sourceID
}

func (e *knownIDsByHostEnv) softDeleteRow(t *testing.T, source, sourceID string) {
	t.Helper()
	ctx := context.Background()
	row, err := e.externalRepo.GetBySource(ctx, source, sourceID, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "row %s should exist before soft-delete", sourceID)
	tx, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	require.NoError(t, e.externalRepo.SoftDeleteTx(ctx, tx, row.ID))
	require.NoError(t, tx.Commit(ctx))
}

func getKnownIDsByHost(t *testing.T, env *knownIDsByHostEnv, urlHostID, headerHostID, hostKey, source string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/"+urlHostID+"/sync/"+source+"/known-ids", nil)
	if headerHostID != "" {
		req.Header.Set("X-Mac-Host-ID", headerHostID)
	}
	if hostKey != "" {
		req.Header.Set("Authorization", "Bearer "+hostKey)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

type knownIDsByHostEntry struct {
	SourceID        string  `json:"source_id"`
	LastContentHash *string `json:"last_content_hash"`
}

type knownIDsByHostResp struct {
	Success bool `json:"success"`
	Data    struct {
		IDs []knownIDsByHostEntry `json:"ids"`
	} `json:"data"`
}

func parseKnownIDsByHostResp(t *testing.T, w *httptest.ResponseRecorder) knownIDsByHostResp {
	t.Helper()
	var r knownIDsByHostResp
	require.NoError(t, json.NewDecoder(w.Body).Decode(&r), "body: %s", w.Body.String())
	return r
}

func TestKnownIDsByHost_Auth_Missing_401(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), "", "icloud_contacts")
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

func TestKnownIDsByHost_Auth_WrongHostID_403(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	other := uuid.New().String()
	// Valid bearer + X-Mac-Host-ID, but URL :id differs — middleware
	// enforces the consistency check.
	w := getKnownIDsByHost(t, env, other, env.pairedHostID.String(), env.pairedHostKey, "icloud_contacts")
	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

func TestKnownIDsByHost_UnknownSource_400(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "bogus")
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

func TestKnownIDsByHost_EmptyDB_ReturnsEmptyArray(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "icloud_contacts")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)
	require.True(t, r.Success)
	require.NotNil(t, r.Data.IDs, "ids must serialize as `[]`, never null")
	// Other tests may have left their rows behind; the per-test
	// source_id prefix scopes our expectation. Filter to only rows we
	// seeded (in this test we seeded none).
	for _, entry := range r.Data.IDs {
		require.NotContains(t, entry.SourceID, env.sourceIDPrefix,
			"empty-DB test must not see its own seeded rows")
	}
}

func TestKnownIDsByHost_ReturnsLiveRows(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	h1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	h3 := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	sid1 := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "alpha", &h1)
	sid2 := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "beta", &h2)
	sid3 := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "gamma", &h3)

	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "icloud_contacts")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)
	require.True(t, r.Success)

	// Build a lookup over only OUR seeded rows.
	mine := map[string]*string{}
	for _, entry := range r.Data.IDs {
		if _, ok := map[string]struct{}{sid1: {}, sid2: {}, sid3: {}}[entry.SourceID]; ok {
			mine[entry.SourceID] = entry.LastContentHash
		}
	}
	require.Len(t, mine, 3, "all three seeded live rows must be returned")
	require.NotNil(t, mine[sid1])
	require.Equal(t, h1, *mine[sid1])
	require.NotNil(t, mine[sid2])
	require.Equal(t, h2, *mine[sid2])
	require.NotNil(t, mine[sid3])
	require.Equal(t, h3, *mine[sid3])
}

func TestKnownIDsByHost_ExcludesTombstonedRows(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	h1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	h3 := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	sid1 := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "live-a", &h1)
	sid2 := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "live-b", &h2)
	tomb := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "tombstoned", &h3)
	env.softDeleteRow(t, "icloud_contacts", tomb)

	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "icloud_contacts")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)

	mine := map[string]bool{}
	for _, entry := range r.Data.IDs {
		mine[entry.SourceID] = true
	}
	require.True(t, mine[sid1], "live row sid1 must appear")
	require.True(t, mine[sid2], "live row sid2 must appear")
	require.False(t, mine[tomb], "tombstoned row must NOT appear in known-ids")
}

func TestKnownIDsByHost_HostScoped(t *testing.T) {
	// Pair host A, seed two rows under A. Revoke A. Pair host B, seed
	// one row under B. B's /known-ids must return only B's row — A's
	// rows survive (no cascade per plan D-JC1) but they're owned by A
	// and the partial filter excludes them from B's view.
	env := setupKnownIDsByHostEnv(t)
	ctx := context.Background()

	// Host A is already paired by setup; treat env.pairedHostID as A.
	hA := env.pairedHostID
	h1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	sid1A := env.seedExternalContact(t, hA, "icloud_contacts", "a-1", &h1)
	sid2A := env.seedExternalContact(t, hA, "icloud_contacts", "a-2", &h2)

	// Revoke A so B can pair (idx_mac_host_singleton allows only one
	// non-revoked host).
	require.NoError(t, env.macService.RevokeHost(ctx, hA))

	// Pair host B.
	plainB, _, err := env.macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pairB, err := env.macService.PairWithToken(ctx, plainB, "host-b", "0.1.0", 1)
	require.NoError(t, err)
	h3 := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	sid1B := env.seedExternalContact(t, pairB.HostID, "icloud_contacts", "b-1", &h3)

	// Query as B.
	w := getKnownIDsByHost(t, env, pairB.HostID.String(), pairB.HostID.String(), pairB.APIKey, "icloud_contacts")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)

	mine := map[string]bool{}
	for _, entry := range r.Data.IDs {
		mine[entry.SourceID] = true
	}
	require.True(t, mine[sid1B], "host B's own row must appear")
	require.False(t, mine[sid1A], "host A's row must NOT appear in host B's known-ids")
	require.False(t, mine[sid2A], "host A's row must NOT appear in host B's known-ids")
}

func TestKnownIDsByHost_NullLastContentHashSerializesAsNull(t *testing.T) {
	env := setupKnownIDsByHostEnv(t)
	// Legacy row simulation: no last_content_hash on the upsert.
	sid := env.seedExternalContact(t, env.pairedHostID, "icloud_contacts", "legacy", nil)

	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "icloud_contacts")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)

	found := false
	for _, entry := range r.Data.IDs {
		if entry.SourceID == sid {
			found = true
			require.Nil(t, entry.LastContentHash, "legacy row's last_content_hash must serialize as null")
		}
	}
	require.True(t, found, "seeded legacy row must appear in response")
}

func TestKnownIDsByHost_NonExternalContactSource_ReturnsEmpty(t *testing.T) {
	// `messages` is an allowed push source but has no external_contact
	// rows. The query filter on source returns an empty array.
	env := setupKnownIDsByHostEnv(t)
	w := getKnownIDsByHost(t, env, env.pairedHostID.String(), env.pairedHostID.String(), env.pairedHostKey, "messages")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsByHostResp(t, w)
	require.NotNil(t, r.Data.IDs)
	// Filter to only the rows we seeded (none in this test).
	for _, entry := range r.Data.IDs {
		require.NotContains(t, entry.SourceID, env.sourceIDPrefix,
			"messages source should not contain any of our seeded icloud rows")
	}
}
