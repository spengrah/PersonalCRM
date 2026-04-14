package tests

import (
	"context"
	"os"
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

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPeriodicTick_EndToEndEnqueueWithAtomicClaim asserts that the
// SchedulerTickWorker, driven by a real SyncService + SyncRepository
// stack, reads due sync states and enqueues exactly one
// SyncProviderAccountJob per due account. This is the production-path
// integration check: plan + repo + worker + river client together.
//
// The rescue / crash-recovery behavior lives in
// sync_worker_leased_retry_test.go; dedup (in-flight skip, completed
// doesn't block, cross-window) lives in sync_repo_enqueue_test.go. This
// file only proves the tick-to-insert plumbing.
func TestPeriodicTick_EndToEndEnqueueWithAtomicClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	// Clean-up pre-existing state that could collide with this test's
	// account IDs. The source strings embed the test name so future
	// tests with different strings don't collide.
	src1 := "tick_test_gmail"
	src2 := "tick_test_todoist"

	_, err = database.Pool.Exec(ctx,
		`DELETE FROM river_job WHERE kind = 'sync_provider_account'
		 AND (args->>'source') IN ($1, $2)`, src1, src2)
	require.NoError(t, err)

	_, err = database.Pool.Exec(ctx,
		`DELETE FROM external_sync_state WHERE source IN ($1, $2)`, src1, src2)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()

	// Register stub providers for both sources so service.ListDueAccounts
	// does not filter them out as "unregistered" (that filter exists to
	// skip poison jobs for stale sync_state rows; see service/sync.go).
	registry.Register(&tickStubProvider{cfg: syncpkg.SourceConfig{
		Name:            src1,
		DisplayName:     src1,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}})
	registry.Register(&tickStubProvider{cfg: syncpkg.SourceConfig{
		Name:            src2,
		DisplayName:     src2,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}})

	// Seed two sync_state rows whose next_sync_at is already in the past,
	// so they're eligible for the tick.
	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	acct := "user@example.com"
	_, err = syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     src1,
		AccountID:  &acct,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	_, err = syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     src2,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM external_sync_state WHERE source IN ($1, $2)`, src1, src2)
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM river_job WHERE kind = 'sync_provider_account'
			 AND (args->>'source') IN ($1, $2)`, src1, src2)
	})

	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Build a test-only river client. TestOnly prevents leader/periodic
	// loops from racing with our manual Work() invocation below, and we
	// never call client.Start — the tick worker's Work is invoked
	// directly so the pending-available rows aren't drained by the
	// worker pool.
	workers := river.NewWorkers()
	river.AddWorker(workers, scheduler.NewSchedulerTickWorker(svc))
	river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	svc.SetRiverEnqueuer(client)

	// Invoke the tick worker's Work directly — the periodic scheduling
	// mechanism is river's own and tested separately; here we verify the
	// worker's contract against a live service + repository.
	tickWorker := scheduler.NewSchedulerTickWorker(svc)
	err = tickWorker.Work(ctx, &river.Job[scheduler.SchedulerTickArgs]{Args: scheduler.SchedulerTickArgs{}})
	require.NoError(t, err)

	// Both due accounts should have exactly one river_job row each.
	for _, src := range []string{src1, src2} {
		var cnt int
		require.NoError(t, database.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM river_job WHERE kind = 'sync_provider_account'
			 AND (args->>'source') = $1`, src,
		).Scan(&cnt))
		assert.Equal(t, 1, cnt, "expected exactly one enqueued job for source=%s", src)
	}
}

// TestPeriodicTick_FiresOnStart verifies the actual river PeriodicJob
// wiring: a client configured with a 5-minute PeriodicJob and
// RunOnStart:true must invoke SchedulerTickWorker.Work once shortly
// after client.Start. This guards the production main.go wiring
// (RunOnStart flag, PeriodicJob list) that the direct-Work test above
// cannot exercise.
//
// We use a counting tick worker shim wrapped around SchedulerTickWorker
// so the test can observe the Work invocation without scheduling any
// downstream sync_provider_account jobs (registry empty → service
// returns empty due list → tick Work returns nil fast).
func TestPeriodicTick_FiresOnStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	// Empty registry → service.ListDueAccounts returns 0 due accounts →
	// Work returns nil immediately with no downstream enqueues.
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Counting tick worker: wraps the real tick worker. Shares the
	// SchedulerTickArgs kind, so registering this satisfies river. Could
	// also just inject a fake SyncServiceForTick, but reusing
	// SchedulerTickWorker exercises more of the real code path.
	var tickCalls atomic.Int32
	counting := &countingTickWorker{
		inner: scheduler.NewSchedulerTickWorker(svc),
		calls: &tickCalls,
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, counting)
	// A sync worker is also needed because the PeriodicJob construction
	// in main.go registers both. For this test the sync worker never
	// runs (no due accounts enumerated), but river rejects unknown
	// kinds at client-build time in some code paths.
	river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(svc))

	periodicJobs := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return scheduler.SchedulerTickArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		Workers:      workers,
		PeriodicJobs: periodicJobs,
	})
	require.NoError(t, err)
	svc.SetRiverEnqueuer(client)

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	// RunOnStart should fire within a few seconds after Start returns.
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for tickCalls.Load() < 1 {
		select {
		case <-waitCtx.Done():
			t.Fatalf("expected tick worker to fire on start within 10s; got %d invocations", tickCalls.Load())
		case <-tick.C:
		}
	}
	assert.GreaterOrEqual(t, tickCalls.Load(), int32(1),
		"tick worker should have fired at least once after client.Start with RunOnStart:true")
}

// countingTickWorker wraps a real SchedulerTickWorker and increments a
// counter each time Work is invoked. Satisfies river.Worker for
// SchedulerTickArgs.
type countingTickWorker struct {
	river.WorkerDefaults[scheduler.SchedulerTickArgs]
	inner *scheduler.SchedulerTickWorker
	calls *atomic.Int32
}

func (w *countingTickWorker) Work(ctx context.Context, job *river.Job[scheduler.SchedulerTickArgs]) error {
	w.calls.Add(1)
	return w.inner.Work(ctx, job)
}

// tickStubProvider satisfies sync.SyncProvider so a registry can be
// populated without wiring real providers. The tick test never actually
// calls Sync — it only invokes the tick worker, which reads due states
// and enqueues jobs — so this stub's Sync is a no-op.
type tickStubProvider struct {
	cfg syncpkg.SourceConfig
}

func (p *tickStubProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *tickStubProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}
func (p *tickStubProvider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	return &syncpkg.SyncResult{}, nil
}
