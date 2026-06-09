//go:build integration_testdb

// Command cleanclones sweeps leaked personal_crm_test_clone_* databases AND
// stale personal_crm_test_template_<hash> databases in a single pass. It is
// invoked ONLY by `make test-clean-clones` via `go run`; because it is a
// `package main` (not a _test.go file), it can never execute during
// `go test ./internal/testdb/...`. All drops route through
// testdb.CleanStaleDatabases, which name-guards every database before dropping
// it and serializes the template drop pass under the build advisory lock.
//
// It resolves the migrations path (so the current run's template is kept warm,
// not reaped) from MIGRATIONS_PATH if that is set to an absolute path, else from
// a runtime.Caller-relative path. If neither resolves to an existing directory
// it passes "" — the current-template exclusion is then skipped, which is still
// safe (the lock + open-backend skip protect any in-use template).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"personal-crm/backend/internal/testdb"
)

func main() {
	if err := testdb.CleanStaleDatabases(resolveMigrationsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "cleanclones: %v\n", err)
		os.Exit(1)
	}
}

// resolveMigrationsPath returns the backend migrations directory, or "" if it
// cannot be resolved to an existing directory. MIGRATIONS_PATH (absolute) wins;
// otherwise the path is computed four "../" hops up from this command file
// (.../internal/testdb/cmd/cleanclones → backend) plus "/migrations".
func resolveMigrationsPath() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		if dirHasUpMigrations(path) {
			return path
		}
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// .../backend/internal/testdb/cmd/cleanclones/main.go → backend/migrations.
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..", "migrations")
	if dirHasUpMigrations(path) {
		return path
	}
	return ""
}

// dirHasUpMigrations reports whether path is a directory containing at least one
// *.up.sql migration file, so a wrong hop count or a stale MIGRATIONS_PATH
// resolves to "" rather than silently mis-naming the current template.
func dirHasUpMigrations(path string) bool {
	matches, err := filepath.Glob(filepath.Join(path, "*.up.sql"))
	return err == nil && len(matches) > 0
}
