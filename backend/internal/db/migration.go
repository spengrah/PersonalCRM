package db

import (
	"context"
	"fmt"
	"log"

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
	defer m.Close()

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	if err == migrate.ErrNoChange {
		log.Println("No new migrations to run")
	} else {
		log.Println("Migrations completed successfully")
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
	log.Println("Database seeding would be implemented here")
	log.Printf("Seed file: %s", seedFile)

	// Execute the seed SQL
	_, err := db.Pool.Exec(ctx, seedSQL)
	if err != nil {
		return fmt.Errorf("failed to seed database: %w", err)
	}

	return nil
}
