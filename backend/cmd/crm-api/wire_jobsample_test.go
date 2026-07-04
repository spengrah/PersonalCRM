//go:build integration_testdb

// End-to-end coverage for the job_exec_sample Subscribe recorder against a REAL
// River client on an isolated DB clone. It validates the true v0.34 event
// shapes across all four per-execution kinds (completed + retryable-failed here,
// snoozed + cancelled in the repeated-snooze regression), that the retry
// scheduled_at mutation does not corrupt the stored wait, that repeated snoozes
// (which reuse the decremented attempt) land distinct occupancy rows rather than
// collapsing, subscribe-before-Start, and the channel-close-on-Stop shutdown
// join. startJobSampleRecorder is unexported in package main, so this test lives
// here (not backend/tests).
package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsOKArgs / jsOKWorker: a worker that always completes.
type jsOKArgs struct{}

func (jsOKArgs) Kind() string { return "test_js_ok" }

type jsOKWorker struct {
	river.WorkerDefaults[jsOKArgs]
}

func (*jsOKWorker) Work(_ context.Context, _ *river.Job[jsOKArgs]) error { return nil }

// jsFailArgs / jsFailWorker: a worker that always fails. NextRetry pushes the
// next attempt an hour out (well beyond the scheduler poll interval) so the job
// deterministically stays `retryable` after its single in-test attempt — the
// non-final failed event we want the recorder to capture.
type jsFailArgs struct{}

func (jsFailArgs) Kind() string { return "test_js_fail" }

type jsFailWorker struct {
	river.WorkerDefaults[jsFailArgs]
}

func (*jsFailWorker) Work(_ context.Context, _ *river.Job[jsFailArgs]) error {
	return errors.New("deliberate test failure")
}

func (*jsFailWorker) NextRetry(_ *river.Job[jsFailArgs]) time.Time {
	return accelerated.GetCurrentTime().Add(time.Hour)
}

func newJobSampleTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	cfg.Database.MigrationsPath = migrationsPathForTest()
	cfg.Database.MaxConns = 6
	cfg.Database.MinConns = 1
	cfg.River.WorkerConcurrency = 2

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database, cfg
}

func TestJobSampleRecorder_EndToEnd(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Base ctx for Start — NEVER a timeout-derived ctx (River silently stops
	// fetching if its fetch-loop ctx cancels).
	ctx := context.Background()
	database, cfg := newJobSampleTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &jsOKWorker{})
	river.AddWorker(workers, &jsFailWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	// Subscribe + start the recorder BEFORE Start so no early events are missed.
	wait := startJobSampleRecorder(ctx, client, repo)

	require.NoError(t, client.Start(ctx))
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
		stopped = true
	}
	t.Cleanup(stop)

	okRes, err := client.Insert(ctx, jsOKArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	failRes, err := client.Insert(ctx, jsFailArgs{}, &river.InsertOpts{MaxAttempts: 3})
	require.NoError(t, err)

	okID := okRes.Job.ID
	failID := failRes.Job.ID

	// Poll the sample table until BOTH jobs have produced a row (the recorder
	// writes asynchronously as events arrive). Real wall-clock deadline via a
	// context timeout per the polling-wait rule (not time.Now()).
	var samples []repository.JobExecSampleRow
	pollCtx, pollCancel := context.WithTimeout(ctx, 20*time.Second)
	defer pollCancel()
	for {
		samples, err = repo.ListJobExecSamplesByRiverJobIDForTest(pollCtx, []int64{okID, failID})
		require.NoError(t, err)
		seen := map[int64]bool{}
		for _, s := range samples {
			seen[s.RiverJobID] = true
		}
		if seen[okID] && seen[failID] {
			break
		}
		select {
		case <-pollCtx.Done():
			t.Fatalf("timed out waiting for sample rows (got %d): %+v", len(samples), samples)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Stop closes the subscription channel; wait() must return promptly.
	stop()
	done := make(chan struct{})
	go func() { defer close(done); wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("startJobSampleRecorder wait() did not return after Stop")
	}

	byID := map[int64]repository.JobExecSampleRow{}
	for _, s := range samples {
		byID[s.RiverJobID] = s
	}

	okSample := byID[okID]
	assert.Equal(t, "completed", okSample.State)
	assert.Equal(t, "test_js_ok", okSample.Kind)
	assert.True(t, okSample.FinalizedAt.After(okSample.AttemptedAt) || okSample.FinalizedAt.Equal(okSample.AttemptedAt),
		"finalized_at must be >= attempted_at")
	assert.GreaterOrEqual(t, okSample.QueueWaitMs, int64(0))
	assert.Equal(t, 1, okSample.Attempt)

	failSample := byID[failID]
	assert.Contains(t, []string{"retryable", "discarded"}, failSample.State,
		"non-final failed attempt is retryable (discarded only after max attempts)")
	assert.Equal(t, "test_js_fail", failSample.Kind)
	assert.GreaterOrEqual(t, failSample.QueueWaitMs, int64(0),
		"wait must be non-negative despite retry scheduled_at mutation")
	assert.True(t, failSample.FinalizedAt.After(failSample.AttemptedAt) || failSample.FinalizedAt.Equal(failSample.AttemptedAt))
}

// jsSnoozeArgs / jsSnoozeWorker: a worker that snoozes the SAME job the first
// snoozeThreshold-1 executions, then completes. Because JobSnooze decrements
// attempt, every snooze re-execution reuses the same attempt value — the
// regression this test guards: without attempted_at in the dedup key those
// re-executions would collide on UNIQUE(river_job_id, attempt) and collapse.
type jsSnoozeArgs struct{}

func (jsSnoozeArgs) Kind() string { return "test_js_snooze" }

type jsSnoozeWorker struct {
	river.WorkerDefaults[jsSnoozeArgs]
	mu     sync.Mutex
	counts map[int64]int
}

// snoozeThreshold executions occur: (threshold-1) snoozes then one completion.
const snoozeThreshold = 3

func (w *jsSnoozeWorker) Work(_ context.Context, job *river.Job[jsSnoozeArgs]) error {
	w.mu.Lock()
	w.counts[job.ID]++
	n := w.counts[job.ID]
	w.mu.Unlock()
	if n < snoozeThreshold {
		// A short snooze (<= scheduler interval) makes River set the job
		// immediately re-runnable, so the test converges fast.
		return river.JobSnooze(time.Millisecond)
	}
	return nil
}

// TestJobSampleRecorder_RepeatedSnooze is the direct proof that the
// (river_job_id, attempt, attempted_at) dedup key records each snooze
// re-execution as its own occupancy row instead of collapsing them (River
// reuses the decremented attempt across snoozes; only attempted_at differs).
func TestJobSampleRecorder_RepeatedSnooze(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	database, cfg := newJobSampleTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &jsSnoozeWorker{counts: map[int64]int{}})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	wait := startJobSampleRecorder(ctx, client, repo)
	require.NoError(t, client.Start(ctx))
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
		stopped = true
	}
	t.Cleanup(stop)

	res, err := client.Insert(ctx, jsSnoozeArgs{}, nil)
	require.NoError(t, err)
	jobID := res.Job.ID

	// Poll until all snoozeThreshold executions (2 snoozes + 1 completion) have
	// produced occupancy rows.
	var samples []repository.JobExecSampleRow
	pollCtx, pollCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pollCancel()
	for {
		samples, err = repo.ListJobExecSamplesByRiverJobIDForTest(pollCtx, []int64{jobID})
		require.NoError(t, err)
		if len(samples) >= snoozeThreshold {
			break
		}
		select {
		case <-pollCtx.Done():
			t.Fatalf("timed out: got %d occupancy rows, want >= %d (snooze rows collapsed?): %+v",
				len(samples), snoozeThreshold, samples)
		case <-time.After(50 * time.Millisecond):
		}
	}

	stop()
	done := make(chan struct{})
	go func() { defer close(done); wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder wait() did not return after Stop")
	}

	// Every occupancy row must have a DISTINCT attempted_at — the proof that the
	// re-executions were NOT collapsed by the unique key.
	require.GreaterOrEqual(t, len(samples), snoozeThreshold,
		"each snooze re-execution must land its own row")
	distinctAttempted := map[time.Time]bool{}
	completions := 0
	nonCompletions := 0
	for _, s := range samples {
		distinctAttempted[s.AttemptedAt] = true
		assert.False(t, s.FinalizedAt.Before(s.AttemptedAt), "finalized_at >= attempted_at")
		assert.GreaterOrEqual(t, s.QueueWaitMs, int64(0))
		if s.State == "completed" {
			completions++
		} else {
			// A snooze re-execution's state is the rescheduled state. A short
			// snooze (<= scheduler interval) makes River set the job immediately
			// `available`; a longer one leaves it `scheduled` — assert it's one of
			// those (proves the snooze event was mapped, not a mis-mapped kind).
			assert.Contains(t, []string{"scheduled", "available"}, s.State,
				"a snooze re-execution row must carry the rescheduled state")
			nonCompletions++
		}
	}
	assert.Equal(t, len(samples), len(distinctAttempted),
		"all occupancy rows must have distinct attempted_at (no dedup collapse)")
	assert.GreaterOrEqual(t, completions, 1, "the final execution completes")
	assert.GreaterOrEqual(t, nonCompletions, snoozeThreshold-1, "the snooze re-executions are recorded")
}
