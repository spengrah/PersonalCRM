package tests

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncWorkerForEnqueueTests is a no-op worker registered only to satisfy
// river's "at least one worker" requirement at client construction. The
// tests manipulate river_job directly and do not let river fetch jobs,
// so this worker's Work method is never called.
type syncWorkerForEnqueueTests struct {
	river.WorkerDefaults[scheduler.SyncProviderAccountArgs]
}

func (*syncWorkerForEnqueueTests) Work(_ context.Context, _ *river.Job[scheduler.SyncProviderAccountArgs]) error {
	return nil
}

// enqueueArgs is a tiny helper that builds the args value the repo
// helper now requires as an explicit argument. Matches what
// service.EnqueueAccountSyncIfNotInFlight constructs in production.
func enqueueArgs(source string, accountID *string) scheduler.SyncProviderAccountArgs {
	return scheduler.SyncProviderAccountArgs{Source: source, AccountID: accountID}
}

// newEnqueueTestEnv stands up the shared DB and a pool-aware sync repo.
// Each test also builds its own river client via newJobEnqueuer so the
// tests stay isolated.
func newEnqueueTestEnv(t *testing.T) (*repository.SyncRepository, *db.Database) {
	t.Helper()
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

	repo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	// Clear river_job rows of our kind at the start of each test so prior
	// test runs don't pollute the in-flight check. Shared-DB integration
	// runs are the norm on CI.
	_, err = database.Pool.Exec(ctx, `DELETE FROM river_job WHERE kind = 'sync_provider_account'`)
	require.NoError(t, err)

	return repo, database
}

// seedRiverJob inserts a fake river_job row with the given state and
// (source, account_id) args so the in-flight check can observe it.
// Terminal states (cancelled, completed, discarded) require a non-null
// finalized_at per river's CHECK constraint; this helper populates it
// automatically so callers don't have to remember the invariant.
func seedRiverJob(t *testing.T, ctx context.Context, database *db.Database, state string, source string, accountID *string) {
	t.Helper()
	args := map[string]any{"source": source}
	if accountID != nil {
		args["account_id"] = *accountID
	}
	argsJSON, err := json.Marshal(args)
	require.NoError(t, err)

	var finalizedAtExpr string
	switch state {
	case "cancelled", "completed", "discarded":
		finalizedAtExpr = "NOW()"
	default:
		finalizedAtExpr = "NULL"
	}

	_, err = database.Pool.Exec(ctx,
		`INSERT INTO river_job (state, attempt, max_attempts, kind, args, queue, metadata, finalized_at)
		 VALUES ($1, 0, 25, 'sync_provider_account', $2::jsonb, 'default', '{}'::jsonb, `+finalizedAtExpr+`)`,
		state, argsJSON)
	require.NoError(t, err)
}

// newJobEnqueuer constructs a real *river.Client[pgx.Tx] that the repo
// helper can use. It's a separate helper from newEnqueueTestEnv so tests
// that do NOT need the client (e.g., the in-flight-skip tests that only
// call the helper after observing seeded rows) don't pay to build one.
func newJobEnqueuer(t *testing.T, database *db.Database) (repository.JobEnqueuer, func()) {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &syncWorkerForEnqueueTests{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return client, func() {}
}

func TestEnqueueAccountSyncIfNotInFlight_SkipsWhenInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "gmail"
	acct := "u1@example.com"

	// Seed a live 'running' row for the same (source, account_id).
	seedRiverJob(t, ctx, database, "running", source, &acct)

	enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, &acct, enqueueArgs(source, &acct))
	require.NoError(t, err)
	assert.False(t, enqueued, "helper must skip when an in-flight row already exists")

	// Only the seeded row exists; no new row inserted.
	var cnt int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'sync_provider_account'
		 AND (args->>'source') = $1 AND COALESCE(args->>'account_id', '') = COALESCE($2::text, '')`,
		source, acct,
	).Scan(&cnt))
	assert.Equal(t, 1, cnt)
}

func TestEnqueueAccountSyncIfNotInFlight_InsertsWhenClear(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "gmail"
	acct := "u2@example.com"

	enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, &acct, enqueueArgs(source, &acct))
	require.NoError(t, err)
	assert.True(t, enqueued, "helper must insert when no in-flight row exists")

	var cnt int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'sync_provider_account' AND state = 'available'
		 AND (args->>'source') = $1 AND COALESCE(args->>'account_id', '') = COALESCE($2::text, '')`,
		source, acct,
	).Scan(&cnt))
	assert.Equal(t, 1, cnt, "exactly one new available row expected")
}

func TestEnqueueAccountSyncIfNotInFlight_CompletedDoesNotBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "gmail"
	acct := "u3@example.com"

	// A completed row must not count as "in-flight". This is the
	// property that rules out river's default ByState dedup.
	seedRiverJob(t, ctx, database, "completed", source, &acct)

	enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, &acct, enqueueArgs(source, &acct))
	require.NoError(t, err)
	assert.True(t, enqueued, "a completed row must not block a new enqueue")
}

func TestEnqueueAccountSyncIfNotInFlight_CrossWindowOverlapPrevented(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "calendar"
	acct := "u4@example.com"

	// Seed a running row that has been running for longer than a single
	// 5-minute window — the failure mode that ByPeriod would miss.
	seedRiverJob(t, ctx, database, "running", source, &acct)
	_, err := database.Pool.Exec(ctx,
		`UPDATE river_job SET attempted_at = now() - interval '7 minutes'
		 WHERE kind = 'sync_provider_account' AND state = 'running'
		 AND (args->>'source') = $1`, source)
	require.NoError(t, err)

	enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, &acct, enqueueArgs(source, &acct))
	require.NoError(t, err)
	assert.False(t, enqueued,
		"cross-window overlap must be prevented (this is what ByPeriod would miss)")
}

func TestEnqueueAccountSyncIfNotInFlight_AtomicUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "todoist"
	acct := "race@example.com"

	// Spawn 50 goroutines all calling the helper concurrently for the
	// same (source, account_id). The advisory-lock + in-flight check +
	// InsertTx transaction must serialize them; exactly one must see
	// enqueued=true.
	const N = 50
	var wg sync.WaitGroup
	start := make(chan struct{})

	var successes int32
	var failures int32
	var errs []error
	var errsMu sync.Mutex

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all goroutines race from the same starting gate
			enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, &acct, enqueueArgs(source, &acct))
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
				return
			}
			if enqueued {
				atomic.AddInt32(&successes, 1)
			} else {
				atomic.AddInt32(&failures, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	errsMu.Lock()
	assert.Empty(t, errs, "no goroutine should error (got %d errors, first: %v)", len(errs), firstErr(errs))
	errsMu.Unlock()

	assert.Equal(t, int32(1), atomic.LoadInt32(&successes),
		"exactly one goroutine must see enqueued=true")
	assert.Equal(t, int32(N-1), atomic.LoadInt32(&failures),
		"the other %d goroutines must see enqueued=false", N-1)

	var cnt int
	require.NoError(t, database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'sync_provider_account'
		 AND (args->>'source') = $1 AND COALESCE(args->>'account_id', '') = COALESCE($2::text, '')`,
		source, acct,
	).Scan(&cnt))
	assert.Equal(t, 1, cnt, "exactly one river_job row should exist for this (source, account_id)")
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// TestEnqueueAccountSyncIfNotInFlight_NilAccountID covers the nil-account
// path (single-account providers like iMessage). The COALESCE in the SQL
// must treat nil and empty as the same bucket.
func TestEnqueueAccountSyncIfNotInFlight_NilAccountID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	repo, database := newEnqueueTestEnv(t)
	enqueuer, cleanup := newJobEnqueuer(t, database)
	defer cleanup()

	ctx := context.Background()
	source := "imessage"

	// First call: inserts.
	enqueued, err := repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, nil, enqueueArgs(source, nil))
	require.NoError(t, err)
	assert.True(t, enqueued)

	// Second call: sees the in-flight row and skips.
	enqueued, err = repo.EnqueueAccountSyncIfNotInFlight(ctx, enqueuer, source, nil, enqueueArgs(source, nil))
	require.NoError(t, err)
	assert.False(t, enqueued)
}
