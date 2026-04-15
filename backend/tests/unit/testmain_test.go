package unit

import (
	"context"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/db"
)

// TestMain applies database migrations once per `go test` invocation before
// running any tests in this package. The sync service tests are DB-backed but
// all hit the same shared schema, so rerunning migrations in each test only
// burns CI wall-clock on River's migration/bootstrap path.
func TestMain(m *testing.M) {
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		if err := db.RunMigrations(context.Background(), databaseURL, migrationsPathForUnit()); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: failed to run migrations: %v\n", err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}
