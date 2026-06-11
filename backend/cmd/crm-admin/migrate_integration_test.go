//go:build integration_testdb

// End-to-end exit-code contract for crm-admin --migrate / --migrate-check,
// driving the REAL runMigrate / runMigrateCheck (which call db.MigrationStatus /
// db.RunMigrations) against a real golang-migrate + River clone. This proves the
// 0/1/2 exit-code contract deploy-artifact.sh depends on end-to-end, not just the
// boolean MigrationStatus or the stubbed exit-code mapping in main_test.go.
//
// These mutate the schema (roll migrations down to manufacture a pending state),
// so each uses an ISOLATED per-test clone via testdb.NewEphemeralClone — never
// the shared package DB. Migration-subject tests stay serial (no t.Parallel()).
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
)

// TestMain clones a per-package template database (testdb.SetupPackage) and
// rewrites DATABASE_URL to the clone before running this package's tagged tests.
// Excluded from the no-tag unit build, so main_test.go's DB-free unit tests are
// unaffected.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForTest())))
}

// migrationsPathForTest resolves the migrations dir relative to this test file
// (cmd/crm-admin → ../../migrations), honoring an absolute MIGRATIONS_PATH
// override like the other integration packages.
func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// newMigrateClone returns a fresh fully-migrated isolated clone URL + migrations
// path. The testdb template already applied all migrations, so --migrate-check
// starts up-to-date.
func newMigrateClone(t *testing.T) (cloneURL, migrationsPath string) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	return cloneURL, migrationsPathForTest()
}

// TestMigrateCheckExitContract drives the real runMigrateCheck end-to-end and
// asserts the 0 / 2 exit-code contract against a real DB.
func TestMigrateCheckExitContract(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrateClone(t)

	// Call before any read (CI bare-Postgres gotcha; here a no-op backstop).
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	// Up-to-date → nil (exit 0).
	var out bytes.Buffer
	err := runMigrateCheck(ctx, cloneURL, migrationsPath, &out)
	require.NoError(t, err, "fully-migrated clone must report exit 0")
	require.Contains(t, out.String(), "app_pending=0 river_pending=0")

	// Roll the app schema down one step → pending → exitErr{code:2}.
	rollAppDownOne(t, cloneURL, migrationsPath)

	out.Reset()
	err = runMigrateCheck(ctx, cloneURL, migrationsPath, &out)
	var ee exitErr
	require.ErrorAs(t, err, &ee, "pending DB must return an exitErr")
	require.Equal(t, migrateExitPending, ee.code, "pending DB must map to exit 2")
	require.Contains(t, out.String(), "app_pending=1")
}

// TestMigrateAppliesThenCheckClean drives the real runMigrate end-to-end: after a
// real --migrate the DB is up-to-date, so --migrate-check returns exit 0. Also
// confirms --migrate is idempotent (a second apply is a clean no-op).
func TestMigrateAppliesThenCheckClean(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrateClone(t)

	// Roll the app schema down one step so --migrate has real work to do.
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))
	rollAppDownOne(t, cloneURL, migrationsPath)

	// --migrate applies the pending migration (exit 0) and prints its summary.
	var out bytes.Buffer
	require.NoError(t, runMigrate(ctx, cloneURL, migrationsPath, &out))
	require.Contains(t, out.String(), "migrations applied")

	// --migrate is idempotent: a second apply is a clean no-op.
	out.Reset()
	require.NoError(t, runMigrate(ctx, cloneURL, migrationsPath, &out))

	// --migrate-check now reports up-to-date (exit 0).
	out.Reset()
	require.NoError(t, runMigrateCheck(ctx, cloneURL, migrationsPath, &out))
	require.Contains(t, out.String(), "app_pending=0 river_pending=0")
}

// rollAppDownOne rolls the golang-migrate app schema down by exactly one
// migration (no raw SQL — uses the migrate library).
func rollAppDownOne(t *testing.T, databaseURL, migrationsPath string) {
	t.Helper()
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), databaseURL)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "roll app schema down one step")
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
}
