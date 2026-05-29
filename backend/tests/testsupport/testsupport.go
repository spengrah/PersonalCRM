// Package testsupport holds small, stdlib-only helpers shared across the
// integration test packages (tests, tests/river, tests/scheduler, ...).
//
// It is deliberately UNTAGGED (no //go:build integration_testdb) and imports
// only the Go standard library. The files that consume it — the River and
// scheduler test files — are themselves untagged and must still compile under
// the no-tag unit build before they self-skip on an unset DATABASE_URL.
// A build-tagged helper package would leave those untagged importers with no
// buildable dependency under the no-tag build. Keeping testsupport untagged
// and stdlib-only avoids that.
//
// The functions hold no mutable package state, so the package is import-safe
// from multiple concurrent test packages.
package testsupport

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const longTestsEnvVar = "LONG_TESTS"

// GetMigrationsPath returns the absolute path to the backend migrations
// directory.
//
// If MIGRATIONS_PATH is set to an absolute path it is used verbatim;
// otherwise the path is computed relative to THIS file's location. Because
// runtime.Caller(0) resolves to the file that defines this function
// (backend/tests/testsupport/testsupport.go, two levels below backend/), the
// "../../migrations" join always resolves to backend/migrations regardless of
// which package calls GetMigrationsPath. The TestMain bridges inject the
// result explicitly via testdb.WithMigrationsPath, so the value is computed
// once at the call site and passed in.
func GetMigrationsPath() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}

	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename)
	return filepath.Join(testDir, "..", "..", "migrations")
}

// RequireLongTests skips the calling test unless the LONG_TESTS environment
// variable is set (and short mode is off). It gates the intentionally-slow
// River timing tests so developers can opt in locally and the nightly slow
// workflow can run them, while the default fast suite skips them.
func RequireLongTests(t testing.TB) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping long integration test in short mode")
	}
	if os.Getenv(longTestsEnvVar) == "" {
		t.Skip("skipping long integration test; set LONG_TESTS=1 or run make test-integration-slow")
	}
}
