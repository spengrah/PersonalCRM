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

func newAPIIsolatedTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
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
