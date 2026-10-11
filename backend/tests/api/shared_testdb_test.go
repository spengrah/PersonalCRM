//go:build integration_testdb

package api

import (
	"context"
	"personal-crm/backend/tests/testsupport"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"

	"github.com/stretchr/testify/require"
)

func newAPISharedTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()

	databaseURL := testsupport.RequireDatabase(t)

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	return database, cfg
}
