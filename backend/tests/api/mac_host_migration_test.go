//go:build integration_testdb

package api

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

// TestMacHostMigrations verifies the up + down behaviour of migrations
// 047 and 048:
//
//   - 047 up creates the mac_host + mac_host_pairing_token tables with
//     the partial singleton unique index.
//   - 048 up widens the external_sync_state.strategy CHECK to include
//     'push'.
//   - 048 down refuses to revert when any row still uses strategy='push'
//     (data-loss guard).
//   - After deleting the push rows, 048 down reverts the CHECK and
//     re-rejects new push inserts.
//   - 047 down drops the two tables.
//
// The test uses a dedicated migration helper that walks the migrations
// dir step-by-step (rather than the shared TestMain `db.RunMigrations`)
// so it can exercise individual up/down boundaries. It runs against its
// own ephemeral clone (testdb.NewEphemeralClone), so the mid-test schema
// rollback cannot affect the package clone or sibling packages, and the
// clone is dropped wholesale on cleanup.
func TestMacHostMigrations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Dedicated ephemeral clone: this test rolls the schema down to 046 and
	// re-applies 047/048. On its own clone the rollback is harmless, and the
	// clone is dropped on cleanup — no HEAD-restore needed.
	databaseURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	ctx := context.Background()

	// Roll down to 046 so we can re-apply 047 + 048 from scratch.
	mig, err := newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Migrate(46), "migrate to 046")
	closeMigrator(t, mig)

	// Apply 047 — tables exist, singleton index enforces one row.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(1), "047 up")
	closeMigrator(t, mig)

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	// Insert a host, then attempt a second one — singleton blocks.
	_, err = database.Queries.SeedMacHost(ctx, db.SeedMacHostParams{
		Hostname:        "mig-test-host-1",
		DaemonVersion:   "0.0.0",
		ProtocolVersion: 1,
		ApiKeyHash:      "$2a$04$abc",
	})
	require.NoError(t, err)
	_, err = database.Queries.SeedMacHost(ctx, db.SeedMacHostParams{
		Hostname:        "mig-test-host-2",
		DaemonVersion:   "0.0.0",
		ProtocolVersion: 1,
		ApiKeyHash:      "$2a$04$def",
	})
	require.Error(t, err, "second non-revoked host must violate the singleton index")

	// Apply 048 — push strategy accepted.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(1), "048 up")
	closeMigrator(t, mig)

	pushSource := "mig-push-" + fmt.Sprint(accelerated.GetCurrentTime().UnixNano())
	_, err = database.Queries.SeedExternalSyncState(ctx, db.SeedExternalSyncStateParams{
		Source:     pushSource,
		AccountID:  pgtype.Text{Valid: false},
		Enabled:    true,
		Status:     "idle",
		Strategy:   "push",
		NextSyncAt: pgtype.Timestamptz{Valid: false},
	})
	require.NoError(t, err, "push strategy must be accepted after 048 up")

	// 048 down with a live push row → guard raises. The migrate
	// library marks the migration as "dirty" on failure; call
	// Force(48) to clear that flag so subsequent up/down steps
	// proceed cleanly.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	downErr := mig.Steps(-1)
	require.Error(t, downErr, "048 down must fail while a strategy='push' row exists")
	require.NoError(t, mig.Force(48), "clear dirty migration flag")
	closeMigrator(t, mig)

	// Delete the push row, then 048 down succeeds.
	states, err := database.Queries.ListSyncStates(ctx)
	require.NoError(t, err)
	for _, s := range states {
		if s.Source == pushSource {
			require.NoError(t, database.Queries.DeleteSyncState(ctx, s.ID))
		}
	}
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(-1), "048 down with no push rows must succeed")
	closeMigrator(t, mig)

	// Inserting a push row must now be rejected.
	_, err = database.Queries.SeedExternalSyncState(ctx, db.SeedExternalSyncStateParams{
		Source:     "mig-push-after-down",
		AccountID:  pgtype.Text{Valid: false},
		Enabled:    true,
		Status:     "idle",
		Strategy:   "push",
		NextSyncAt: pgtype.Timestamptz{Valid: false},
	})
	require.Error(t, err, "push strategy must be rejected after 048 down")

	// Clean leftover hosts before 047 down (the migration drops the
	// tables; no data integrity required, but keeps the cleanup
	// path simple).
	_, _ = database.Queries.DeleteAllMacHosts(ctx)

	// 047 down — tables dropped.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(-1), "047 down")
	closeMigrator(t, mig)

	// Subsequent SeedMacHost will fail because the table no longer
	// exists — confirm.
	_, err = database.Queries.SeedMacHost(ctx, db.SeedMacHostParams{
		Hostname:        "post-drop",
		DaemonVersion:   "0.0.0",
		ProtocolVersion: 1,
		ApiKeyHash:      "$2a$04$xyz",
	})
	require.Error(t, err, "mac_host table should be gone after 047 down")
}

func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	migrationsPath, err := filepath.Abs(getMigrationsPath())
	if err != nil {
		return nil, err
	}
	return migrate.New("file://"+migrationsPath, databaseURL)
}

func closeMigrator(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	srcErr, dbErr := m.Close()
	if srcErr != nil {
		t.Logf("migrator close (source): %v", srcErr)
	}
	if dbErr != nil {
		t.Logf("migrator close (db): %v", dbErr)
	}
}
