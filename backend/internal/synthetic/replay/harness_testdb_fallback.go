//go:build !integration_testdb

package replay

import (
	"testing"

	"personal-crm/backend/internal/db"
)

// openTestClone is the no-tag fallback. Integration targets compile the tagged
// version, which opens a clone through internal/testdb.
func openTestClone(t *testing.T) *db.Database {
	t.Helper()
	t.Skip("integration_testdb build tag required for synthetic harness tests")
	return nil
}
