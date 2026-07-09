package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
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
// against a per-test isolated DB clone with a locally-defined no-op worker
// starts and stops cleanly.
func TestRiverClient_StartStop_Integration(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Per-test isolated clone so the live client owns a private river_job.
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	workers := river.NewWorkers()
	river.AddWorker(workers, &localNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	// Root ctx, not a timeout-derived one: River binds its fetch/work loops to
	// the Start ctx, so a start-deadline cancel silently stops fetching
	// (documented project gotcha; applies to test harnesses too).
	require.NoError(t, client.Start(ctx))

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
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Per-test isolated clone so the inserted job lands in a private
	// river_job and no concurrent sibling steals it. newIsolatedRiverTestDB
	// registers database.Close() on t.Cleanup; the client.Stop cleanup
	// registered below runs first (t.Cleanup is LIFO), so the pool stays
	// alive while the client finalizes its last job batch.
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

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

	// Root ctx, not a timeout-derived one: River binds its fetch/work loops to
	// the Start ctx, so a start-deadline cancel silently stops fetching
	// (documented project gotcha; applies to test harnesses too).
	require.NoError(t, client.Start(ctx))

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
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Per-test isolated clone so the live client owns a private river_job.
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

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

	// Root ctx, not a timeout-derived one: River binds its fetch/work loops to
	// the Start ctx, so a start-deadline cancel silently stops fetching
	// (documented project gotcha; applies to test harnesses too).
	require.NoError(t, client.Start(ctx), "client.Start must succeed when only the noop worker is registered")

	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stopCancel()
	require.NoError(t, client.Stop(stopCtx))
}

// alwaysFailJobArgs is a test-only worker type whose worker always returns an
// error, so it exhausts its attempts and lands in `discarded`.
type alwaysFailJobArgs struct{}

func (alwaysFailJobArgs) Kind() string { return "test_always_fail" }

type alwaysFailWorker struct {
	river.WorkerDefaults[alwaysFailJobArgs]
}

func (*alwaysFailWorker) Work(_ context.Context, _ *river.Job[alwaysFailJobArgs]) error {
	return errors.New("deliberate test failure")
}

// errorHandlerFastRetry puts every retry 200ms out so a two-attempt job reaches
// `discarded` in a few seconds. The accelerated clock is used per the repo's
// no-time.Now rule.
type errorHandlerFastRetry struct{}

func (errorHandlerFastRetry) NextRetry(_ *rivertype.JobRow) time.Time {
	return accelerated.GetCurrentTime().Add(200 * time.Millisecond)
}

// TestRiverErrorHandler_DiscardLogging_Integration pins the ErrorHandler's
// discard predicate against the REAL River executor: an always-failing job with
// MaxAttempts=2 must produce exactly one WARN (attempt 1, discarded=false) and
// one ERROR (attempt 2, discarded=true). This is the upgrade-time safety net for
// the handler's duplication of River's `Attempt >= MaxAttempts` comparison — a
// future River bump that changes the discard semantics fails here instead of
// silently mislabeling.
func TestRiverErrorHandler_DiscardLogging_Integration(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	// Thread-safe buffer: River invokes the ErrorHandler on its own goroutines.
	buf := &bytes.Buffer{}
	zl := zerolog.New(zerolog.SyncWriter(buf)).Level(zerolog.DebugLevel).With().Timestamp().Logger()

	workers := river.NewWorkers()
	river.AddWorker(workers, &alwaysFailWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:      workers,
		ErrorHandler: events.NewRiverErrorHandler(&zl),
		RetryPolicy:  errorHandlerFastRetry{},
		// NOT TestOnly: we need the scheduler loop so the retryable attempt 1
		// becomes available and gets worked a second time to reach `discarded`.
	})
	require.NoError(t, err)

	// Start with the test's BASE ctx (never a timeout-derived ctx — River
	// silently stops fetching when its fetch-loop ctx cancels).
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	insertRes, err := client.Insert(ctx, alwaysFailJobArgs{}, &river.InsertOpts{MaxAttempts: 2})
	require.NoError(t, err)
	jobID := insertRes.Job.ID

	// Poll until the job lands in `discarded` (both attempts exhausted). Generous
	// deadline: ~200ms retry backoff + scheduler interval.
	waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		state, qerr := database.Queries.GetRiverJobStateByID(ctx, jobID)
		if qerr == nil && state == db.RiverJobStateDiscarded {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("timed out waiting for job %d to reach discarded", jobID)
		case <-tick.C:
		}
	}

	// Parse the buffered JSON log lines, filtering to records carrying our
	// job_id (Logger is unset here, so only the ErrorHandler writes).
	var warnRec, errorRec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		if id, ok := m["job_id"].(float64); !ok || int64(id) != jobID {
			continue
		}
		switch m["level"] {
		case "warn":
			warnRec = m
		case "error":
			errorRec = m
		}
	}

	require.NotNil(t, warnRec, "expected a WARN record for the non-final attempt")
	require.Equal(t, false, warnRec["discarded"])
	require.Equal(t, float64(1), warnRec["attempt"])
	require.Equal(t, "test_always_fail", warnRec["kind"])
	require.Contains(t, warnRec["error"], "deliberate test failure")
	require.NotNil(t, warnRec["args"], "args field must be present")

	require.NotNil(t, errorRec, "expected an ERROR record for the final (discard) attempt")
	require.Equal(t, true, errorRec["discarded"])
	require.Equal(t, float64(2), errorRec["attempt"])
	require.Equal(t, float64(2), errorRec["max_attempts"])
}
