package tests

import (
	"context"
	"os"
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
// worker defines its own to avoid coupling to main.
type localNoopJobArgs struct{}

// Kind implements river.JobArgs.
func (localNoopJobArgs) Kind() string { return "test_noop" }

type localNoopWorker struct {
	river.WorkerDefaults[localNoopJobArgs]
}

func (*localNoopWorker) Work(_ context.Context, _ *river.Job[localNoopJobArgs]) error {
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
