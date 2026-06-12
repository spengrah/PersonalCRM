package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRiverStatsReader is a configurable health.RiverStatsReader for unit
// tests. Each field overrides one method's return; the *err fields force an
// error from that method.
type mockRiverStatsReader struct {
	discarded      int64
	discardedErr   error
	oldestDue      *time.Time
	oldestDueErr   error
	latestComplete *time.Time
	latestErr      error
}

func (m *mockRiverStatsReader) CountDiscardedRiverJobs(ctx context.Context) (int64, error) {
	return m.discarded, m.discardedErr
}

func (m *mockRiverStatsReader) OldestDueRiverJobScheduledAt(ctx context.Context, now time.Time) (*time.Time, error) {
	return m.oldestDue, m.oldestDueErr
}

func (m *mockRiverStatsReader) LatestCompletedRiverJobByKind(ctx context.Context, kind string) (*time.Time, error) {
	return m.latestComplete, m.latestErr
}

// mockStalenessReader is a configurable health.StalenessReader for unit tests.
type mockStalenessReader struct {
	breaches []repository.StalenessBreach
	err      error
}

func (m *mockStalenessReader) ListActiveBreaches(ctx context.Context) ([]repository.StalenessBreach, error) {
	return m.breaches, m.err
}

// okDiskUsage returns a disk-usage func reporting freePercent free space over a
// 100-byte total (so free bytes == freePercent). errDisk forces a statfs error.
func okDiskUsage(freePercent uint64) health.DiskUsageFunc {
	return func(string) (uint64, uint64, error) {
		return 100, freePercent, nil
	}
}

func errDiskUsage(err error) health.DiskUsageFunc {
	return func(string) (uint64, uint64, error) {
		return 0, 0, err
	}
}

// defaultTestThresholds mirrors config.HealthConfig defaults but disables the
// disk floor so CI disk fullness can't flake the component tests; callers that
// exercise the floor override DiskMinFreePercent.
func defaultTestThresholds() health.Thresholds {
	return health.Thresholds{
		RiverDiscardedMax:  0,
		RiverOldestDueMax:  30 * time.Minute,
		SyncWatchdogMaxAge: 30 * time.Minute,
		DiskPath:           "/",
		DiskMinFreePercent: 0,
	}
}

// freshWatchdogReader returns a river reader whose latest-completed watchdog
// trail is recent (so the sync freshness guard passes), with the given breach
// reader semantics handled separately.
func freshWatchdogReader() *mockRiverStatsReader {
	now := time.Now()
	return &mockRiverStatsReader{latestComplete: &now}
}

// doHealthRequest serves one GET /health request (optionally as readiness) and
// returns the recorder + parsed response.
func doHealthRequest(t *testing.T, hc *health.HealthChecker, ready bool) (*httptest.ResponseRecorder, health.HealthResponse) {
	t.Helper()
	router := gin.New()
	router.GET("/health", hc.Handler)

	url := "/health"
	if ready {
		url = "/health?ready=1"
	}
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp health.HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return w, resp
}

func TestHealthComponents_AllHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            freshWatchdogReader(),
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	// Bare /health: liveness, 200 + healthy.
	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "healthy", resp.Status)
	assert.Equal(t, "liveness", resp.Probe)
	for _, key := range []string{"database", "river", "sync", "disk"} {
		assert.Contains(t, resp.Components, key)
		assert.Equal(t, "healthy", resp.Components[key].Status, "component %s", key)
	}

	// ?ready=1: readiness, 200 + healthy.
	wr, respr := doHealthRequest(t, hc, true)
	assert.Equal(t, http.StatusOK, wr.Code)
	assert.Equal(t, "healthy", respr.Status)
	assert.Equal(t, "readiness", respr.Probe)
}

func TestHealthComponents_RiverDiscardedDegrades(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	river := freshWatchdogReader()
	river.discarded = 1
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	// Bare stays 200 + top-level healthy; river component itself is degraded.
	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "healthy", resp.Status)
	assert.Equal(t, "degraded", resp.Components["river"].Status)

	// Readiness flips to 503 + degraded.
	wr, respr := doHealthRequest(t, hc, true)
	assert.Equal(t, http.StatusServiceUnavailable, wr.Code)
	assert.Equal(t, "degraded", respr.Status)
}

func TestHealthComponents_RiverDiscardedToleranceKnob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	river := freshWatchdogReader()
	river.discarded = 1
	thresholds := defaultTestThresholds()
	thresholds.RiverDiscardedMax = 1 // tolerate exactly one (strictly-greater)
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       thresholds,
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "healthy", resp.Components["river"].Status)
}

func TestHealthComponents_RiverOldestDueBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}

	cases := []struct {
		name       string
		ageMinutes time.Duration
		wantStatus string
	}{
		{"under threshold", 29 * time.Minute, "healthy"},
		{"exactly threshold", 30 * time.Minute, "healthy"}, // strictly-greater
		{"over threshold", 31 * time.Minute, "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			due := now.Add(-tc.ageMinutes)
			river := freshWatchdogReader()
			river.oldestDue = &due
			hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
				River:            river,
				Staleness:        &mockStalenessReader{},
				SyncWatchdogKind: "sync_staleness_watchdog",
				DiskUsage:        okDiskUsage(50),
				Thresholds:       defaultTestThresholds(),
			})
			_, resp := doHealthRequest(t, hc, true)
			assert.Equal(t, tc.wantStatus, resp.Components["river"].Status)
		})
	}
}

func TestHealthComponents_RiverNoDueJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	river := freshWatchdogReader() // oldestDue nil
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "healthy", resp.Components["river"].Status)
	// oldest_due_age_seconds must be ABSENT (nil pointer), not 0.
	raw := marshalDetails(t, resp.Components["river"].Details)
	assert.NotContains(t, raw, "oldest_due_age_seconds")
	assert.Contains(t, raw, "discarded_count")
}

func TestHealthComponents_RiverDueExactlyNowAgeZeroPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	now := time.Now()
	river := freshWatchdogReader()
	river.oldestDue = &now // age ~0
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	// age 0 must be PRESENT (pointer set), distinguishing "due now" from "none".
	raw := marshalDetails(t, resp.Components["river"].Details)
	assert.Contains(t, raw, "oldest_due_age_seconds")
}

func TestHealthComponents_RiverOldestDueDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	now := time.Now()
	old := now.Add(-10 * time.Hour)
	river := freshWatchdogReader()
	river.oldestDue = &old
	thresholds := defaultTestThresholds()
	thresholds.RiverOldestDueMax = 0 // disables the age check
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       thresholds,
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "healthy", resp.Components["river"].Status)
}

func TestHealthComponents_RiverQueryErrorUnhealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	river := freshWatchdogReader()
	river.discardedErr = errors.New("boom query")
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	// Endpoint still answers (never 500); river is unhealthy.
	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusOK, w.Code) // bare liveness unaffected
	assert.Equal(t, "unhealthy", resp.Components["river"].Status)
	require.NotNil(t, resp.Components["river"].Error)
	assert.Contains(t, *resp.Components["river"].Error, "boom query")
}

func TestHealthComponents_RiverNilReaderUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		Staleness:  &mockStalenessReader{},
		DiskUsage:  okDiskUsage(50),
		Thresholds: defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "unknown", resp.Components["river"].Status)
}

func TestHealthComponents_SyncBreachDegrades(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	now := time.Now()
	river := freshWatchdogReader()
	staleness := &mockStalenessReader{breaches: []repository.StalenessBreach{
		{Source: "gcontacts", StaleSince: now.Add(-2 * time.Hour)},
	}}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        staleness,
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "degraded", resp.Components["sync"].Status)
	raw := marshalDetails(t, resp.Components["sync"].Details)
	assert.Contains(t, raw, "active_breach_count")
	assert.Contains(t, raw, "max_staleness_seconds")
}

func TestHealthComponents_SyncWatchdogStaleUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	staleTrail := time.Now().Add(-2 * time.Hour) // older than 30m
	// Zero breaches, but the watchdog completion trail is stale: must report
	// unknown (covers total-River-death + persistently-failing-worker).
	river := &mockRiverStatsReader{latestComplete: &staleTrail}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	wr, respr := doHealthRequest(t, hc, true)
	assert.Equal(t, "unknown", respr.Components["sync"].Status)
	assert.Equal(t, http.StatusServiceUnavailable, wr.Code) // readiness 503
	require.NotNil(t, respr.Components["sync"].Error)
	assert.Contains(t, *respr.Components["sync"].Error, "watchdog not running")
}

func TestHealthComponents_SyncWatchdogAbsentUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	// No completed watchdog job at all (latestComplete nil).
	river := &mockRiverStatsReader{}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "unknown", resp.Components["sync"].Status)
}

func TestHealthComponents_SyncWatchdogGuardDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	// Stale trail, but guard disabled via empty kind → sync evaluates breaches
	// only (0 breaches → healthy).
	staleTrail := time.Now().Add(-10 * time.Hour)
	river := &mockRiverStatsReader{latestComplete: &staleTrail}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "", // guard disabled
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "healthy", resp.Components["sync"].Status)

	// Also disabled via zero max-age.
	thresholds := defaultTestThresholds()
	thresholds.SyncWatchdogMaxAge = 0
	hc2 := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       thresholds,
	})
	_, resp2 := doHealthRequest(t, hc2, true)
	assert.Equal(t, "healthy", resp2.Components["sync"].Status)
}

// TestHealthComponents_SyncPIIGuard is the load-bearing privacy assertion: the
// /health route is unauthenticated, so the serialized sync component must
// expose ONLY counts + max-age — never the breach AccountID (an email) or
// Details (provider error text).
func TestHealthComponents_SyncPIIGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	now := time.Now()
	const secretEmail = "private.person@example.com"
	const secretDetails = "oauth token refresh failed for private.person@example.com"
	river := freshWatchdogReader()
	staleness := &mockStalenessReader{breaches: []repository.StalenessBreach{
		{
			Source:     "gcontacts",
			AccountID:  secretEmail,
			BreachType: "sync_error",
			Details:    secretDetails,
			StaleSince: now.Add(-3 * time.Hour),
		},
	}}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        staleness,
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	w, _ := doHealthRequest(t, hc, true)
	body := w.Body.String()
	assert.NotContains(t, body, secretEmail, "account email must not leak into /health")
	assert.NotContains(t, body, secretDetails, "breach details must not leak into /health")
	assert.NotContains(t, body, "sync_error", "breach type must not leak into /health")
	// The safe fields ARE present.
	assert.Contains(t, body, "active_breach_count")
}

func TestHealthComponents_SyncReaderErrorUnhealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	river := freshWatchdogReader()
	staleness := &mockStalenessReader{err: errors.New("breach read failed")}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            river,
		Staleness:        staleness,
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "unhealthy", resp.Components["sync"].Status)
}

func TestHealthComponents_SyncNilReaderUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:      freshWatchdogReader(),
		DiskUsage:  okDiskUsage(50),
		Thresholds: defaultTestThresholds(),
	})

	_, resp := doHealthRequest(t, hc, true)
	assert.Equal(t, "unknown", resp.Components["sync"].Status)
}

func TestHealthComponents_DiskFloorBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}

	cases := []struct {
		name        string
		freePercent uint64
		floor       int
		wantStatus  string
	}{
		{"above floor", 20, 10, "healthy"},
		{"equal floor", 10, 10, "healthy"}, // strictly-less degrades
		{"below floor", 5, 10, "degraded"},
		{"floor disabled", 1, 0, "healthy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thresholds := defaultTestThresholds()
			thresholds.DiskMinFreePercent = tc.floor
			hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
				River:            freshWatchdogReader(),
				Staleness:        &mockStalenessReader{},
				SyncWatchdogKind: "sync_staleness_watchdog",
				DiskUsage:        okDiskUsage(tc.freePercent),
				Thresholds:       thresholds,
			})
			_, resp := doHealthRequest(t, hc, true)
			assert.Equal(t, tc.wantStatus, resp.Components["disk"].Status)
		})
	}
}

func TestHealthComponents_DiskStatfsErrorUnhealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            freshWatchdogReader(),
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        errDiskUsage(errors.New("no such path")),
		Thresholds:       defaultTestThresholds(),
	})

	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusOK, w.Code) // bare liveness unaffected
	assert.Equal(t, "unhealthy", resp.Components["disk"].Status)
	require.NotNil(t, resp.Components["disk"].Error)
	assert.Contains(t, *resp.Components["disk"].Error, "no such path")
}

func TestHealthComponents_DiskDefaultStatfsWhenNilUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: false}
	thresholds := defaultTestThresholds() // floor disabled, so this can't flake
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            freshWatchdogReader(),
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		// DiskUsage nil → constructor installs the real statfs default.
		Thresholds: thresholds,
	})

	_, resp := doHealthRequest(t, hc, false)
	disk := resp.Components["disk"]
	// Real statfs over "/" should succeed and report a non-zero total.
	assert.Equal(t, "healthy", disk.Status)
	raw := marshalDetails(t, disk.Details)
	assert.Contains(t, raw, "total_bytes")
	assert.Contains(t, raw, "free_percent")
}

func TestHealthComponents_DBDownSkipsRiverSync(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB := &mockDatabaseChecker{shouldError: true, err: errors.New("connection refused")}
	hc := health.NewHealthChecker(mockDB, 5*time.Second, health.Deps{
		River:            freshWatchdogReader(),
		Staleness:        &mockStalenessReader{},
		SyncWatchdogKind: "sync_staleness_watchdog",
		DiskUsage:        okDiskUsage(50),
		Thresholds:       defaultTestThresholds(),
	})

	// Bare: 503 + degraded (unchanged contract); river/sync skipped/unknown;
	// disk still evaluated.
	w, resp := doHealthRequest(t, hc, false)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	assert.Equal(t, "unhealthy", resp.Components["database"].Status)
	assert.Equal(t, "unknown", resp.Components["river"].Status)
	assert.Equal(t, "unknown", resp.Components["sync"].Status)
	require.NotNil(t, resp.Components["river"].Error)
	assert.Contains(t, *resp.Components["river"].Error, "skipped: database unhealthy")
	assert.Equal(t, "healthy", resp.Components["disk"].Status)

	// Readiness also 503.
	wr, _ := doHealthRequest(t, hc, true)
	assert.Equal(t, http.StatusServiceUnavailable, wr.Code)
}

// marshalDetails re-marshals a ComponentStatus.Details payload to JSON so tests
// can assert on field presence/absence (omitempty semantics) at the wire level.
func marshalDetails(t *testing.T, details any) string {
	t.Helper()
	require.NotNil(t, details)
	b, err := json.Marshal(details)
	require.NoError(t, err)
	return string(b)
}
