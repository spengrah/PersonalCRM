//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalenessNS builds a unique per-test namespace token so breach rows seeded by
// this test never collide with parallel tests on the shared package DB.
func stalenessNS(t *testing.T) string {
	return "syncstale-" + uuid.NewString()[:8]
}

func setupStalenessAPITest(t *testing.T) (*gin.Engine, *repository.StalenessRepository, func()) {
	t.Helper()
	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)

	// The endpoint reads through the service's ListActiveBreaches.
	svc := service.NewStalenessService(config.TestConfig().Staleness, true, syncRepo, macHostRepo, breachRepo)
	handler := handlers.NewStalenessHandler(svc)

	router := gin.New()
	v1 := router.Group("/api/v1")
	v1.GET("/sync/staleness", handler.GetActiveBreaches)

	cleanup := func() { database.Close() }
	return router, breachRepo, cleanup
}

func TestStalenessAPI_GetActiveBreaches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	router, breachRepo, cleanup := setupStalenessAPITest(t)
	defer cleanup()

	ns := stalenessNS(t)
	now := time.Now()

	// One OPEN breach for this namespace.
	openAcct := ns + "-open"
	_, err := breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
		Source:           ns,
		AccountID:        openAcct,
		BreachType:       repository.BreachTypeSyncStale,
		StaleSince:       now.Add(-30 * time.Hour),
		ThresholdSeconds: 86400,
		Details:          "no successful sync for 1d6h (threshold 24h)",
		ObservedAt:       now,
	})
	require.NoError(t, err)

	// One RESOLVED breach for this namespace — must NOT appear.
	resolvedAcct := ns + "-resolved"
	resolved, err := breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
		Source:           ns,
		AccountID:        resolvedAcct,
		BreachType:       repository.BreachTypeHeartbeat,
		StaleSince:       now.Add(-2 * time.Hour),
		ThresholdSeconds: 900,
		Details:          "no heartbeat",
		ObservedAt:       now,
	})
	require.NoError(t, err)
	_, err = breachRepo.ResolveBreach(ctx, resolved.ID, now)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/staleness", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Success bool                         `json:"success"`
		Data    []repository.StalenessBreach `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)

	// Membership assertions (shared DB → other tests' breaches may also be
	// present; assert on THIS namespace's rows only).
	var foundOpen, foundResolved bool
	for _, b := range resp.Data {
		if b.AccountID == openAcct {
			foundOpen = true
			assert.Equal(t, ns, b.Source)
			assert.Equal(t, repository.BreachTypeSyncStale, b.BreachType)
		}
		if b.AccountID == resolvedAcct {
			foundResolved = true
		}
	}
	assert.True(t, foundOpen, "the open breach must be in the response")
	assert.False(t, foundResolved, "the resolved breach must NOT be in the response")
}

func TestStalenessAPI_EmptyIsArrayNotNull(t *testing.T) {
	t.Parallel()
	router, _, cleanup := setupStalenessAPITest(t)
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/staleness", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// The data field must serialize as an array literal, never null, so the
	// frontend can iterate without a nil guard. (Other tests may have open
	// breaches on the shared DB, so we only assert the array shape, not
	// emptiness.)
	body := w.Body.String()
	assert.True(t, strings.Contains(body, `"data":[`), "data must be a JSON array, got: %s", body)
	assert.NotContains(t, body, `"data":null`)
}
