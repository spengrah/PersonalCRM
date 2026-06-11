package db

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// ErrDirtyMigration reports that golang-migrate's schema_migrations row is
// marked dirty (a prior migration failed mid-apply). This needs manual
// intervention — callers must NOT treat it as "pending" and must NOT apply
// migrations over a dirty DB. MigrationStatus returns it wrapped so callers
// can detect it with errors.Is.
var ErrDirtyMigration = errors.New("dirty migration state, manual intervention required")

// MigrationStatus reports whether application (golang-migrate) and/or River
// migrations are pending WITHOUT mutating the database. It is the read-only
// counterpart of RunMigrations: --migrate-check calls it to decide whether a
// deploy needs a snapshot-then-migrate (pending) or can skip straight to the
// image swap (up-to-date).
//
// It NEVER applies a migration. App pending is detected by comparing the
// current applied version (golang-migrate m.Version()) against the highest
// version available in the migrations source; River pending is detected via a
// rivermigrate DryRun, which is guaranteed not to write.
//
// A dirty golang-migrate state is surfaced as an error wrapping
// ErrDirtyMigration (NOT "pending"), so the caller can abort and require manual
// intervention rather than snapshot-and-migrate over a half-applied schema.
func MigrationStatus(ctx context.Context, databaseURL string, migrationsPath string) (appPending bool, riverPending bool, err error) {
	appPending, err = appMigrationsPending(ctx, databaseURL, migrationsPath)
	if err != nil {
		return false, false, err
	}

	riverPending, err = riverMigrationsPending(ctx, databaseURL)
	if err != nil {
		return false, false, err
	}

	return appPending, riverPending, nil
}

// appMigrationsPending reports whether any golang-migrate application migration
// is unapplied. It reads the current applied version and compares it against the
// highest version present in the migrations source; it never calls Up/Steps.
//
// Fresh DB (no schema_migrations table yet) counts as pending iff the source has
// any migration. A dirty state returns an error wrapping ErrDirtyMigration.
//
// Strictly non-mutating: golang-migrate's Postgres driver CREATEs the
// schema_migrations table on migrate.New (its ensureVersionTable runs at Open),
// which would mutate a fresh DB just by reading. So we first detect a fresh DB
// via a read-only catalog check and short-circuit BEFORE opening migrate.New —
// preserving the "report pending WITHOUT mutating" contract on the fresh path.
func appMigrationsPending(ctx context.Context, databaseURL string, migrationsPath string) (bool, error) {
	sourceURL := fmt.Sprintf("file://%s", migrationsPath)

	highest, hasAny, err := highestSourceVersion(sourceURL)
	if err != nil {
		return false, fmt.Errorf("read highest source migration version: %w", err)
	}

	// Fresh-DB short-circuit: if the version-tracking table does not exist yet,
	// the DB has never been migrated. Detect this read-only (no migrate.New, which
	// would create the table) and report pending iff the source has any migration.
	tracked, err := migrationsTableExists(ctx, databaseURL)
	if err != nil {
		return false, fmt.Errorf("check migrations table exists: %w", err)
	}
	if !tracked {
		return hasAny, nil
	}

	m, err := migrate.New(sourceURL, databaseURL)
	if err != nil {
		return false, fmt.Errorf("create migration instance: %w", err)
	}
	defer func() {
		// Closing reports source/db errors; they are non-fatal to the read we
		// already completed, so log-and-ignore via the package logger would add
		// noise here. We intentionally discard them.
		_, _ = m.Close()
	}()

	current, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			// schema_migrations exists but holds no row (e.g. a prior failed
			// bootstrap): pending iff the source defines any migration.
			return hasAny, nil
		}
		return false, fmt.Errorf("read current migration version: %w", err)
	}
	if dirty {
		return false, fmt.Errorf("%w (current version %d)", ErrDirtyMigration, current)
	}

	// hasAny is implied when current is set (the applied version came from the
	// source), but guard anyway: with no source migrations nothing can pend.
	if !hasAny {
		return false, nil
	}
	return current < highest, nil
}

// migrationsTableExists reports whether golang-migrate's schema_migrations
// tracking table is present, via a read-only to_regclass catalog lookup on a
// short-lived connection.
//
// Raw-SQL exception: this is the same migration-infrastructure carve-out
// core.md blesses for the advisory-lock SELECTs already in migration.go
// (runRiverMigrations) — a single static, non-mutating, system-catalog read. It
// has no sqlc representation by construction: schema_migrations is
// golang-migrate's OWN internal bookkeeping table, not part of the application
// schema sqlc generates from, and golang-migrate exposes no API to test the
// table's existence (its version reads go through Open, which CREATEs the table).
// The read exists precisely so the fresh-DB path can short-circuit BEFORE
// migrate.New and stay strictly non-mutating (see appMigrationsPending).
func migrationsTableExists(ctx context.Context, databaseURL string) (bool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return false, fmt.Errorf("parse DATABASE_URL for migrations-table check: %w", err)
	}
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return false, fmt.Errorf("open pool for migrations-table check: %w", err)
	}
	defer pool.Close()

	var regclass *string
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations')::text").Scan(&regclass); err != nil {
		return false, fmt.Errorf("read schema_migrations regclass: %w", err)
	}
	return regclass != nil, nil
}

// highestSourceVersion opens the migrations source read-only and walks it to
// find the highest available version. hasAny is false when the source defines no
// migrations at all. The source driver is read-only (it never touches the DB).
func highestSourceVersion(sourceURL string) (highest uint, hasAny bool, err error) {
	src, err := source.Open(sourceURL)
	if err != nil {
		return 0, false, fmt.Errorf("open migrations source: %w", err)
	}
	defer func() { _ = src.Close() }()

	version, err := src.First()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No migrations in the source.
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read first source version: %w", err)
	}

	highest = version
	for {
		next, err := src.Next(version)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return 0, false, fmt.Errorf("read next source version after %d: %w", version, err)
		}
		version = next
		if version > highest {
			highest = version
		}
	}
	return highest, true, nil
}

// riverMigrationsPending reports whether River has any unapplied migration. It
// runs a DryRun up-migration (guaranteed non-mutating: rivermigrate's
// versionsInsert/versionsDelete early-return when DryRun is set) and inspects the
// versions that WOULD apply — a non-empty set means pending.
//
// Unlike runRiverMigrations, the read does NOT take the migration advisory lock:
// a DryRun never writes, so it needs no serialization against concurrent writers,
// and skipping the lock avoids blocking the check behind an in-progress migrate.
func riverMigrationsPending(ctx context.Context, databaseURL string) (bool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return false, fmt.Errorf("parse DATABASE_URL for river migration check: %w", err)
	}
	poolCfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return false, fmt.Errorf("open pool for river migration check: %w", err)
	}
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return false, fmt.Errorf("create river migrator: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{DryRun: true})
	if err != nil {
		return false, fmt.Errorf("river migration dry-run: %w", err)
	}
	return len(res.Versions) > 0, nil
}
