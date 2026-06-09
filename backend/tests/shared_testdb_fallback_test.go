//go:build !integration_testdb

package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
)

func newSharedTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()
	_ = ctx
	t.Skip("integration_testdb build tag required for shared integration database tests")
	return nil, nil
}
