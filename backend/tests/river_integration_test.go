package tests

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// localNoopJobArgs is a test-only worker type. It intentionally does not
// import from the `main` package — each package that needs a smoke-test
// worker defines its own to avoid coupling tests to the main binary.
type localNoopJobArgs struct{}

// Kind implements river.JobArgs.
func (localNoopJobArgs) Kind() string { return "test_noop" }

// localNoopWorker optionally records that Work was invoked on a shared
// counter. A zero-value worker is safe for the Start/Stop smoke test
// that never enqueues jobs.
type localNoopWorker struct {
	river.WorkerDefaults[localNoopJobArgs]
	// invoked is incremented each time Work runs. nil → pure no-op.
	invoked *atomic.Int32
}

func (w *localNoopWorker) Work(_ context.Context, _ *river.Job[localNoopJobArgs]) error {
	if w.invoked != nil {
		w.invoked.Add(1)
	}
	return nil
}

// TestRunMigrations_River_Integration asserts that RunMigrations applies
// River's own schema migrations and that a second call is idempotent.
func TestRunMigrations_River_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	ctx := context.Background()

	// First run: golang-migrate + river-migrate both apply (or report
	// no-op if a previous test already migrated the shared DB).
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	// Verify river's schema is present by querying its migration table.
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	var count int
	err = database.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM river_migration`).Scan(&count)
	require.NoError(t, err)
	require.Positive(t, count, "river_migration table should have at least one row")

	// Second run: idempotent no-op on both sides.
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))
}

// TestRiverClient_StartStop_Integration asserts that a River client built
// against the shared test DB with a locally-defined no-op worker starts and
// stops cleanly.
func TestRiverClient_StartStop_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	workers := river.NewWorkers()
	river.AddWorker(workers, &localNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))

	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stopCancel()
	require.NoError(t, client.Stop(stopCtx))
}

// TestRiverClient_InsertAndWork_Integration proves the full enqueue →
// worker Work path. Codex review on PR 1 flagged that Start/Stop alone
// does not demonstrate that the AddWorker registration actually dispatches
// a job — this test does. It's kept separate from the Start/Stop smoke
// test so a registration regression fails this test specifically.
func TestRiverClient_InsertAndWork_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// Register db.Close first so it runs AFTER client.Stop below — t.Cleanup
	// runs functions in LIFO order, so the pool stays alive while the
	// client finalizes its last job batch.
	t.Cleanup(func() { database.Close() })

	var invoked atomic.Int32
	workers := river.NewWorkers()
	river.AddWorker(workers, &localNoopWorker{invoked: &invoked})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true, // skip leader election + periodic loops in tests
	})
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	// Enqueue a job and wait for Work to run.
	_, err = client.Insert(ctx, localNoopJobArgs{}, nil)
	require.NoError(t, err)

	// Poll for the worker invocation. River picks up work shortly after
	// insert; 10s is generous headroom for a loaded test DB. We use a
	// context timeout rather than time.Now so the forbidigo lint rule
	// (accelerated.GetCurrentTime vs time.Now) stays satisfied.
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if invoked.Load() >= 1 {
			return
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("expected localNoopWorker.Work to run at least once, got %d invocations", invoked.Load())
		case <-tick.C:
		}
	}
}

// TestRiverClient_BootsWithNoopWorkerOnly mirrors the production boot
// path when `cfg.Features.EnableExternalSync == false` (the default):
// main.go registers ONLY the placeholder noop worker, no scheduler_tick
// or sync_provider_account workers, and NO periodic jobs. The test
// asserts that river.NewClient accepts this bundle and that Start /
// Stop complete cleanly.
//
// Regression guard for the PR 3 bug where the scheduler workers were
// registered inside `if cfg.Features.EnableExternalSync` but the noop
// was removed, producing an empty Workers bundle and a boot failure
// ("at least one Worker must be added to the Workers bundle") in the
// default configuration. #281 restored the noop.
func TestRiverClient_BootsWithNoopWorkerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	// Simulate the main.go boot path with EnableExternalSync=false: only
	// the noop worker is registered, no periodic jobs.
	workers := river.NewWorkers()
	river.AddWorker(workers, &localNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
		// No PeriodicJobs — mirrors the disabled-sync main.go branch.
	})
	require.NoError(t, err, "river.NewClient must accept a Workers bundle with just the noop worker")

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx), "client.Start must succeed when only the noop worker is registered")

	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stopCancel()
	require.NoError(t, client.Stop(stopCtx))
}
