package db

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/logger"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// RunMigrations runs database migrations
func RunMigrations(databaseURL string, migrationsPath string) error {
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
