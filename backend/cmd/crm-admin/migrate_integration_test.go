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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
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

// TestMigrateCheckRiverPendingExit2 drives the real runMigrateCheck end-to-end
// against a clone whose River schema is one migration behind (app fully applied),
// asserting River-pending maps to exit 2.
func TestMigrateCheckRiverPendingExit2(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrateClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	rollRiverDownOne(t, ctx, cloneURL)

	var out bytes.Buffer
	err := runMigrateCheck(ctx, cloneURL, migrationsPath, &out)
	var ee exitErr
	require.ErrorAs(t, err, &ee, "River-pending DB must return an exitErr")
	require.Equal(t, migrateExitPending, ee.code, "River-pending DB must map to exit 2")
	require.Contains(t, out.String(), "river_pending=1")
	require.Contains(t, out.String(), "app_pending=0")
}

// TestMigrateCheckDirtyExit1 drives the real runMigrateCheck end-to-end against a
// clone forced into a dirty golang-migrate state, asserting the operational-error
// mapping to exit 1 (a plain error, NOT an exitErr{code:2}).
func TestMigrateCheckDirtyExit1(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrateClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	forceAppMigrationDirty(t, ctx, cloneURL, migrationsPath)

	var out bytes.Buffer
	err := runMigrateCheck(ctx, cloneURL, migrationsPath, &out)
	require.Error(t, err, "a dirty DB must surface as an operational error")
	require.ErrorIs(t, err, db.ErrDirtyMigration, "dirty error must wrap ErrDirtyMigration")
	var ee exitErr
	require.False(t, errors.As(err, &ee), "a dirty DB must map to exit 1, NOT exitErr{code:2}")
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

// rollRiverDownOne rolls River's migration schema down by exactly one version
// using River's own migrator (no raw SQL).
func rollRiverDownOne(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	poolCfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	require.NoError(t, err)
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionDown, &rivermigrate.MigrateOpts{MaxSteps: 1})
	require.NoError(t, err)
	require.Len(t, res.Versions, 1, "expected exactly one River migration rolled down")
}

// forceAppMigrationDirty marks the golang-migrate schema_migrations row dirty at
// the current applied version via the postgres database driver's SetVersion — a
// library call (no raw SQL). Uses the pgx stdlib sql driver to open the *sql.DB
// the migrate postgres driver wraps.
func forceAppMigrationDirty(t *testing.T, ctx context.Context, databaseURL, migrationsPath string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", databaseURL)
	require.NoError(t, err)
	defer func() { _ = sqlDB.Close() }()
	require.NoError(t, sqlDB.PingContext(ctx))

	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	require.NoError(t, err)

	current, dirty, err := driver.Version()
	require.NoError(t, err)
	require.False(t, dirty, "clone should start clean")
	require.NotEqual(t, -1, current, "clone should have an applied app version")
	require.NoError(t, driver.SetVersion(current, true))

	// Sanity: a fresh migrate instance now reads dirty=true.
	verify, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), databaseURL)
	require.NoError(t, err)
	defer func() { _, _ = verify.Close() }()
	_, gotDirty, verr := verify.Version()
	require.NoError(t, verr)
	require.True(t, gotDirty, "expected the forced dirty marker to be visible")
}
