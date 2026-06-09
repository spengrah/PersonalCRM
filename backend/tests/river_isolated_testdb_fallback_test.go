//go:build !integration_testdb

package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
)

// newIsolatedRiverTestDB is the no-tag fallback used by short unit builds.
// Integration targets compile the tagged helper that creates an ephemeral clone.
func newIsolatedRiverTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()

	_ = ctx
	t.Skip("integration_testdb build tag required for isolated River database tests")
	return nil, nil
}
