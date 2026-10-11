//go:build integration_testdb

package tests

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// nullableNowTimestampBaseline lists the "table.column" timestamp columns that
// are nullable yet default to now() today. The default says "always set", so
// NOT NULL must back it. The rule is forward-only: a column not listed here
// fails, and a listed column that is now NOT NULL fails, so the baseline only
// shrinks. Fix a column with a migration that backfills NULLs and sets NOT
// NULL, then delete its entry.
var nullableNowTimestampBaseline = map[string]struct{}{
	"calendar_event.created_at":         {},
	"calendar_event.synced_at":          {},
	"calendar_event.updated_at":         {},
	"contact.created_at":                {},
	"contact.updated_at":                {},
	"contact_enrichment.enriched_at":    {},
	"contact_method.created_at":         {},
	"contact_method.updated_at":         {},
	"contact_tag.created_at":            {},
	"contact_task.created_at":           {},
	"contact_task.updated_at":           {},
	"external_contact.created_at":       {},
	"external_contact.updated_at":       {},
	"external_identity.created_at":      {},
	"external_identity.updated_at":      {},
	"external_sync_log.created_at":      {},
	"external_sync_state.created_at":    {},
	"external_sync_state.updated_at":    {},
	"interaction.created_at":            {},
	"mac_host.created_at":               {},
	"mac_host.updated_at":               {},
	"mac_host_pairing_token.created_at": {},
	"messages_message.created_at":       {},
	"note.created_at":                   {},
	"note.updated_at":                   {},
	"oauth_credential.created_at":       {},
	"oauth_credential.updated_at":       {},
	"phone_call.created_at":             {},
	"tag.created_at":                    {},
	"telegram_channel_state.updated_at": {},
	"telegram_chat_config.created_at":   {},
	"telegram_chat_config.updated_at":   {},
	"telegram_message.created_at":       {},
	"telegram_session.created_at":       {},
	"telegram_session.updated_at":       {},
	"telegram_update_state.updated_at":  {},
	"whatsapp_chat_config.created_at":   {},
	"whatsapp_chat_config.updated_at":   {},
}

func TestSchema_NowDefaultTimestampsAreNotNull(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	cols, err := database.Queries.TestListNullableTimestampDefaultNow(ctx)
	require.NoError(t, err)

	found := map[string]struct{}{}
	var added []string
	for _, col := range cols {
		found[col] = struct{}{}
		if _, ok := nullableNowTimestampBaseline[col]; !ok {
			added = append(added, col)
		}
	}
	var stale []string
	for col := range nullableNowTimestampBaseline {
		if _, ok := found[col]; !ok {
			stale = append(stale, col)
		}
	}
	sort.Strings(added)
	sort.Strings(stale)
	require.Empty(t, added, "new nullable timestamp columns with DEFAULT now(): make them NOT NULL in the migration that adds them")
	require.Empty(t, stale, "nullableNowTimestampBaseline entries that are now NOT NULL or gone: delete them")
}
