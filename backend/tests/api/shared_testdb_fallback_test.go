//go:build !integration_testdb

package api

import (
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
)

func newAPISharedTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()
	_ = ctx
	t.Skip("integration_testdb build tag required for shared API database tests")
	return nil, nil
}
