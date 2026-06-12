//go:build integration_testdb

// End-to-end coverage for crm-admin --list-jobs / --retry-job, driving the REAL
// run() dispatch against a real River client + isolated DB clone. Proves the
// list output, --job-limit → JobListParams.First propagation, --job-state
// filtering, and the post-retry river_job state — none of which the unit fakes
// (opaque JobListParams) can see.
//
// Each test starts a River client against an ISOLATED per-test clone via
// testdb.NewEphemeralClone (River-draining tests must own a private river_job,
// or clients steal each other's jobs). t.Parallel() on the clone.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// jobsTestFailArgs is a test-only worker type whose worker always fails, so a
// MaxAttempts=1 job lands directly in `discarded` on its single attempt.
type jobsTestFailArgs struct{}

func (jobsTestFailArgs) Kind() string { return "test_admin_jobs_fail" }

type jobsTestFailWorker struct {
	river.WorkerDefaults[jobsTestFailArgs]
}

func (*jobsTestFailWorker) Work(_ context.Context, _ *river.Job[jobsTestFailArgs]) error {
	return errors.New("deliberate test failure")
}

// newJobsTestDB opens an isolated clone + db.Database for a River-draining admin
// test. Mirrors newIsolatedRiverTestDB in backend/tests (test helpers don't
// cross package boundaries in this repo).
func newJobsTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
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

// waitForJobState polls river_job for the given id until its state equals want
// or the ctx deadline fires. Real wall-clock per the polling-wait rule; reads
// through the test-only sqlc query, not raw SQL.
func waitForJobState(t *testing.T, ctx context.Context, queries *db.Queries, id int64, want db.RiverJobState) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var last db.RiverJobState
	for {
		state, err := queries.GetRiverJobStateByID(ctx, id)
		if err == nil {
			last = state
			if state == want {
				return
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("timed out waiting for river_job id=%d state=%q (last %q, err=%v)", id, want, last, err)
		case <-tick.C:
		}
	}
}

func TestListAndRetryJobs_Integration(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	database, cfg := newJobsTestDB(t, ctx)

	workers := river.NewWorkers()
	river.AddWorker(workers, &jobsTestFailWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: workers,
	})
	require.NoError(t, err)
	require.NoError(t, client.Start(ctx))
	clientStopped := false
	stopClient := func() {
		if clientStopped {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
		clientStopped = true
	}
	t.Cleanup(stopClient)

	// Drive TWO single-attempt jobs to `discarded` (MaxAttempts=1 → one failure
	// discards immediately).
	jobIDs := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		res, ierr := client.Insert(ctx, jobsTestFailArgs{}, &river.InsertOpts{MaxAttempts: 1})
		require.NoError(t, ierr)
		jobIDs = append(jobIDs, res.Job.ID)
	}
	for _, id := range jobIDs {
		waitForJobState(t, ctx, database.Queries, id, db.RiverJobStateDiscarded)
	}

	// Stop the client so it no longer competes for the jobs we list/retry.
	stopClient()

	out := &bytes.Buffer{}
	deps := adminDeps{jobs: client, stdout: out, stderr: &bytes.Buffer{}}

	// (1) --list-jobs (default discarded,retryable) shows BOTH discarded rows.
	require.NoError(t, run(ctx, runOptions{listJobs: true, jobState: "discarded,retryable", jobLimit: 100}, deps))
	listed := out.String()
	for _, id := range jobIDs {
		require.Contains(t, listed, "id="+strconv.FormatInt(id, 10))
	}
	require.Contains(t, listed, "kind=test_admin_jobs_fail")
	require.Contains(t, listed, "state=discarded")
	require.Contains(t, listed, "attempt=1/1")
	require.Contains(t, listed, "deliberate test failure")

	// (2) --list-jobs --job-limit 1 returns exactly one row + the limit-reached
	// note (proves --job-limit → JobListParams.First end-to-end).
	out.Reset()
	require.NoError(t, run(ctx, runOptions{listJobs: true, jobState: "discarded", jobLimit: 1}, deps))
	limited := out.String()
	require.Equal(t, 1, strings.Count(limited, "id="), "exactly one job row expected")
	require.Contains(t, limited, "note: limit reached (1 rows shown)")

	// (3) --list-jobs --job-state retryable filters the discarded jobs out.
	out.Reset()
	require.NoError(t, run(ctx, runOptions{listJobs: true, jobState: "retryable", jobLimit: 100}, deps))
	require.Contains(t, out.String(), "no jobs found (states=retryable)")

	// (4) --retry-job <id> makes the first job available again.
	retryID := jobIDs[0]
	out.Reset()
	require.NoError(t, run(ctx, runOptions{retryJobID: retryID}, deps))
	require.Contains(t, out.String(), "id="+strconv.FormatInt(retryID, 10))
	require.Contains(t, out.String(), "retried job")
	// The row is now re-workable (River bumps max_attempts on an exhausted job).
	state, serr := database.Queries.GetRiverJobStateByID(ctx, retryID)
	require.NoError(t, serr)
	require.Equal(t, db.RiverJobStateAvailable, state, "retried job should be available")

	// (5) --retry-job <unknown> → friendly not-found error mentioning the id.
	out.Reset()
	err = run(ctx, runOptions{retryJobID: 999999999}, deps)
	require.Error(t, err)
	require.Contains(t, err.Error(), "999999999")
}
