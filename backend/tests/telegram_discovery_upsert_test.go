package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupDiscoveryUpsertTest spins up a real DB-backed ExternalContactRepository
// and returns a per-test-unique source_id prefix plus a cleanup function that
// hard-deletes only this test's rows (scoped to that prefix). The per-test
// prefix is what makes these funcs safe under t.Parallel() — two parallel
// copies would otherwise share fixed "tg-discovery-upsert-<name>" source_ids
// and the prefix-wide cleanup would delete each other's rows mid-test.
func setupDiscoveryUpsertTest(t *testing.T) (
	*repository.ExternalContactRepository,
	*db.Database,
	string,
	func(),
) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)

	repo := repository.NewExternalContactRepository(database.Queries)

	prefix := "tg-discovery-upsert-" + syntheticNS(t) + "-"
	cleanup := func() {
		// Narrow cleanup: only this test's per-run prefix. Use the sqlc query
		// (also used by the /test/cleanup handler) rather than raw SQL.
		_, _ = database.Queries.DeleteExternalContactsBySourceIDPrefix(
			context.Background(),
			pgtype.Text{String: prefix, Valid: true},
		)
		database.Close()
	}
	return repo, database, prefix, cleanup
}

func TestUpsertDiscoveryCandidate_InsertsNewRow(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:      "telegram",
		SourceID:    prefix + "insert",
		DisplayName: strPtr("Dale Dobeck"),
		FirstName:   strPtr("Dale"),
		LastName:    strPtr("Dobeck"),
		Metadata:    map[string]any{"username": "@daledobeck", "message_count": 5},
		SyncedAt:    &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "telegram", got.Source)
	assert.Equal(t, prefix+"insert", got.SourceID)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Dale Dobeck", *got.DisplayName)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName)
	require.NotNil(t, got.LastName)
	assert.Equal(t, "Dobeck", *got.LastName)
	assert.Equal(t, "@daledobeck", got.Metadata["username"])
	// JSON numbers deserialize as float64
	assert.EqualValues(t, 5, got.Metadata["message_count"])
	assert.Equal(t, repository.MatchStatusUnmatched, got.MatchStatus)
}

// TestUpsertDiscoveryCandidate_TelegramSemanticsUnchanged is the regression
// guard for source-parameterizing what used to be a Telegram-only query: the
// four semantics a divergence would silently break, asserted on one row.
func TestUpsertDiscoveryCandidate_TelegramSemanticsUnchanged(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()
	sourceID := prefix + "semantics"

	seeded, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source: "telegram", SourceID: sourceID,
		DisplayName: strPtr("Dale Dobeck"), FirstName: strPtr("Dale"), LastName: strPtr("Dobeck"),
		Metadata: map[string]any{"username": "@daledobeck", "message_count": 5},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	require.Equal(t, repository.MatchStatusUnmatched, seeded.MatchStatus)

	// A nil name preserves; a non-nil name overwrites; metadata merges with the
	// incoming value winning per key; match_status is untouched.
	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source: "telegram", SourceID: sourceID,
		FirstName: nil, LastName: strPtr("Renamed"),
		Metadata: map[string]any{"message_count": 9},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName, "a nil name never clears a captured one")
	require.NotNil(t, got.LastName)
	assert.Equal(t, "Renamed", *got.LastName, "a non-nil name does overwrite")
	assert.Equal(t, "@daledobeck", got.Metadata["username"], "keys the new map omits are retained")
	assert.EqualValues(t, 9, got.Metadata["message_count"], "and the incoming value wins per key")
	assert.Equal(t, repository.MatchStatusUnmatched, got.MatchStatus, "match_status is never touched")
}

// TestUpsertDiscoveryCandidate_WhatsAppRowUsesSameSemantics proves the source
// really is a parameter and not a decoration: the identical sequence on a
// whatsapp row behaves identically.
func TestUpsertDiscoveryCandidate_WhatsAppRowUsesSameSemantics(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()
	sourceID := prefix + "wa@lid"

	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source: "whatsapp", SourceID: sourceID,
		DisplayName: strPtr("WhatsApp 8880001"),
		Metadata:    map[string]any{"peer_jid": sourceID, "message_count": 3},
		SyncedAt:    &now,
	})
	require.NoError(t, err)

	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source: "whatsapp", SourceID: sourceID,
		DisplayName: strPtr("Their Name"),
		Metadata:    map[string]any{"message_count": 7, "phone_e164": "+15559876543"},
		SyncedAt:    &now,
	})
	require.NoError(t, err)
	assert.Equal(t, "whatsapp", got.Source)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Their Name", *got.DisplayName, "a later push name upgrades the JID label")
	assert.Equal(t, sourceID, got.Metadata["peer_jid"], "earlier keys survive the merge")
	assert.EqualValues(t, 7, got.Metadata["message_count"])
	assert.Equal(t, "+15559876543", got.Metadata["phone_e164"])
	assert.Equal(t, repository.MatchStatusUnmatched, got.MatchStatus)
}

func TestUpsertDiscoveryCandidate_PreservesFirstNameWhenNil(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed with names populated.
	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:    "telegram",
		SourceID:  prefix + "preserve",
		FirstName: strPtr("Dale"),
		LastName:  strPtr("Dobeck"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	// Re-upsert with nil names — should preserve existing.
	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "preserve",
		SyncedAt: &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName)
	require.NotNil(t, got.LastName)
	assert.Equal(t, "Dobeck", *got.LastName)
}

func TestUpsertDiscoveryCandidate_OverwritesFirstNameWhenNewValueProvided(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:    "telegram",
		SourceID:  prefix + "overwrite",
		FirstName: strPtr("Dale"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:    "telegram",
		SourceID:  prefix + "overwrite",
		FirstName: strPtr("Daniel"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Daniel", *got.FirstName)
}

func TestUpsertDiscoveryCandidate_MergesMetadata_PreservesExistingKey(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed with a username key.
	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "merge-keep",
		Metadata: map[string]any{"username": "@dale", "message_count": 5},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	// Re-upsert with only message_count — username must be retained.
	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "merge-keep",
		Metadata: map[string]any{"message_count": 10},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	assert.Equal(t, "@dale", got.Metadata["username"], "username key should be preserved by JSONB merge")
	assert.EqualValues(t, 10, got.Metadata["message_count"], "incoming message_count should win for the duplicate key")
}

func TestUpsertDiscoveryCandidate_MergesMetadata_IncomingWinsForDuplicate(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "merge-new",
		Metadata: map[string]any{"username": "@old"},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "merge-new",
		Metadata: map[string]any{"username": "@new"},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	assert.Equal(t, "@new", got.Metadata["username"])
}

func TestUpsertDiscoveryCandidate_SyncedAtAlwaysUpdates(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()

	start := accelerated.GetCurrentTime()
	_, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "syncedat",
		SyncedAt: &start,
	})
	require.NoError(t, err)

	later := start.Add(1 * time.Hour)
	got, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "syncedat",
		SyncedAt: &later,
	})
	require.NoError(t, err)
	require.NotNil(t, got.SyncedAt)
	assert.WithinDuration(t, later, *got.SyncedAt, time.Second)
}

// TestUpsertDiscoveryCandidate_DoesNotClearEmailsSetBySharedUpsert
// proves the dedicated DO UPDATE SET only touches the 6 columns it manages —
// anything seeded via the shared Upsert (emails, phones, etc.) is untouched.
func TestUpsertDiscoveryCandidate_DoesNotClearEmailsSetBySharedUpsert(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed via the shared Upsert so emails are populated.
	_, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "telegram",
		SourceID: prefix + "emails",
		Emails:   []repository.EmailEntry{{Value: "a@x", Type: "personal", Primary: true}},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	// Call the dedicated upsert with only names.
	_, err = repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:    "telegram",
		SourceID:  prefix + "emails",
		FirstName: strPtr("Dale"),
		Metadata:  map[string]any{"username": "@dale"},
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.GetBySource(ctx, "telegram", prefix+"emails", nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Emails, 1)
	assert.Equal(t, "a@x", got.Emails[0].Value, "existing email should survive the Telegram-specific upsert")
}

func TestUpsertDiscoveryCandidate_DoesNotTouchMatchStatus(t *testing.T) {
	t.Parallel()
	repo, database, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Create a real CRM contact so the FK (external_contact.crm_contact_id ->
	// contact.id) is satisfiable. The namespaced factory contact never
	// collides with other tests and cleans up via its scoped closure.
	gen, _ := migrationGenerator(t)
	crmContact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	defer contactCleanup()

	row, err := repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:   "telegram",
		SourceID: prefix + "match",
		SyncedAt: &now,
	})
	require.NoError(t, err)

	_, err = repo.UpdateMatch(ctx, row.ID, &crmContact.ID, repository.MatchStatusMatched)
	require.NoError(t, err)

	_, err = repo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
		Source:    "telegram",
		SourceID:  prefix + "match",
		FirstName: strPtr("Dale"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.GetBySource(ctx, "telegram", prefix+"match", nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, repository.MatchStatusMatched, got.MatchStatus, "dedicated upsert must not reset match_status")
	require.NotNil(t, got.CRMContactID)
	assert.Equal(t, crmContact.ID, *got.CRMContactID)
}

// TestUpsertExternalContact_SharedUpsertStillOverwrites confirms the shared
// upsert retains its "authoritative overwrite" semantics for Google flows —
// this plan only adds a parallel path, it does not change the shared one.
func TestUpsertExternalContact_SharedUpsertStillOverwrites(t *testing.T) {
	t.Parallel()
	repo, _, prefix, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:    "gcontacts",
		SourceID:  prefix + "shared-overwrite",
		FirstName: strPtr("Alice"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "gcontacts",
		SourceID: prefix + "shared-overwrite",
		// FirstName intentionally nil — shared upsert should clear it.
		SyncedAt: &now,
	})
	require.NoError(t, err)
	assert.Nil(t, got.FirstName, "shared UpsertExternalContact must still overwrite scalar fields with NULL for Google flows")
}

// TestUpdateDiscoveryCandidates_BatchPath_BlankStringsDoNotClobberStoredData
// exercises the end-to-end prod-healing path that caused the original bug:
// two unmatched messages for the same peer — one with blank entity strings
// (outbound private chat), one with real first/last name — are aggregated,
// and the batch UpdateDiscoveryCandidates call is expected to leave the
// external_contact row with populated names and a metadata.username, not
// wipe them.
func TestUpdateDiscoveryCandidates_BatchPath_BlankStringsDoNotClobberStoredData(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// Close via t.Cleanup (LIFO) so it runs AFTER the row-delete t.Cleanup below
	// — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	// Per-test-unique peer/chat IDs. The external_contact / external_identity
	// source_id is derived from the peer_user_id, so two parallel copies sharing
	// a fixed 99001 would collide on the row and on the peer-scoped cleanup.
	_, ns := migrationGenerator(t)
	testPeerID, peerStr := uniqueTestIDs(t, ns)
	testChatID := testPeerID + 1
	username := "daledobeck"
	empty := ""
	firstName := "Dale"
	lastName := "Dobeck"
	text := "hi"

	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identitySvc := service.NewIdentityService(identityRepo)
	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, nil, 2) // threshold = 2

	t.Cleanup(func() {
		_, _ = database.Queries.DeleteTelegramMessagesByPeerUserID(
			ctx,
			pgtype.Int8{Int64: testPeerID, Valid: true},
		)
		_, _ = database.Queries.DeleteExternalContactsBySourceIDPrefix(
			ctx,
			pgtype.Text{String: peerStr, Valid: true},
		)
		// External_identity row created by MatchOrCreate — also keyed by source_id.
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(
			ctx,
			pgtype.Text{String: peerStr, Valid: true},
		)
	})

	base := accelerated.GetCurrentTime().Truncate(time.Microsecond)

	// Newer row: blank entity strings (the outbound private-chat shape).
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 99001,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base,
		IsOutgoing:        true,
		PeerUserID:        ptrInt64(testPeerID),
		PeerUsername:      &empty,
		PeerFirstName:     &empty,
		PeerLastName:      &empty,
	})
	require.NoError(t, err)

	// Older row: good entity data.
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 99002,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base.Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(testPeerID),
		PeerUsername:      &username,
		PeerFirstName:     &firstName,
		PeerLastName:      &lastName,
	})
	require.NoError(t, err)

	// Required: MatchAllUnmatched creates the external_identity row so the
	// peer shows up as unmatched in ListDistinctUnmatchedPeers. Discovery then
	// populates external_contact.
	require.NoError(t, matcher.MatchAllUnmatched(ctx))
	require.NoError(t, matcher.UpdateDiscoveryCandidates(ctx))

	got, err := externalRepo.GetBySource(ctx, "telegram", peerStr, nil)
	require.NoError(t, err)
	require.NotNil(t, got, "discovery should have inserted an external_contact row")
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName, "batch path must surface the populated-name row, not the blank-string row")
	require.NotNil(t, got.LastName)
	assert.Equal(t, "Dobeck", *got.LastName)
	assert.Equal(t, "@daledobeck", got.Metadata["username"])

	// Running the batch again must be idempotent — existing names survive
	// even when future aggregation picks the blank-string row.
	require.NoError(t, matcher.UpdateDiscoveryCandidates(ctx))

	got, err = externalRepo.GetBySource(ctx, "telegram", peerStr, nil)
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName, "re-running discovery must not clobber stored names")
	assert.Equal(t, "@daledobeck", got.Metadata["username"])
}
