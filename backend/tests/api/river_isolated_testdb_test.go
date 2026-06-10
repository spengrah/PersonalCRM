//go:build integration_testdb

package api

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/require"
)

// newIsolatedRiverTestDB opens a per-test clone for tests/api tests that start
// (or build a DB-wide-counting) River client. Sharing the package clone lets
// TestOnly clients steal each other's jobs from river_job and makes DB-wide
// river_job counts collide, so River-draining tests must isolate the whole DB.
// Mirrors backend/tests/river_isolated_testdb_test.go (package tests); the two
// are intentionally identical except for package + this comment.
func newIsolatedRiverTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()

	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	cfg.Database.MigrationsPath = getMigrationsPath()
	cfg.Database.MaxConns = 6
	cfg.Database.MinConns = 1
	cfg.River.WorkerConcurrency = 2

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	return database, cfg
}
