//go:build integration_testdb

package unit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/testdb"
)

// TestMain clones a per-package template database (testdb.SetupPackage) and
// rewrites DATABASE_URL to the clone before running this package's tests. See
// backend/tests/testmain_integration_test.go for the full rationale.
//
// This bridge file is compiled only under the integration_testdb tag. Under
// the no-tag unit build it is excluded, this package has no TestMain, and the
// DB-backed tests self-skip on unset DATABASE_URL / testing.Short().
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForUnit())))
}

// migrationsPathForUnit returns the absolute path to backend/migrations. Lives
// in this tagged bridge because the per-package clone harness is the only
// caller; under the no-tag unit build it is not compiled.
func migrationsPathForUnit() string {
	if p := os.Getenv("MIGRATIONS_PATH"); p != "" && filepath.IsAbs(p) {
		return p
	}
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename) // backend/tests/unit
	return filepath.Join(testDir, "..", "..", "migrations")
}
