package db

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/logger"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// RunMigrations applies both our golang-migrate migrations and River's own
// queue schema migrations. River migrations are applied after ours via its
// programmatic migrator (see https://riverqueue.com). Both sides are idempotent.
func RunMigrations(ctx context.Context, databaseURL string, migrationsPath string) error {
	// 1. Application migrations via golang-migrate.
	m, err := migrate.New(
		fmt.Sprintf("file://%s", migrationsPath),
		databaseURL,
	)
	if err != nil {
		return fmt.Errorf("failed to create migration instance: %w", err)
	}
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			logger.Error().
				Err(srcErr).
				Err(dbErr).
				Msg("error closing migration instance")
		}
	}()

	err = m.Up()
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	if errors.Is(err, migrate.ErrNoChange) {
		logger.Info().Msg("no new migrations to run")
	} else {
		logger.Info().Msg("migrations completed successfully")
	}

	// 2. River's own migrations via its programmatic migrator.
	//    Uses a short-lived pool so the bootstrap step doesn't share or leak
	//    the long-lived application pool (which is opened later by NewDatabase).
	if err := runRiverMigrations(ctx, databaseURL); err != nil {
		return err
	}

	return nil
}

// riverMigrateAdvisoryLockID is a fixed key used with pg_advisory_lock to
// serialize concurrent callers of runRiverMigrations against the same
// database. rivermigrate does not provide its own advisory locking (unlike
// golang-migrate), so two processes running integration tests in parallel —
// or two `go test` packages that each bootstrap the schema — will race on
// `CREATE TABLE` / `CREATE TYPE`, producing duplicate-key violations on
// `pg_type_typname_nsp_index`. The lock ID is an arbitrary stable int64
// chosen to not collide with golang-migrate's lock (which hashes the DB name).
const riverMigrateAdvisoryLockID int64 = 9230423_0001

// runRiverMigrations opens a short-lived pgxpool and applies any pending River
// migrations. Idempotent: if River's schema is current, it is a no-op.
// Uses a pg session-level advisory lock to serialize concurrent callers.
func runRiverMigrations(ctx context.Context, databaseURL string) error {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL for river migrations: %w", err)
	}
	// MaxConns = 2 so the migrator (which acquires its own conn from the pool)
	// doesn't deadlock against the advisory-lock connection we hold below.
	poolCfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("open pool for river migrations: %w", err)
	}
	defer pool.Close()

	// Acquire and hold the advisory lock on a single connection for the entire
	// migrate operation. pg_advisory_lock / pg_advisory_unlock are session-scoped,
	// so we must use the same connection for both.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for river migrations lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", riverMigrateAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire river migrations advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", riverMigrateAdvisoryLockID); unlockErr != nil {
			logger.Error().Err(unlockErr).Msg("failed to release river migrations advisory lock")
		}
	}()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("run river migrations: %w", err)
	}
	if len(res.Versions) == 0 {
		logger.Info().Msg("no new river migrations to run")
	} else {
		logger.Info().
			Int("applied", len(res.Versions)).
			Msg("river migrations completed successfully")
	}
	return nil
}

// SeedDatabase seeds the database with demo data
func (db *Database) SeedDatabase(ctx context.Context, seedFile string) error {
	// Read the seed file
	seedSQL := `
-- Seed data execution marker
SELECT 1;
	`

	// For now, we'll implement basic seeding
	// In a full implementation, you would read the seed.sql file and execute it
	logger.Debug().Str("seed_file", seedFile).Msg("database seeding would be implemented here")

	// Execute the seed SQL
	_, err := db.Pool.Exec(ctx, seedSQL)
	if err != nil {
		return fmt.Errorf("failed to seed database: %w", err)
	}

	return nil
}
