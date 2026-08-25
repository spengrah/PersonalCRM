//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration080ContentInteractionIDIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	err = m.Migrate(80)
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err)
	}
	require.NoError(t, m.Steps(-1))
	support := repository.NewSyntheticSupportRepository(database.Queries)
	for _, table := range []string{"comms_message", "telegram_message", "messages_message", "phone_call"} {
		defs, err := support.ListIndexDefsForTable(ctx, table)
		require.NoError(t, err)
		assert.NotContains(t, defs, "idx_"+table+"_interaction_id")
	}
	require.NoError(t, m.Steps(1))
	cases := []struct{ table, name string }{{"comms_message", "idx_comms_message_interaction_id"}, {"telegram_message", "idx_telegram_message_interaction_id"}, {"messages_message", "idx_messages_message_interaction_id"}, {"phone_call", "idx_phone_call_interaction_id"}}
	for _, tc := range cases {
		defs, err := support.ListIndexDefsForTable(ctx, tc.table)
		require.NoError(t, err)
		def, ok := defs[tc.name]
		require.True(t, ok)
		assert.Equal(t, []string{"interaction_id"}, usingKeyColumns(t, def))
		assert.ElementsMatch(t, []string{"interaction_id is not null"}, predicateConjuncts(t, def))
	}
}
