//go:build integration_testdb

package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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

// These exercise db.MigrationStatus and the crm-admin --migrate / --migrate-check
// exit-code contract against REAL golang-migrate + River, NOT a unit stub. They
// mutate the schema (roll migrations down to manufacture a pending/dirty state),
// so each uses an ISOLATED per-test clone via testdb.NewEphemeralClone — never
// the shared package DB, whose schema a half-rewound migration would corrupt for
// sibling tests. Migration-subject tests stay serial (no t.Parallel()).

// newMigrationStatusClone returns a fresh, fully-migrated isolated clone URL plus
// the migrations path. The testdb template already applied all app + River
// migrations into the clone, so MigrationStatus starts up-to-date.
func newMigrationStatusClone(t *testing.T) (cloneURL, migrationsPath string) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	return cloneURL, getMigrationsPath()
}

func TestMigrationStatus_UpToDate(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)

	// The clone is template-migrated; RunMigrations is a no-op backstop and must
	// stay clean per the CI bare-Postgres gotcha (call it before any read).
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.False(t, appPending, "fully-migrated clone must report no app migrations pending")
	require.False(t, riverPending, "fully-migrated clone must report no River migrations pending")
}

func TestMigrationStatus_AppPending(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	// Roll the app schema down by exactly one migration, so the latest app
	// migration is unapplied. This proves the golang-migrate version-comparison
	// path against real migrations (not a stub).
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "roll app schema down one step")
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.True(t, appPending, "after rolling down one app migration, app should be pending")
	require.False(t, riverPending, "River was not touched, so it should report up-to-date")

	// Re-applying restores up-to-date — confirms the down/up symmetry holds and
	// MigrationStatus flips back to false.
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))
	appPending, _, err = db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.False(t, appPending, "after re-applying, app should be up-to-date again")
}

func TestMigrationStatus_RiverPending(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	// Roll River's schema down by one migration via its own migrator, leaving the
	// app schema fully applied. This proves the rivermigrate DryRun read detects a
	// DB missing River's latest version.
	rollRiverDownOne(t, ctx, cloneURL)

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.False(t, appPending, "app schema was not touched, so it should be up-to-date")
	require.True(t, riverPending, "after rolling River down one migration, River should be pending")

	// Re-applying River migrations restores up-to-date.
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))
	_, riverPending, err = db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.False(t, riverPending, "after re-applying River migrations, River should be up-to-date")
}

func TestMigrationStatus_DirtyIsOperationalError(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	// Force the golang-migrate schema_migrations row into a dirty state via the
	// database driver's SetVersion (a library call, not raw SQL). A dirty DB must
	// be surfaced as an operational error (→ exit 1), NEVER as "pending" (exit 2).
	forceAppMigrationDirty(t, ctx, cloneURL)

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.Error(t, err, "a dirty DB must surface as an operational error")
	require.ErrorIs(t, err, db.ErrDirtyMigration, "dirty error must wrap ErrDirtyMigration")
	require.False(t, appPending)
	require.False(t, riverPending)
}

func TestMigrate_Idempotent(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)

	// First apply (clone is already template-migrated, so this is the no-op
	// backstop path), then a second apply must also be a clean no-op.
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath), "re-running --migrate must be a clean no-op")

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.False(t, appPending)
	require.False(t, riverPending)
}

// TestMigrationStatus_FreshDBNonMutating proves MigrationStatus is strictly
// non-mutating on a fresh (never-migrated) DB: it reports pending WITHOUT
// creating golang-migrate's schema_migrations tracking table. golang-migrate's
// Postgres driver creates that table at migrate.New time, so a naive read would
// mutate a fresh DB — the read-only to_regclass short-circuit must prevent it.
func TestMigrationStatus_FreshDBNonMutating(t *testing.T) {
	ctx := context.Background()
	cloneURL, migrationsPath := newMigrationStatusClone(t)
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))

	// Drop ALL tables (app + River + schema_migrations) via the migrate library
	// to manufacture a genuinely fresh DB — no raw SQL.
	dropAllTables(t, cloneURL, migrationsPath)
	require.False(t, schemaMigrationsTableExists(t, ctx, cloneURL),
		"precondition: schema_migrations must be absent on the fresh DB")

	appPending, riverPending, err := db.MigrationStatus(ctx, cloneURL, migrationsPath)
	require.NoError(t, err)
	require.True(t, appPending, "a fresh DB with source migrations must report app pending")
	require.True(t, riverPending, "a fresh DB must report River pending")

	// The load-bearing assertion: the status read must NOT have re-created the
	// tracking table.
	require.False(t, schemaMigrationsTableExists(t, ctx, cloneURL),
		"MigrationStatus must not create schema_migrations on a fresh DB (non-mutating contract)")
}

// dropAllTables drops every table (app + River + schema_migrations) via the
// migrate library's Drop, manufacturing a genuinely fresh DB without raw SQL.
func dropAllTables(t *testing.T, databaseURL, migrationsPath string) {
	t.Helper()
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), databaseURL)
	require.NoError(t, err)
	require.NoError(t, m.Drop(), "drop all tables to manufacture a fresh DB")
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
}

// schemaMigrationsTableExists reports whether golang-migrate's schema_migrations
// table is present, via a read-only to_regclass catalog lookup (no raw DDL). Used
// to assert the fresh-DB non-mutating contract in
// TestMigrationStatus_FreshDBNonMutating.
func schemaMigrationsTableExists(t *testing.T, ctx context.Context, databaseURL string) bool {
	t.Helper()
	pool, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer pool.Close()
	var regclass *string
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations')::text").Scan(&regclass))
	return regclass != nil
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
func forceAppMigrationDirty(t *testing.T, ctx context.Context, databaseURL string) {
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

	// SetVersion(version, dirty=true) writes the dirty marker the migrate library
	// itself uses to signal a failed mid-apply state.
	require.NoError(t, driver.SetVersion(current, true))

	// Sanity: a fresh migrate instance now reads dirty=true. Version() reads the
	// marker straight from the driver, so it returns (version, true, nil) — the
	// ErrDirty sentinel is only produced by Up/Steps, not Version.
	verify, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), databaseURL)
	require.NoError(t, err)
	defer func() { _, _ = verify.Close() }()
	_, gotDirty, verr := verify.Version()
	require.NoError(t, verr, "Version() should read the dirty marker without an error")
	require.True(t, gotDirty, "expected the forced dirty marker to be visible")
}
