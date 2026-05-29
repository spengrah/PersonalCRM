//go:build integration_testdb

package scheduler

import (
	"os"
	"testing"

	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/tests/testsupport"
)

// TestMain clones a per-package template database (testdb.SetupPackage) and
// rewrites DATABASE_URL to the clone before running this package's tests, so
// the os.Getenv("DATABASE_URL") call sites transparently use a private
// database. See backend/tests/testmain_integration_test.go for the full
// rationale.
//
// This bridge file is compiled only under the integration_testdb tag. Under
// the no-tag unit build it is excluded, this package has no TestMain, and the
// DB-backed tests self-skip on unset DATABASE_URL / testing.Short().
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(testsupport.GetMigrationsPath())))
}
