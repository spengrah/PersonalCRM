//go:build integration_testdb

package judgment_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/testdb"
)

// TestMain clones a per-package template database and rewrites DATABASE_URL to
// the clone before running this package's DB-backed tests.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForTest())))
}

func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}
