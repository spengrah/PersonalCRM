//go:build integration_testdb

package repository_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/testdb"
)

// TestMain clones a per-package template database (testdb.SetupPackage) and
// rewrites DATABASE_URL to the clone before running this package's DB-backed
// tests. Compiled only under the integration_testdb tag; the package's unit
// tests need no database.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForTest())))
}

// migrationsPathForTest returns the absolute path to backend/migrations from
// the location of this test file.
func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename) // backend/internal/repository
	return filepath.Join(testDir, "..", "..", "migrations")
}
