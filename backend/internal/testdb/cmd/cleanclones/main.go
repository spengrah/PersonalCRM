//go:build integration_testdb

// Command cleanclones drops every leaked personal_crm_test_clone_* database.
// It is invoked ONLY by `make test-clean-clones` via `go run`; because it is a
// `package main` (not a _test.go file), it can never execute during
// `go test ./internal/testdb/...`. All drops route through testdb.CleanClones,
// which name-guards every database before dropping it.
package main

import (
	"fmt"
	"os"

	"personal-crm/backend/internal/testdb"
)

func main() {
	if err := testdb.CleanClones(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanclones: %v\n", err)
		os.Exit(1)
	}
}
