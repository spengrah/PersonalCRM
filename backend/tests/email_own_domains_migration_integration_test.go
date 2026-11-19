//go:build integration_testdb

package tests

import (
	"fmt"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// ownDomainsKeyMigration is the migration that renames the email own-domain
// metadata key discovery_own_domains to own_domains.
const ownDomainsKeyMigration = 107

// TestMigration107_EmailOwnDomainsKey proves the up migration moves each email
// sync state's discovery_own_domains value to own_domains, leaving no
// discovery_own_domains key and a state without the key unchanged, and that
// the down migration restores the old key. It rolls the schema of its own
// clone, so it stays serial.
func TestMigration107_EmailOwnDomainsKey(t *testing.T) {
	ctx := t.Context()
	cloneURL, migrationsPath := newMigrationStatusClone(t)
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Migrate(ownDomainsKeyMigration-1), "roll the schema to before the migration")

	pool, err := pgxpool.New(ctx, cloneURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	syncRepo := repository.NewSyncRepository(db.New(pool))

	accountA := "acct-a@synthetic.example"
	accountB := "acct-b@synthetic.example"
	configured, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    google.GmailSourceName,
		AccountID: &accountA,
		Enabled:   true,
		Metadata: map[string]any{
			"backfill_since":        "2026-01-01",
			"discovery_own_domains": []any{"own-domain.example", "second.example"},
		},
	})
	require.NoError(t, err)
	unconfigured, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    google.GmailSourceName,
		AccountID: &accountB,
		Enabled:   true,
		Metadata:  map[string]any{"backfill_since": "2026-01-01"},
	})
	require.NoError(t, err)

	metadataOf := func(id uuid.UUID) map[string]any {
		t.Helper()
		state, err := syncRepo.GetSyncState(ctx, id)
		require.NoError(t, err)
		return state.Metadata
	}

	require.NoError(t, m.Migrate(ownDomainsKeyMigration), "apply the up migration")
	require.Equal(t, map[string]any{
		"backfill_since": "2026-01-01",
		"own_domains":    []any{"own-domain.example", "second.example"},
	}, metadataOf(configured.ID), "the value moves to own_domains and no discovery_own_domains key is left")
	require.Equal(t, map[string]any{"backfill_since": "2026-01-01"}, metadataOf(unconfigured.ID),
		"a state without the key is unchanged")

	require.NoError(t, m.Migrate(ownDomainsKeyMigration-1), "apply the down migration")
	require.Equal(t, map[string]any{
		"backfill_since":        "2026-01-01",
		"discovery_own_domains": []any{"own-domain.example", "second.example"},
	}, metadataOf(configured.ID), "the down migration restores the old key")
	require.Equal(t, map[string]any{"backfill_since": "2026-01-01"}, metadataOf(unconfigured.ID))
}
