//go:build integration_testdb

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthRepository_RiverQueries pins the three production river_job reads
// against an isolated clone (DB-wide river_job counts would collide with other
// tests' jobs on the shared package DB). All timestamps are caller-supplied via
// InsertRiverJobForTest so the count/age/latest-completed discrimination is
// deterministic.
func TestHealthRepository_RiverQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewHealthRepository(database.Queries)

	now := accelerated.GetCurrentTime()
	watchdogKind := scheduler.StalenessWatchdogArgs{}.Kind()

	// --- Empty queue: count 0, nil oldest-due, nil latest-completed. ---
	count, err := repo.CountDiscardedRiverJobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	oldest, err := repo.OldestDueRiverJobScheduledAt(ctx, now)
	require.NoError(t, err)
	assert.Nil(t, oldest, "no due jobs → nil")

	latest, err := repo.LatestCompletedRiverJobByKind(ctx, watchdogKind)
	require.NoError(t, err)
	assert.Nil(t, latest, "no completed watchdog job → nil")

	// --- One discarded job → count 1. ---
	require.NoError(t, repo.InsertRiverJobForTest(ctx, "some_kind", db.RiverJobStateDiscarded, now, ptrTime(now)))
	count, err = repo.CountDiscardedRiverJobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// --- An available job scheduled 2h in the past → oldest-due age ≈ 2h. ---
	twoHoursAgo := now.Add(-2 * time.Hour)
	require.NoError(t, repo.InsertRiverJobForTest(ctx, "available_kind", db.RiverJobStateAvailable, twoHoursAgo, nil))
	oldest, err = repo.OldestDueRiverJobScheduledAt(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, oldest)
	assert.WithinDuration(t, twoHoursAgo.UTC(), oldest.UTC(), time.Second)

	// --- A retryable job scheduled in the FUTURE → excluded from oldest-due
	// (its backoff has not elapsed), so the 2h-past available row still wins. ---
	future := now.Add(1 * time.Hour)
	require.NoError(t, repo.InsertRiverJobForTest(ctx, "retry_kind", db.RiverJobStateRetryable, future, nil))
	oldest, err = repo.OldestDueRiverJobScheduledAt(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, oldest)
	assert.WithinDuration(t, twoHoursAgo.UTC(), oldest.UTC(), time.Second,
		"future-scheduled retryable must not become the oldest-due")
}

// TestHealthRepository_LatestCompletedByKind pins the watchdog-liveness query:
// the newest COMPLETED row of the target kind wins, ignoring other kinds,
// non-completed states of the target kind, and a later-finalized OTHER kind.
func TestHealthRepository_LatestCompletedByKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewHealthRepository(database.Queries)

	now := accelerated.GetCurrentTime()
	watchdogKind := scheduler.StalenessWatchdogArgs{}.Kind()

	older := now.Add(-3 * time.Hour)
	newer := now.Add(-1 * time.Hour)
	newest := now.Add(-10 * time.Minute)

	// Two COMPLETED watchdog rows hours apart → the newer finalized_at wins.
	require.NoError(t, repo.InsertRiverJobForTest(ctx, watchdogKind, db.RiverJobStateCompleted, older, ptrTime(older)))
	require.NoError(t, repo.InsertRiverJobForTest(ctx, watchdogKind, db.RiverJobStateCompleted, newer, ptrTime(newer)))

	// A NON-completed watchdog row finalized even more recently must be ignored
	// (the model: a discarded/failed run must not look like a healthy run).
	require.NoError(t, repo.InsertRiverJobForTest(ctx, watchdogKind, db.RiverJobStateDiscarded, newest, ptrTime(newest)))

	// A DIFFERENT kind, completed and finalized most recently, must be ignored.
	require.NoError(t, repo.InsertRiverJobForTest(ctx, "other_kind", db.RiverJobStateCompleted, newest, ptrTime(newest)))

	latest, err := repo.LatestCompletedRiverJobByKind(ctx, watchdogKind)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.WithinDuration(t, newer.UTC(), latest.UTC(), time.Second,
		"newest COMPLETED watchdog row wins; non-completed + other-kind rows ignored")
}

// TestHealth_EndToEnd_ReadinessAndPIIGuard wires the real HealthRepository and a
// real StalenessService through the handler on one clone. It seeds an open
// breach with an email-shaped account_id + sensitive details and a FRESH
// completed watchdog row (so the freshness guard passes), then asserts:
//   - bare /health → 200 + top-level healthy (liveness contract intact),
//   - ?ready=1 → 503 + degraded with the right river/sync details,
//   - the readiness body leaks neither the account email nor the breach details
//     (full-stack PII guard on the unauthenticated route).
func TestHealth_EndToEnd_ReadinessAndPIIGuard(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)
	healthRepo := repository.NewHealthRepository(database.Queries)
	stalenessService := service.NewStalenessService(cfg.Staleness, true, syncRepo, macHostRepo, breachRepo)

	now := accelerated.GetCurrentTime()
	watchdogKind := scheduler.StalenessWatchdogArgs{}.Kind()

	// Open one breach with realistic PII-bearing fields.
	const secretEmail = "private.person@example.com"
	const secretDetails = "oauth refresh failed for private.person@example.com"
	_, err := breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
		Source:           "gcontacts",
		AccountID:        secretEmail,
		BreachType:       repository.BreachTypeSyncError,
		StaleSince:       now.Add(-3 * time.Hour),
		ThresholdSeconds: 86400,
		Details:          secretDetails,
		ObservedAt:       now,
	})
	require.NoError(t, err)

	// A FRESH completed watchdog row so the sync freshness guard passes (sync
	// reports degraded for the breach, not unknown for a stale watchdog).
	require.NoError(t, healthRepo.InsertRiverJobForTest(ctx, watchdogKind, db.RiverJobStateCompleted, now, ptrTime(now)))

	thresholds := health.Thresholds{
		RiverDiscardedMax:  cfg.Health.RiverDiscardedMax,
		RiverOldestDueMax:  cfg.Health.RiverOldestDueMax,
		SyncWatchdogMaxAge: cfg.Health.SyncWatchdogMaxAge,
		DiskPath:           cfg.Health.DiskPath,
		DiskMinFreePercent: 0, // disable disk floor so CI disk fullness can't flake this
	}
	hc := health.NewHealthChecker(database, cfg.Database.HealthTimeout, health.Deps{
		River:            healthRepo,
		Staleness:        stalenessService,
		SyncWatchdogKind: watchdogKind,
		Thresholds:       thresholds,
	})

	router := gin.New()
	router.GET("/health", hc.Handler)

	// --- Bare /health: liveness, 200 + healthy (the breach must NOT flip it). ---
	bareResp := doIntegrationHealthRequest(t, router, "/health")
	assert.Equal(t, http.StatusOK, bareResp.code)
	assert.Equal(t, "healthy", bareResp.body.Status)
	assert.Equal(t, "liveness", bareResp.body.Probe)

	// --- ?ready=1: readiness, 503 + degraded; sync degraded by the breach. ---
	readyResp := doIntegrationHealthRequest(t, router, "/health?ready=1")
	assert.Equal(t, http.StatusServiceUnavailable, readyResp.code)
	assert.Equal(t, "degraded", readyResp.body.Status)
	assert.Equal(t, "readiness", readyResp.body.Probe)
	assert.Equal(t, "degraded", readyResp.body.Components["sync"].Status)
	assert.Equal(t, "healthy", readyResp.body.Components["river"].Status)

	// --- Full-stack PII guard: the unauthenticated body must carry neither the
	// account email nor the breach details. ---
	assert.NotContains(t, readyResp.raw, secretEmail, "account email must not leak into /health")
	assert.NotContains(t, readyResp.raw, secretDetails, "breach details must not leak into /health")
	assert.Contains(t, readyResp.raw, "active_breach_count")
}

type integrationHealthResult struct {
	code int
	raw  string
	body health.HealthResponse
}

func doIntegrationHealthRequest(t *testing.T, router *gin.Engine, url string) integrationHealthResult {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var body health.HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return integrationHealthResult{code: w.Code, raw: w.Body.String(), body: body}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
