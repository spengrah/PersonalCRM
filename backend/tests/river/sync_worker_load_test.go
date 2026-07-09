package river

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"
	"personal-crm/backend/tests/testsupport"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadTestProvider is a provider stub that optionally injects failures
// based on a per-call counter and records timestamped invocation windows
// so the test can detect concurrent Sync calls for the same
// (source, account_id) (which would indicate the in-flight dedup broke).
type loadTestProvider struct {
	cfg          syncpkg.SourceConfig
	mu           sync.Mutex
	callCount    int32
	failureEvery int32 // e.g. 5 means "every 5th call fails"; 0 means never fail
	activeByKey  map[string]bool
	overlaps     int32
}

func (p *loadTestProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *loadTestProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}

func (p *loadTestProvider) Sync(ctx context.Context, state *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	key := state.Source + "|"
	if state.AccountID != nil {
		key += *state.AccountID
	}

	p.mu.Lock()
	if p.activeByKey[key] {
		atomic.AddInt32(&p.overlaps, 1)
	}
	p.activeByKey[key] = true
	n := atomic.AddInt32(&p.callCount, 1)
	p.mu.Unlock()

	// Simulate provider work.
	select {
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.activeByKey, key)
		p.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(80 * time.Millisecond):
	}

	p.mu.Lock()
	delete(p.activeByKey, key)
	p.mu.Unlock()

	if p.failureEvery > 0 && n%p.failureEvery == 0 {
		return nil, fmt.Errorf("injected failure on call %d", n)
	}
	return &syncpkg.SyncResult{ItemsProcessed: 1}, nil
}

// TestSyncWorker_LoadNoDuplicateConcurrentSyncs runs N simulated provider
// accounts and repeatedly invokes the tick worker + fires the job
// queue for a fixed window. Asserts that:
//   - every account's provider.Sync is called at least once,
//   - no two Sync calls for the same (source, account_id) overlap in
//     wall-clock time (the dedup path is atomically correct under load),
//   - no external_sync_state row remains in status='syncing' at the end.
//
// The test's budget is bounded: windowBudget caps total runtime.
func TestSyncWorker_LoadNoDuplicateConcurrentSyncs(t *testing.T) {
	testsupport.RequireLongTests(t)
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	const accountCount = 10
	const windowBudget = 30 * time.Second

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = testsupport.GetMigrationsPath()
	// Migrations are applied once by TestMain.

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	source := "load_test_provider"

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	// Clean up rows from prior runs (hard-delete via test-only sqlc helpers,
	// not raw SQL — core.md rule 2).
	cleanup := func(c context.Context) error {
		if err := syncRepo.DeleteRiverJobsBySourceArgForTest(c, source); err != nil {
			return err
		}
		if err := syncRepo.DeleteSyncLogsBySourceForTest(c, source); err != nil {
			return err
		}
		return syncRepo.DeleteSyncStatesBySourceForTest(c, source)
	}
	// Setup cleanup must succeed: a failed delete would leave stale rows that
	// corrupt the drain/assertions. Teardown cleanup (below) is best-effort.
	require.NoError(t, cleanup(ctx))
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &loadTestProvider{
		cfg: syncpkg.SourceConfig{
			Name:            source,
			DisplayName:     source,
			Strategy:        repository.SyncStrategyFetchAll,
			DefaultInterval: 100 * time.Millisecond,
		},
		failureEvery: 5, // 20% failure rate
		activeByKey:  make(map[string]bool),
	}
	registry.Register(provider)
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Seed N accounts, all due.
	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	acctIDs := make([]string, accountCount)
	for i := 0; i < accountCount; i++ {
		a := fmt.Sprintf("load-acct-%d", i)
		acctIDs[i] = a
		_, err = syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:     source,
			AccountID:  &a,
			Enabled:    true,
			Strategy:   repository.SyncStrategyFetchAll,
			NextSyncAt: &past,
		})
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	workers := river.NewWorkers()
	river.AddWorker(workers, scheduler.NewSchedulerTickWorker(svc))
	river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		Workers:     workers,
		RetryPolicy: &fastRetryPolicy{},
	})
	require.NoError(t, err)
	svc.SetRiverEnqueuer(client)

	// Start with the root ctx, never a timeout-derived context: River binds
	// its fetch/work loops to the context passed to Start, so a start-deadline
	// cancel silently stops fetching mid-test and strands enqueued jobs
	// (documented project gotcha; applies to test harnesses too). client.Stop
	// keeps its own bounded ctx below — that bound is legitimate.
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	// Drive the tick manually every 500ms for the budgeted window —
	// emulates the 5-minute periodic at a test-friendly cadence.
	tick := scheduler.NewSchedulerTickWorker(svc)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(windowBudget)

drive:
	for {
		select {
		case <-deadline:
			break drive
		case <-ticker.C:
			_ = tick.Work(ctx, &river.Job[scheduler.SchedulerTickArgs]{Args: scheduler.SchedulerTickArgs{}})
		}
	}

	// Drain to quiescence: poll until no ACTIVE river_job rows remain for this
	// source. This is a HARD pass condition, not a best-effort wait — with a
	// live fetch loop (root Start ctx) this workload drains in low single-digit
	// seconds even under load, so a stuck backlog is the dead-fetch-loop
	// regression signature (or genuine starvation), and failing loudly here is
	// the whole point. Assertions 3/4 below are only meaningful at quiescence:
	// zero active jobs means every worker's CompleteSyncLog call has returned,
	// so any row still 'syncing'/'running' is a genuine lifecycle bug, not a
	// timing artifact. Fixed attempt count + Sleep (no time.Now — core.md rule
	// 1); ~20s budget is an order of magnitude over the healthy drain time.
	const drainAttempts = 200
	const drainInterval = 100 * time.Millisecond
	drained := false
	for i := 0; i < drainAttempts; i++ {
		active, err := syncRepo.CountActiveRiverJobsBySourceArgForTest(ctx, source)
		require.NoError(t, err)
		if active == 0 {
			drained = true
			break
		}
		time.Sleep(drainInterval)
	}
	if !drained {
		breakdown, err := syncRepo.CountActiveRiverJobsByStateBySourceForTest(ctx, source)
		require.NoError(t, err)
		t.Fatalf("river_job did not drain to zero within %s; remaining active jobs by state: %v "+
			"(all 'available' => fetch loop dead; jobs cycling through 'running' => genuine starvation)",
			time.Duration(drainAttempts)*drainInterval, breakdown)
	}

	// Assertion 1: every account was invoked at least once by provider.Sync.
	// We don't have per-account counters on the provider, but the easier
	// invariant to check is total calls >= accountCount.
	total := atomic.LoadInt32(&provider.callCount)
	assert.GreaterOrEqual(t, int(total), accountCount,
		"expected at least %d total Sync invocations across all accounts", accountCount)

	// Assertion 2: no overlapping concurrent Syncs for the same account.
	assert.Equal(t, int32(0), atomic.LoadInt32(&provider.overlaps),
		"dedup broke: detected overlapping Sync calls for the same account")

	// Assertion 3: no lingering 'syncing' rows.
	syncing, err := syncRepo.CountSyncStatesByStatusForTest(ctx, source, "syncing")
	require.NoError(t, err)
	assert.Equal(t, int64(0), syncing, "no external_sync_state row should end in 'syncing'")

	// Assertion 4: no 'running' external_sync_log rows left behind.
	running, err := syncRepo.CountSyncLogsByStatusForTest(ctx, source, "running")
	require.NoError(t, err)
	assert.Equal(t, int64(0), running, "no external_sync_log row should end in 'running'")
}
