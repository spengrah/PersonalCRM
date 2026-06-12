//go:build integration_testdb

package tests

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalenessTestConfig is the production-default staleness config the seeded
// fixtures are positioned against.
func stalenessTestConfig() config.StalenessConfig {
	return config.TestConfig().Staleness
}

// pushHealth builds a source_health JSONB blob for a single source.
func pushHealth(t *testing.T, source string, enabled bool, lastPushedAt *time.Time) json.RawMessage {
	t.Helper()
	entry := map[string]any{"enabled": enabled}
	if lastPushedAt != nil {
		entry["last_pushed_at"] = lastPushedAt.UTC().Format(time.RFC3339Nano)
	}
	blob, err := json.Marshal(map[string]any{source: entry})
	require.NoError(t, err)
	return blob
}

func openBreachByType(breaches []repository.StalenessBreach, breachType string) *repository.StalenessBreach {
	for i := range breaches {
		if breaches[i].BreachType == breachType {
			return &breaches[i]
		}
	}
	return nil
}

// TestStalenessWatchdog_LifecycleEndToEnd drives the full detect → persist →
// re-observe → resolve → re-detect cycle against an isolated clone DB
// (mandatory: RunChecks reads the whole DB and the mac_host singleton index
// forbids sharing the package DB).
func TestStalenessWatchdog_LifecycleEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)

	now := accelerated.GetCurrentTime()

	// --- Seed a stale pull sync-state (last success 30 days back). ---
	acct := "watchdog-pull@example.com"
	pullState, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     "gcal",
		AccountID:  &acct,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchFiltered,
		NextSyncAt: &now,
	})
	require.NoError(t, err)
	lastSuccess := now.Add(-30 * 24 * time.Hour)
	require.NoError(t, syncRepo.SetSyncStateFreshnessForTest(ctx, repository.SetSyncStateFreshnessForTestParams{
		ID:                   pullState.ID,
		Status:               repository.SyncStatusIdle,
		LastSyncAt:           &lastSuccess,
		LastSuccessfulSyncAt: &lastSuccess,
		ErrorCount:           0,
	}))

	// --- Seed an active host: heartbeat 2h back, messages push 10 days back. ---
	pushAt := now.Add(-10 * 24 * time.Hour)
	host, err := macHostRepo.SeedHostForTest(ctx, "watchdog-host", "0.3.1", 1, "hash", nil,
		pushHealth(t, "messages", true, &pushAt))
	require.NoError(t, err)
	heartbeatAt := now.Add(-2 * time.Hour)
	require.NoError(t, macHostRepo.SetHeartbeatAtForTest(ctx, host.ID, heartbeatAt))

	svc := service.NewStalenessService(stalenessTestConfig(), true, syncRepo, macHostRepo, breachRepo)

	// --- First run: exactly the three expected open breaches. ---
	require.NoError(t, svc.RunChecks(ctx))
	breaches, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)
	require.Len(t, breaches, 3, "expected sync_stale + heartbeat + push_stale")

	syncStale := openBreachByType(breaches, repository.BreachTypeSyncStale)
	require.NotNil(t, syncStale, "expected a sync_stale breach")
	assert.Equal(t, "gcal", syncStale.Source)
	assert.Equal(t, acct, syncStale.AccountID)
	assert.WithinDuration(t, lastSuccess.UTC(), syncStale.StaleSince, time.Second)
	assert.Equal(t, int64((24 * time.Hour).Seconds()), syncStale.ThresholdSeconds)

	heartbeat := openBreachByType(breaches, repository.BreachTypeHeartbeat)
	require.NotNil(t, heartbeat, "expected a heartbeat breach")
	assert.Equal(t, host.ID.String(), heartbeat.AccountID)
	assert.WithinDuration(t, heartbeatAt.UTC(), heartbeat.StaleSince, time.Second)

	pushStale := openBreachByType(breaches, repository.BreachTypePushStale)
	require.NotNil(t, pushStale, "expected a push_stale breach")
	assert.Equal(t, "messages", pushStale.Source)
	assert.Equal(t, host.ID.String(), pushStale.AccountID)
	assert.WithinDuration(t, pushAt.UTC(), pushStale.StaleSince, time.Second)

	firstDetectedAt := map[string]time.Time{}
	firstObservedAt := map[string]time.Time{}
	for _, b := range breaches {
		firstDetectedAt[b.BreachType] = b.DetectedAt
		firstObservedAt[b.BreachType] = b.LastObservedAt
	}

	// --- Second run: same rows, detected_at stable, last_observed_at advances. ---
	time.Sleep(10 * time.Millisecond) // ensure NOW() moves for last_observed_at
	require.NoError(t, svc.RunChecks(ctx))
	breaches2, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)
	require.Len(t, breaches2, 3, "re-run must not open new breaches")
	for _, b := range breaches2 {
		assert.Equal(t, firstDetectedAt[b.BreachType], b.DetectedAt, "detected_at must be immutable for %s", b.BreachType)
		assert.False(t, b.LastObservedAt.Before(firstObservedAt[b.BreachType]), "last_observed_at must be monotonic for %s", b.BreachType)
	}

	// --- Heal all three. ---
	freshSuccess := now.Add(-1 * time.Minute)
	require.NoError(t, syncRepo.SetSyncStateFreshnessForTest(ctx, repository.SetSyncStateFreshnessForTestParams{
		ID:                   pullState.ID,
		Status:               repository.SyncStatusIdle,
		LastSyncAt:           &freshSuccess,
		LastSuccessfulSyncAt: &freshSuccess,
		ErrorCount:           0,
	}))
	require.NoError(t, macHostRepo.SetHeartbeatAtForTest(ctx, host.ID, freshSuccess))
	_, err = macHostRepo.UpdateHeartbeat(ctx, host.ID, repository.HeartbeatPayload{
		DaemonVersion:   "0.3.1",
		ProtocolVersion: 1,
		SourceHealth:    pushHealth(t, "messages", true, &freshSuccess),
	})
	require.NoError(t, err)

	require.NoError(t, svc.RunChecks(ctx))
	healed, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)
	assert.Empty(t, healed, "all three breaches should resolve after healing")

	// --- Re-break the heartbeat: a NEW open row with a fresh detected_at. ---
	require.NoError(t, macHostRepo.SetHeartbeatAtForTest(ctx, host.ID, now.Add(-2*time.Hour)))
	require.NoError(t, svc.RunChecks(ctx))
	reopened, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)
	require.Len(t, reopened, 1)
	require.Equal(t, repository.BreachTypeHeartbeat, reopened[0].BreachType)
	assert.NotEqual(t, firstDetectedAt[repository.BreachTypeHeartbeat], reopened[0].DetectedAt,
		"re-broken breach should have a new detected_at, not reuse the resolved row")
}

// TestStalenessWatchdog_SyncErrorPath pins the two-term sync_error predicate
// against the real DB write path (UpdateSyncStateStatus increments error_count
// on every error attempt; UpdateSyncStateSuccess clears it).
func TestStalenessWatchdog_SyncErrorPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)

	now := accelerated.GetCurrentTime()
	svc := service.NewStalenessService(stalenessTestConfig(), true, syncRepo, macHostRepo, breachRepo)

	// Erroring row: last success already older than ErrorThreshold (6h).
	acct := "watchdog-error@example.com"
	errState, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    "todoist",
		AccountID: &acct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyFetchAll,
	})
	require.NoError(t, err)
	oldSuccess := now.Add(-8 * time.Hour)
	require.NoError(t, syncRepo.SetSyncStateFreshnessForTest(ctx, repository.SetSyncStateFreshnessForTestParams{
		ID:                   errState.ID,
		Status:               repository.SyncStatusIdle,
		LastSyncAt:           &oldSuccess,
		LastSuccessfulSyncAt: &oldSuccess,
		ErrorCount:           0,
	}))

	// Control row: recent success but also 3 errors — must NOT breach (the
	// transient-blip case, pinned against the real write path).
	ctrlAcct := "watchdog-control@example.com"
	ctrlState, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    "todoist",
		AccountID: &ctrlAcct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyFetchAll,
	})
	require.NoError(t, err)
	recentSuccess := now.Add(-2 * time.Minute)
	require.NoError(t, syncRepo.SetSyncStateFreshnessForTest(ctx, repository.SetSyncStateFreshnessForTestParams{
		ID:                   ctrlState.ID,
		Status:               repository.SyncStatusIdle,
		LastSyncAt:           &recentSuccess,
		LastSuccessfulSyncAt: &recentSuccess,
		ErrorCount:           0,
	}))

	// Drive 3 real error attempts on both rows (UpdateSyncStateStatus bumps
	// error_count and stamps NOW() onto last_sync_at, leaving
	// last_successful_sync_at untouched).
	msg := "provider returned 503"
	for i := 0; i < 3; i++ {
		_, err = syncRepo.UpdateSyncStateStatus(ctx, errState.ID, repository.SyncStatusError, &msg)
		require.NoError(t, err)
		_, err = syncRepo.UpdateSyncStateStatus(ctx, ctrlState.ID, repository.SyncStatusError, &msg)
		require.NoError(t, err)
	}

	require.NoError(t, svc.RunChecks(ctx))
	breaches, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)

	var errBreach *repository.StalenessBreach
	for i := range breaches {
		if breaches[i].BreachType == repository.BreachTypeSyncError && breaches[i].AccountID == acct {
			errBreach = &breaches[i]
		}
		if breaches[i].AccountID == ctrlAcct {
			t.Fatalf("control row (recent success) must not breach: %+v", breaches[i])
		}
	}
	require.NotNil(t, errBreach, "erroring row past the duration floor must open a sync_error breach")
	assert.Contains(t, errBreach.Details, "3 consecutive errors")

	// Recover the erroring row → resolves.
	_, err = syncRepo.UpdateSyncStateSuccess(ctx, errState.ID, now.Add(5*time.Minute), nil)
	require.NoError(t, err)
	require.NoError(t, svc.RunChecks(ctx))
	after, err := svc.ListActiveBreaches(ctx)
	require.NoError(t, err)
	for _, b := range after {
		if b.AccountID == acct && b.BreachType == repository.BreachTypeSyncError {
			t.Fatalf("sync_error breach should resolve after success: %+v", b)
		}
	}
}

// TestStalenessWatchdog_RetentionPrune verifies that resolved breaches older
// than the 90-day cutoff are deleted by a tick, while recent resolved rows
// survive.
func TestStalenessWatchdog_RetentionPrune(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)

	now := accelerated.GetCurrentTime()

	// OLD resolved breach (observed + resolved ~200 days ago) → must be pruned.
	oldObserved := now.Add(-200 * 24 * time.Hour)
	opened, err := breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
		Source:           "gcal",
		AccountID:        "retention-old",
		BreachType:       repository.BreachTypeSyncStale,
		StaleSince:       oldObserved,
		ThresholdSeconds: 86400,
		Details:          "old",
		ObservedAt:       oldObserved,
	})
	require.NoError(t, err)
	_, err = breachRepo.ResolveBreach(ctx, opened.ID, oldObserved.Add(time.Hour))
	require.NoError(t, err)

	// RECENT resolved breach (resolved ~1 day ago) → must survive.
	recentObserved := now.Add(-1 * 24 * time.Hour)
	recent, err := breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
		Source:           "gcal",
		AccountID:        "retention-recent",
		BreachType:       repository.BreachTypeSyncStale,
		StaleSince:       recentObserved,
		ThresholdSeconds: 86400,
		Details:          "recent",
		ObservedAt:       recentObserved,
	})
	require.NoError(t, err)
	_, err = breachRepo.ResolveBreach(ctx, recent.ID, recentObserved)
	require.NoError(t, err)

	svc := service.NewStalenessService(stalenessTestConfig(), true, syncRepo, macHostRepo, breachRepo)
	require.NoError(t, svc.RunChecks(ctx))

	oldCount, err := breachRepo.CountBreachesByAccountForTest(ctx, "retention-old")
	require.NoError(t, err)
	assert.Equal(t, int64(0), oldCount, "old resolved breach should be pruned past the 90d cutoff")

	recentCount, err := breachRepo.CountBreachesByAccountForTest(ctx, "retention-recent")
	require.NoError(t, err)
	assert.Equal(t, int64(1), recentCount, "recent resolved breach should survive retention")
}

// TestStalenessWatchdog_WorkerThroughRiver registers the real worker on a River
// client over the clone DB, inserts one StalenessWatchdogArgs job, starts the
// client with the test's base ctx, and asserts the breach is opened — pinning
// the args/worker/service wiring end to end.
func TestStalenessWatchdog_WorkerThroughRiver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	breachRepo := repository.NewStalenessRepository(database.Queries)

	now := accelerated.GetCurrentTime()

	// Seed one stale pull row so the watchdog has something to detect.
	acct := "river-watchdog@example.com"
	state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    "gcal",
		AccountID: &acct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyFetchFiltered,
	})
	require.NoError(t, err)
	lastSuccess := now.Add(-30 * 24 * time.Hour)
	require.NoError(t, syncRepo.SetSyncStateFreshnessForTest(ctx, repository.SetSyncStateFreshnessForTestParams{
		ID:                   state.ID,
		Status:               repository.SyncStatusIdle,
		LastSyncAt:           &lastSuccess,
		LastSuccessfulSyncAt: &lastSuccess,
		ErrorCount:           0,
	}))

	svc := service.NewStalenessService(stalenessTestConfig(), true, syncRepo, macHostRepo, breachRepo)

	workers := river.NewWorkers()
	river.AddWorker(workers, scheduler.NewStalenessWatchdogWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	// Start with the test's base ctx (NOT a timeout ctx — River silently stops
	// fetching when its fetch-loop ctx cancels; core.md).
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	_, err = client.Insert(ctx, scheduler.StalenessWatchdogArgs{}, nil)
	require.NoError(t, err)

	// Poll until the breach row appears (the worker ran).
	deadline := time.Now().Add(20 * time.Second)
	var breaches []repository.StalenessBreach
	for time.Now().Before(deadline) {
		breaches, err = svc.ListActiveBreaches(ctx)
		require.NoError(t, err)
		if len(breaches) > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	require.NotEmpty(t, breaches, "worker should have opened a breach via River")
	assert.Equal(t, repository.BreachTypeSyncStale, breaches[0].BreachType)
	assert.Equal(t, acct, breaches[0].AccountID)
}
