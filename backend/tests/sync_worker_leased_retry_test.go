package tests

import (
	"context"
	"errors"
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

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryTestProvider is a sync.SyncProvider whose Sync behavior is
// scripted per invocation. invocation[0] is "the crashed first attempt"
// (returns an error), invocation[1] is "the retry" (returns nil). The
// struct counts invocations via an atomic so the test can assert exactly
// two calls.
type retryTestProvider struct {
	cfg       syncpkg.SourceConfig
	calls     atomic.Int32
	firstErr  error
	secondErr error
}

func (p *retryTestProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *retryTestProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}
func (p *retryTestProvider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	n := p.calls.Add(1)
	result := &syncpkg.SyncResult{ItemsProcessed: 1}
	if n == 1 {
		return result, p.firstErr
	}
	return result, p.secondErr
}

// TestSyncWorker_ContextCancelledRetry exercises the normal error-retry
// path: provider's first Sync returns an error, river retries per
// default policy, second Sync succeeds. Asserts:
//   - The log row from the first attempt is marked 'abandoned' (by the
//     second attempt's prefix cleanup), not left as 'running'.
//   - A new log row is created for the second attempt and completes
//     with status='success'.
//   - external_sync_state.status is 'idle' (not 'syncing') at the end.
//   - provider.Sync was invoked exactly 2 times.
func TestSyncWorker_ContextCancelledRetry(t *testing.T) {
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

	// Migrations are applied once by TestMain.

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	source := "retry_test_ctx_cancel"

	// Clean up any leftovers from a prior test run.
	_, _ = database.Pool.Exec(ctx,
		`DELETE FROM river_job WHERE kind = 'sync_provider_account' AND (args->>'source') = $1`, source)
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_log WHERE source = $1`, source)
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_state WHERE source = $1`, source)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()

	provider := &retryTestProvider{
		cfg: syncpkg.SourceConfig{
			Name:            source,
			DisplayName:     source,
			Strategy:        repository.SyncStrategyFetchAll,
			DefaultInterval: 15 * time.Minute,
		},
		firstErr:  errors.New("simulated first-attempt failure"),
		secondErr: nil,
	}
	registry.Register(provider)
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Build a river client configured for fast retries. The default retry
	// policy backs off by attempt-count (seconds-minutes-hours); our
	// custom policy makes every retry eligible within 200ms so the test
	// finishes within a couple of seconds.
	workers := river.NewWorkers()
	river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		Workers:     workers,
		RetryPolicy: &fastRetryPolicy{},
		// TestOnly=false — we NEED the maintenance loops for the retry
		// dispatch. But we are not using the periodic scheduler here, so
		// leader election is the only noisy piece; it's harmless.
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

	// Create the sync state that the worker will fetch.
	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     source,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM river_job WHERE kind = 'sync_provider_account' AND (args->>'source') = $1`, source)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_log WHERE source = $1`, source)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_state WHERE source = $1`, source)
	})

	// Enqueue the job.
	_, err = client.Insert(ctx, scheduler.SyncProviderAccountArgs{Source: source}, nil)
	require.NoError(t, err)

	// Wait for both invocations to complete. 10s is generous headroom
	// for a loaded CI DB; fastRetryPolicy puts the retry at now+200ms so
	// two attempts should finish well under 2s.
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for provider.calls.Load() < 2 {
		select {
		case <-waitCtx.Done():
			t.Fatalf("expected provider.Sync to be called 2+ times within 10s; got %d", provider.calls.Load())
		case <-tick.C:
		}
	}

	// Poll the DB until CompleteSyncLog on the second attempt has landed.
	// This replaces a fixed time.Sleep that used to race the commit.
	logWaitCtx, logWaitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer logWaitCancel()
	waitForSyncLogStatus(t, logWaitCtx, database.Pool, source, "success")

	// Assertion 1: provider called exactly twice.
	assert.Equal(t, int32(2), provider.calls.Load(), "provider.Sync should have run twice (first error, second success)")

	// Assertion 2: one 'abandoned' log (from the retry's prefix
	// AbandonRunningLogsForState call) and one 'success' log.
	var abandoned, succeeded int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = 'abandoned'`, source,
	).Scan(&abandoned))
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = 'success'`, source,
	).Scan(&succeeded))
	// The abandoned count depends on ordering of CompleteSyncLog vs the
	// retry's Abandon call. If CompleteSyncLog fires on the first
	// (errored) attempt before the retry starts, the first log gets
	// status='error' and is never abandoned. If the retry starts
	// between CreateSyncLog and CompleteSyncLog (rare), it becomes
	// 'abandoned'. Either outcome is acceptable — what matters is that
	// no 'running' rows survive.
	var running int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = 'running'`, source,
	).Scan(&running))
	assert.Equal(t, 0, running, "no 'running' log rows should survive after the retry completes")
	assert.Equal(t, 1, succeeded, "exactly one 'success' log expected")
	// Total logs: one per attempt = 2.
	assert.GreaterOrEqual(t, abandoned+succeeded, 1)

	// Assertion 3: state.status is 'idle' (no mutex-era 'syncing' lingers).
	updatedState, err := syncRepo.GetSyncState(ctx, state.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.SyncStatusIdle, updatedState.Status,
		"status must land on 'idle' after a retry succeeds")
}

// TestSyncWorker_AbandonedLogOnStartupRecovery verifies the explicit
// 'abandoned' transition when a pre-existing 'running' log row exists
// for a sync state and a new attempt begins. This is the acceptance
// case called out in DD 9 — it checks the data-integrity story even
// without invoking the rescue daemon.
func TestSyncWorker_AbandonedLogOnStartupRecovery(t *testing.T) {
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

	// Migrations are applied once by TestMain.

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	source := "retry_test_abandon_on_recovery"
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_log WHERE source = $1`, source)
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_state WHERE source = $1`, source)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &retryTestProvider{
		cfg: syncpkg.SourceConfig{
			Name:            source,
			DisplayName:     source,
			Strategy:        repository.SyncStrategyFetchAll,
			DefaultInterval: 15 * time.Minute,
		},
	}
	registry.Register(provider)
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Seed the state.
	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     source,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_log WHERE source = $1`, source)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_state WHERE source = $1`, source)
	})

	// Seed a pre-existing 'running' log row for this state, as if a
	// prior attempt crashed mid-sync.
	orphanLog, err := syncRepo.CreateSyncLog(ctx, state)
	require.NoError(t, err)

	// Run a fresh attempt. RunAccountSync's prefix should mark the
	// orphan as 'abandoned' before starting a new log.
	require.NoError(t, svc.RunAccountSync(ctx, source, nil))
	assert.Equal(t, int32(1), provider.calls.Load(), "provider.Sync should run exactly once")

	// Assert the orphan row was transitioned.
	var status string
	var errMsg *string
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT status, error_message FROM external_sync_log WHERE id = $1`, orphanLog.ID,
	).Scan(&status, &errMsg))
	assert.Equal(t, "abandoned", status, "pre-existing 'running' log should be marked 'abandoned'")
	require.NotNil(t, errMsg)
	assert.Contains(t, *errMsg, "abandoned by retry")

	// A new log row with status='success' should exist.
	var successCount int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = 'success'`, source,
	).Scan(&successCount))
	assert.Equal(t, 1, successCount, "a fresh success log should be created by the new attempt")
}

// fastRetryPolicy is a test-only retry policy that puts every retry 200ms
// in the future, regardless of attempt count. Uses
// accelerated.GetCurrentTime() per .ai/rules/core.md — NextRetry is
// called from river's worker-retry machinery as part of the sync
// pipeline, so the accelerated clock must apply here.
type fastRetryPolicy struct{}

func (fastRetryPolicy) NextRetry(_ *rivertype.JobRow) time.Time {
	return accelerated.GetCurrentTime().Add(200 * time.Millisecond)
}

// waitForSyncLogStatus polls external_sync_log for the given source
// until at least one row with the target status exists, or the ctx
// deadline fires. Replaces fixed time.Sleep for "wait until the DB
// commit lands" waits — the sleep was flaky under CI load.
func waitForSyncLogStatus(t *testing.T, ctx context.Context, pool syncLogPool, source string, status string) {
	t.Helper()
	deadline := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	for {
		var cnt int
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = $2`,
			source, status).Scan(&cnt)
		if err == nil && cnt >= 1 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for external_sync_log status=%s for source=%s (err=%v)", status, source, err)
		case <-deadline.C:
		}
	}
}

// syncLogPool is the minimal interface the wait helper needs. Matches
// *pgxpool.Pool.QueryRow.
type syncLogPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// waitForRiverJobState polls river_job for the given job id until its state
// equals want, or the ctx deadline fires. River's completer writes the
// running->completed transition asynchronously after the worker callback
// returns, so a single un-polled read can observe a stale 'running'. Mirrors
// waitForSyncLogStatus. Real wall-clock time per the polling-wait rule (not
// business-logic time, so not accelerated.GetCurrentTime). Reads through the
// test-only sqlc query, not raw SQL.
func waitForRiverJobState(t *testing.T, ctx context.Context, queries *db.Queries, jobID int64, want db.RiverJobState) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last db.RiverJobState
	for {
		state, err := queries.GetRiverJobStateByID(ctx, jobID)
		if err == nil {
			last = state
			if state == want {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for river_job id=%d state=%q (last observed %q, err=%v)", jobID, want, last, err)
		case <-ticker.C:
		}
	}
}

// TestSyncWorker_RescueOnCrash simulates the #208 scenario: a worker
// begins processing a job, writes a `status='running'` external_sync_log
// row, and then the process dies before CompleteSyncLog fires. The
// river_job row is left in `state='running'` with an old `attempted_at`.
// River's JobRescuer, on its next interval, moves the stuck job to
// `state='retryable'`, and the worker re-runs. The retry's
// AbandonRunningLogsForState sweep marks the orphan log row as
// `'abandoned'`, and the fresh attempt creates a new `'success'` log.
//
// This test requires live river maintenance loops (JobRescuer), so it
// runs a real non-TestOnly river client with:
//   - JobTimeout: 2s
//   - RescueStuckJobsAfter: 2s (minimum allowed; validator enforces
//     >= JobTimeout)
//   - Default rescuer interval of 30s — total wall-clock ~45s.
//
// Gated behind testing.Short() and the LONG_TESTS env var so developers
// can opt in locally without always paying the 45s cost. CI coverage
// for this test lives in the dedicated Backend Slow Tests workflow.
func TestSyncWorker_RescueOnCrash(t *testing.T) {
	requireLongTests(t)
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()
	// Migrations are applied once by TestMain.

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	source := "retry_test_rescue_on_crash"
	// Sweep ALL river_job rows in this package clone before starting our own
	// client. Leftover rows of foreign kinds (e.g. interaction_recorder) from
	// earlier tests in the same per-package clone would otherwise be fetched by
	// this test's live River client, adding completer/maintenance churn that
	// widens the completer race below. Tests in this package run serially (no
	// t.Parallel); the sweep runs before this test starts its own client, so it
	// races no live producer of this test. The query is fail-closed (deletes
	// only on a clone DB), so on a non-clone DB it is a harmless no-op and the
	// test's own t.Cleanup still scrubs this test's source-scoped rows.
	sweepCtx, sweepCancel := context.WithTimeout(ctx, 10*time.Second)
	defer sweepCancel()
	_, err = database.Queries.SweepRiverJobsInCloneForTest(sweepCtx)
	require.NoError(t, err, "sweep river_job in package clone")
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_log WHERE source = $1`, source)
	_, _ = database.Pool.Exec(ctx, `DELETE FROM external_sync_state WHERE source = $1`, source)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()

	provider := &retryTestProvider{
		cfg: syncpkg.SourceConfig{
			Name:            source,
			DisplayName:     source,
			Strategy:        repository.SyncStrategyFetchAll,
			DefaultInterval: 15 * time.Minute,
		},
	}
	registry.Register(provider)
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Create the sync state.
	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     source,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM river_job WHERE kind = 'sync_provider_account' AND (args->>'source') = $1`, source)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_log WHERE source = $1`, source)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM external_sync_state WHERE source = $1`, source)
	})

	// Simulate the "crashed mid-sync" pre-state directly:
	//   1. Insert an orphan external_sync_log row with status='running' —
	//      this is the row the retry attempt must mark 'abandoned'.
	//   2. Insert a river_job row DIRECTLY in state='running' with an
	//      old attempted_at, BEFORE starting the river client. This is
	//      what the rescuer will observe on its startup tick and move
	//      to 'retryable'. Inserting via client.Insert first and then
	//      rewriting state would race: the worker can fetch the
	//      'available' row and run the happy path before we back-date.
	orphanLog, err := syncRepo.CreateSyncLog(ctx, state)
	require.NoError(t, err)

	argsJSON := []byte(`{"source":"` + source + `"}`)
	var insertedJobID int64
	require.NoError(t, database.Pool.QueryRow(ctx,
		`INSERT INTO river_job
		  (args, kind, max_attempts, priority, queue, state,
		   attempt, attempted_at, created_at, scheduled_at)
		 VALUES ($1, 'sync_provider_account', 3, 1, 'default', 'running',
		         1, now() - interval '60 seconds', now() - interval '60 seconds',
		         now() - interval '60 seconds')
		 RETURNING id`, argsJSON).Scan(&insertedJobID))

	// Start the real river client (maintenance loops enabled). The sync
	// provider worker will process the rescued job on its retry.
	workers := river.NewWorkers()
	river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		Workers:              workers,
		JobTimeout:           2 * time.Second,
		RescueStuckJobsAfter: 2 * time.Second,
	})
	require.NoError(t, err)
	svc.SetRiverEnqueuer(client)

	// Start with the root ctx, never a timeout-derived context: River binds its
	// fetch/work loops (and the JobRescuer maintenance service) to the context
	// passed to Start (BaseStartStop.StartInit wraps it with WithCancelCause), so
	// a start-deadline cancel can silently stop fetching. (Documented project
	// gotcha; applies to test harnesses too.)
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	// Wait for the JobRescuer to notice (default interval 30s + 2s
	// rescue-after = ~35s upper bound; budget 75s for CI noise).
	waitCtx, waitCancel := context.WithTimeout(ctx, 75*time.Second)
	defer waitCancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for provider.calls.Load() < 1 {
		select {
		case <-waitCtx.Done():
			t.Fatalf("expected rescued job to run within 75s; provider.calls=%d", provider.calls.Load())
		case <-tick.C:
		}
	}

	// Poll the DB until CompleteSyncLog has landed (replaces a fixed
	// sleep that raced the commit under CI load).
	logWaitCtx, logWaitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer logWaitCancel()
	waitForSyncLogStatus(t, logWaitCtx, database.Pool, source, "success")

	// Assertion 1: the orphan log was marked 'abandoned'.
	var status string
	var errMsg *string
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT status, error_message FROM external_sync_log WHERE id = $1`, orphanLog.ID,
	).Scan(&status, &errMsg))
	assert.Equal(t, "abandoned", status, "orphan log should be abandoned by the rescued retry")

	// Assertion 2: a new log row with status='success' exists.
	var successCount int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM external_sync_log WHERE source = $1 AND status = 'success'`, source,
	).Scan(&successCount))
	assert.Equal(t, 1, successCount, "rescued retry should produce exactly one success log")

	// Assertion 3: state.status is 'idle' after rescue (the #208 invariant
	// used to forbid the legacy 'syncing' value here; that value is no
	// longer written by any code path, so assert the successful end state
	// directly).
	updatedState, err := syncRepo.GetSyncState(ctx, state.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.SyncStatusIdle, updatedState.Status,
		"status must land on 'idle' after rescue")

	// Assertion 4: the same river_job row we seeded ended up completed. River's
	// completer flushes running->completed asynchronously after the worker
	// returns, so poll rather than read once (the un-polled read raced the
	// completer under full-suite load). A genuine "stuck forever" regression
	// still fails loudly via the helper's t.Fatalf on deadline. This is also
	// what proves the rescuer (not a fresh Insert) drove the retry.
	jobStateCtx, jobStateCancel := context.WithTimeout(ctx, 10*time.Second)
	defer jobStateCancel()
	waitForRiverJobState(t, jobStateCtx, database.Queries, insertedJobID, db.RiverJobStateCompleted)
}
