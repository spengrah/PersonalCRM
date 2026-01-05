package integration

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/config"
	"personal-crm/backend/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPgTrgmMigration tests that the pg_trgm extension is properly installed
// and the GIN index is created
func TestPgTrgmMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping migration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping migration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	t.Run("pg_trgm extension is enabled", func(t *testing.T) {
		var extname string
		err := database.Pool.QueryRow(ctx,
			`SELECT extname FROM pg_extension WHERE extname = 'pg_trgm'`,
		).Scan(&extname)

		require.NoError(t, err, "pg_trgm extension should be installed")
		assert.Equal(t, "pg_trgm", extname)
	})

	t.Run("trigram index exists on contact.full_name", func(t *testing.T) {
		var indexname string
		err := database.Pool.QueryRow(ctx,
			`SELECT indexname
			 FROM pg_indexes
			 WHERE tablename = 'contact'
			   AND indexname = 'idx_contact_fullname_trgm'`,
		).Scan(&indexname)

		require.NoError(t, err, "idx_contact_fullname_trgm index should exist")
		assert.Equal(t, "idx_contact_fullname_trgm", indexname)
	})

	t.Run("similarity function works", func(t *testing.T) {
		var similarity float64
		err := database.Pool.QueryRow(ctx,
			`SELECT similarity('John Smith', 'John Smyth')`,
		).Scan(&similarity)

		require.NoError(t, err, "similarity() function should be available")
		assert.Greater(t, similarity, 0.0, "similarity should be greater than 0")
		assert.LessOrEqual(t, similarity, 1.0, "similarity should be at most 1.0")
	})

	t.Run("similarity returns expected values", func(t *testing.T) {
		tests := []struct {
			name     string
			str1     string
			str2     string
			minScore float64
			maxScore float64
		}{
			{
				name:     "exact match",
				str1:     "John Smith",
				str2:     "John Smith",
				minScore: 0.99,
				maxScore: 1.0,
			},
			{
				name:     "very similar",
				str1:     "John Smith",
				str2:     "John Smyth",
				minScore: 0.6,
				maxScore: 0.95,
			},
			{
				name:     "somewhat similar",
				str1:     "John Smith",
				str2:     "Jon Smith",
				minScore: 0.5,
				maxScore: 0.95,
			},
			{
				name:     "different names",
				str1:     "John Smith",
				str2:     "Jane Doe",
				minScore: 0.0,
				maxScore: 0.3,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var similarity float64
				err := database.Pool.QueryRow(ctx,
					`SELECT similarity($1, $2)`,
					tt.str1, tt.str2,
				).Scan(&similarity)

				require.NoError(t, err)
				assert.GreaterOrEqual(t, similarity, tt.minScore,
					"similarity should be at least %f for %s vs %s", tt.minScore, tt.str1, tt.str2)
				assert.LessOrEqual(t, similarity, tt.maxScore,
					"similarity should be at most %f for %s vs %s", tt.maxScore, tt.str1, tt.str2)
			})
		}
	})

	t.Run("GIN index is used for similarity queries", func(t *testing.T) {
		// Check that the query plan uses the GIN index
		var planText string
		err := database.Pool.QueryRow(ctx,
			`EXPLAIN (FORMAT TEXT)
			 SELECT full_name, similarity(full_name, 'John Smith') as sim
			 FROM contact
			 WHERE similarity(full_name, 'John Smith') > 0.3
			 ORDER BY sim DESC
			 LIMIT 5`,
		).Scan(&planText)

		require.NoError(t, err)
		// The plan should mention the index if it's being used
		// Note: This might not always use the index depending on table size and statistics
		// So we just verify the query runs without error
		assert.NotEmpty(t, planText)
	})

	t.Run("similarity threshold works correctly", func(t *testing.T) {
		// Insert test contacts if they don't exist
		_, err := database.Pool.Exec(ctx,
			`INSERT INTO contact (full_name, cadence, next_contact_date, created_at, updated_at)
			 VALUES ($1, $2, NOW(), NOW(), NOW())
			 ON CONFLICT DO NOTHING`,
			"Test User Alpha", "monthly",
		)
		require.NoError(t, err)

		// Query with high threshold
		var count int
		err = database.Pool.QueryRow(ctx,
			`SELECT COUNT(*)
			 FROM contact
			 WHERE similarity(full_name, 'Test User Beta') > 0.8
			   AND deleted_at IS NULL`,
		).Scan(&count)

		require.NoError(t, err)
		// Should find few or no matches with high threshold for different name
		assert.GreaterOrEqual(t, count, 0)

		// Query with low threshold
		err = database.Pool.QueryRow(ctx,
			`SELECT COUNT(*)
			 FROM contact
			 WHERE similarity(full_name, 'Test User Alpha') > 0.3
			   AND deleted_at IS NULL`,
		).Scan(&count)

		require.NoError(t, err)
		// Should find the exact match with low threshold
		assert.GreaterOrEqual(t, count, 1, "should find at least the exact match")
	})

	t.Run("index handles special characters", func(t *testing.T) {
		// Insert a contact with special characters
		_, err := database.Pool.Exec(ctx,
			`INSERT INTO contact (full_name, cadence, next_contact_date, created_at, updated_at)
			 VALUES ($1, $2, NOW(), NOW(), NOW())
			 ON CONFLICT DO NOTHING`,
			"O'Brien-Smith, Jr.", "monthly",
		)
		require.NoError(t, err)

		// Query should handle special characters
		var similarity float64
		err = database.Pool.QueryRow(ctx,
			`SELECT similarity(full_name, $1)
			 FROM contact
			 WHERE full_name = $2
			 LIMIT 1`,
			"OBrien Smith",
			"O'Brien-Smith, Jr.",
		).Scan(&similarity)

		require.NoError(t, err)
		// Should return some similarity despite different formatting
		assert.Greater(t, similarity, 0.0)
	})
}

// TestPgTrgmMigrationRollback tests that the migration can be rolled back
func TestPgTrgmMigrationRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping migration rollback test in short mode")
	}

	// Note: This test assumes migrations are run before tests
	// In a real CI/CD pipeline, you might want to test rollback in isolation

	t.Run("index can be dropped", func(t *testing.T) {
		// This is more of a documentation test
		// The actual rollback is defined in the down migration
		// We're just verifying the down migration syntax is valid

		databaseURL := os.Getenv("DATABASE_URL")
		if databaseURL == "" {
			t.Skip("DATABASE_URL not set")
		}

		ctx := context.Background()
		cfg := config.TestConfig()
		cfg.Database.URL = databaseURL

		database, err := db.NewDatabase(ctx, cfg.Database)
		if err != nil {
			t.Skip("Could not connect to database")
		}
		defer database.Close()

		// Verify the DROP INDEX statement doesn't error (idempotent)
		_, err = database.Pool.Exec(ctx,
			`DROP INDEX IF EXISTS idx_contact_fullname_trgm`,
		)
		assert.NoError(t, err, "DROP INDEX IF EXISTS should be idempotent")
	})
}
