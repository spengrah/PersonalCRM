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
	// Parse connection string to get base config
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	// Apply Pi-optimized pool configuration
	// These settings are optimized for Raspberry Pi's limited resources:
	// - MaxConns: Limit concurrent connections to avoid memory pressure
	// - MinConns: Keep connections warm for faster response times
	// - MaxConnIdleTime: Recycle idle connections faster than default
	// - MaxConnLifetime: Limit connection lifetime to prevent stale connections
	// - HealthCheckPeriod: Frequent health checks for reliability
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
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
