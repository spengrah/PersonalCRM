//go:build integration_testdb

package tests

import (
	"context"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/require"
)

// wipedTables is the EXACT set ResetSyntheticData truncates (kept in sync with
// the TRUNCATE list in queries/test.sql). The reset test asserts (1) every one
// of these is empty after the reset, and (2) the catalog guard: every public
// base table is in this set, is schema_migrations, or matches the river_%
// allowlist — so a future app table omitted from the TRUNCATE FAILS the test.
var wipedTables = []string{
	"calendar_event",
	"comms_message",
	"connection",
	"contact",
	"contact_enrichment",
	"contact_method",
	"contact_summary",
	"contact_tag",
	"contact_task",
	"event",
	"event_consumer_claim",
	"external_contact",
	"external_identity",
	"external_sync_log",
	"external_sync_state",
	"interaction",
	"mac_host",
	"mac_host_pairing_token",
	"meeting_note",
	"messages_message",
	"note",
	"note_embedding",
	"oauth_credential",
	"phone_call",
	"prompt_query",
	"river_job",
	"tag",
	"telegram_channel_state",
	"telegram_chat_config",
	"telegram_message",
	"telegram_session",
	"telegram_update_state",
}

// TestSyntheticResetSyntheticData_WipesEveryDataTable is the DESTRUCTIVE,
// DB-wide reset test. It runs against a per-test CLONE DB (never the shared
// personal_crm_test) so a TRUNCATE-of-everything cannot wipe sibling tests. It
// asserts: every wiped table is empty after the reset, schema_migrations
// survives, and the catalog guard (every public table is accounted for).
func TestSyntheticResetSyntheticData_WipesEveryDataTable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping destructive reset integration test in short mode")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	support := repository.NewSyntheticSupportRepository(database.Queries)

	// Seed a synthetic world (covers the relational core: contact, contact_method,
	// interaction, event, comms_message, external_*, mac_host, meeting_note, ...)
	// plus standalone markers for the harness-untouched wiped tables. The clone is
	// dropped after the test, so the harness's own cleanup is irrelevant — the
	// reset wipes everything regardless.
	h := synthetic.NewHarnessForNamespace(t, ctx, database, "reset", factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
	require.NoError(t, err)
	_, err = h.SeedOrphanMeetingNote(ctx, "synthetic reset note", "synthetic reset summary")
	require.NoError(t, err)
	require.NoError(t, support.InsertResetMarkers(ctx))
	require.NoError(t, support.InsertNonFinalRiverJob(ctx))

	// Sanity: a representative wiped table has rows BEFORE the reset, so the
	// "empty after" assertion is meaningful.
	beforeContacts, err := support.CountAllRows(ctx, "contact")
	require.NoError(t, err)
	require.Greater(t, beforeContacts, int64(0), "expected seeded contacts before reset")
	beforeTags, err := support.CountAllRows(ctx, "tag")
	require.NoError(t, err)
	require.Greater(t, beforeTags, int64(0), "expected the tag marker before reset")

	// The reset.
	require.NoError(t, support.ResetSyntheticData(ctx))

	// Every wiped table is empty (a table OMITTED from the TRUNCATE would still
	// hold its marker and fail here; a phantom/dropped table in the TRUNCATE
	// would have aborted ResetSyntheticData above).
	for _, tbl := range wipedTables {
		count, err := support.CountAllRows(ctx, tbl)
		require.NoErrorf(t, err, "count %s after reset", tbl)
		require.Equalf(t, int64(0), count, "table %s must be empty after ResetSyntheticData", tbl)
	}

	// schema_migrations must SURVIVE (truncating it orphans the schema).
	migCount, err := support.CountAllRows(ctx, "schema_migrations")
	require.NoError(t, err)
	require.Greater(t, migCount, int64(0), "schema_migrations must survive the reset")

	// Catalog guard: every public base table is in wipedTables, is
	// schema_migrations, or matches the river_% allowlist. A future app data
	// table not added to the TRUNCATE FAILS here.
	tables, err := support.ListPublicTables(ctx)
	require.NoError(t, err)
	wiped := make(map[string]bool, len(wipedTables))
	for _, tbl := range wipedTables {
		wiped[tbl] = true
	}
	for _, tbl := range tables {
		switch {
		case wiped[tbl]:
		case tbl == "schema_migrations":
		case strings.HasPrefix(tbl, "river_"):
		default:
			t.Errorf("public table %q is neither wiped, schema_migrations, nor a river_%% internal table — add it to the ResetSyntheticData TRUNCATE list (and wipedTables)", tbl)
		}
	}
}
