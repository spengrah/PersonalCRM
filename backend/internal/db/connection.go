package db

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database represents the database connection pool
type Database struct {
	Pool    *pgxpool.Pool
	Queries *Queries
}

// NewDatabase creates a new database connection using the provided configuration
func NewDatabase(ctx context.Context, cfg config.DatabaseConfig) (*Database, error) {
	// No validation needed - already validated in config.Load()
	pool, err := pgxpool.New(ctx, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Test the connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	queries := New(pool)

	return &Database{
		Pool:    pool,
		Queries: queries,
	}, nil
}

// Close closes the database connection pool
func (db *Database) Close() {
	if db.Pool != nil {
		db.Pool.Close()
	}
}

// HealthCheck performs a health check on the database
func (db *Database) HealthCheck(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}
