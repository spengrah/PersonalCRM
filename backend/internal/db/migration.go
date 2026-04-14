package db

import (
	"context"
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
	if err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	if err == migrate.ErrNoChange {
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

// runRiverMigrations opens a short-lived pgxpool and applies any pending River
// migrations. Idempotent: if River's schema is current, it is a no-op.
func runRiverMigrations(ctx context.Context, databaseURL string) error {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL for river migrations: %w", err)
	}
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("open pool for river migrations: %w", err)
	}
	defer pool.Close()

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
