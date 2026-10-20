//go:build integration_testdb

package replay

import (
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/tests/testsupport"
)

// openTestClone opens a fresh clone of the migrated template for t, closed and
// dropped on t.Cleanup. The package's TestMain must call testdb.SetupPackage.
func openTestClone(t *testing.T) *db.Database {
	t.Helper()
	testsupport.RequireDatabase(t)
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	return testsupport.OpenDatabaseAt(t, cloneURL)
}
