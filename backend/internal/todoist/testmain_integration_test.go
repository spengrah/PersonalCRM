//go:build integration_testdb

package todoist

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/testdb"
)

// TestMain clones a per-package template database (testdb.SetupPackage) and
// rewrites DATABASE_URL to the clone before running this package's tests. This
// package previously had no TestMain; its DB-backed tests each called
// db.RunMigrations inline. The clone now inherits the fully-migrated schema
// from the template, so those inline migration runs are removed.
//
// This bridge file is compiled only under the integration_testdb tag. Under
// the no-tag unit build it is excluded, this package has no TestMain, and the
// DB-backed tests self-skip on unset DATABASE_URL / testing.Short().
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForTest())))
}

// migrationsPathForTest returns the absolute path to backend/migrations from
// the location of this test file. Lives in this tagged bridge because the
// per-package clone harness is the only caller; under the no-tag unit build it
// is not compiled.
func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename) // backend/internal/todoist
	return filepath.Join(testDir, "..", "..", "migrations")
}
