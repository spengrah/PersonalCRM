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
// and returns a cleanup function that hard-deletes the rows seeded by the test.
func setupDiscoveryUpsertTest(t *testing.T) (
	*repository.ExternalContactRepository,
	*db.Database,
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

	require.NoError(t, db.RunMigrations(context.Background(), databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)

	repo := repository.NewExternalContactRepository(database.Queries)

	cleanup := func() {
		// Narrow cleanup: only touch rows this test file creates. All test
		// source_ids are prefixed with "tg-discovery-upsert-". Use the sqlc
		// query (also used by the /test/cleanup handler) rather than raw SQL.
		_, _ = database.Queries.DeleteExternalContactsBySourceIDPrefix(
			context.Background(),
			pgtype.Text{String: "tg-discovery-upsert-", Valid: true},
		)
		database.Close()
	}
	return repo, database, cleanup
}

func TestUpsertTelegramDiscoveryCandidate_InsertsNewRow(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:    "tg-discovery-upsert-insert",
		DisplayName: strPtr("Dale Dobeck"),
		FirstName:   strPtr("Dale"),
		LastName:    strPtr("Dobeck"),
		Metadata:    map[string]any{"username": "@daledobeck", "message_count": 5},
		SyncedAt:    &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "telegram", got.Source)
	assert.Equal(t, "tg-discovery-upsert-insert", got.SourceID)
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

func TestUpsertTelegramDiscoveryCandidate_PreservesFirstNameWhenNil(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed with names populated.
	_, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:  "tg-discovery-upsert-preserve",
		FirstName: strPtr("Dale"),
		LastName:  strPtr("Dobeck"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	// Re-upsert with nil names — should preserve existing.
	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-preserve",
		SyncedAt: &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName)
	require.NotNil(t, got.LastName)
	assert.Equal(t, "Dobeck", *got.LastName)
}

func TestUpsertTelegramDiscoveryCandidate_OverwritesFirstNameWhenNewValueProvided(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:  "tg-discovery-upsert-overwrite",
		FirstName: strPtr("Dale"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:  "tg-discovery-upsert-overwrite",
		FirstName: strPtr("Daniel"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Daniel", *got.FirstName)
}

func TestUpsertTelegramDiscoveryCandidate_MergesMetadata_PreservesExistingKey(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed with a username key.
	_, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-merge-keep",
		Metadata: map[string]any{"username": "@dale", "message_count": 5},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	// Re-upsert with only message_count — username must be retained.
	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-merge-keep",
		Metadata: map[string]any{"message_count": 10},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	assert.Equal(t, "@dale", got.Metadata["username"], "username key should be preserved by JSONB merge")
	assert.EqualValues(t, 10, got.Metadata["message_count"], "incoming message_count should win for the duplicate key")
}

func TestUpsertTelegramDiscoveryCandidate_MergesMetadata_IncomingWinsForDuplicate(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-merge-new",
		Metadata: map[string]any{"username": "@old"},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-merge-new",
		Metadata: map[string]any{"username": "@new"},
		SyncedAt: &now,
	})
	require.NoError(t, err)
	assert.Equal(t, "@new", got.Metadata["username"])
}

func TestUpsertTelegramDiscoveryCandidate_SyncedAtAlwaysUpdates(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()

	start := accelerated.GetCurrentTime()
	_, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-syncedat",
		SyncedAt: &start,
	})
	require.NoError(t, err)

	later := start.Add(1 * time.Hour)
	got, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-syncedat",
		SyncedAt: &later,
	})
	require.NoError(t, err)
	require.NotNil(t, got.SyncedAt)
	assert.WithinDuration(t, later, *got.SyncedAt, time.Second)
}

// TestUpsertTelegramDiscoveryCandidate_DoesNotClearEmailsSetBySharedUpsert
// proves the dedicated DO UPDATE SET only touches the 6 columns it manages —
// anything seeded via the shared Upsert (emails, phones, etc.) is untouched.
func TestUpsertTelegramDiscoveryCandidate_DoesNotClearEmailsSetBySharedUpsert(t *testing.T) {
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Seed via the shared Upsert so emails are populated.
	_, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "telegram",
		SourceID: "tg-discovery-upsert-emails",
		Emails:   []repository.EmailEntry{{Value: "a@x", Type: "personal", Primary: true}},
		SyncedAt: &now,
	})
	require.NoError(t, err)

	// Call the dedicated upsert with only names.
	_, err = repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:  "tg-discovery-upsert-emails",
		FirstName: strPtr("Dale"),
		Metadata:  map[string]any{"username": "@dale"},
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.GetBySource(ctx, "telegram", "tg-discovery-upsert-emails", nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Emails, 1)
	assert.Equal(t, "a@x", got.Emails[0].Value, "existing email should survive the Telegram-specific upsert")
}

func TestUpsertTelegramDiscoveryCandidate_DoesNotTouchMatchStatus(t *testing.T) {
	repo, database, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	// Create a real CRM contact so the FK (external_contact.crm_contact_id ->
	// contact.id) is satisfiable. Use a unique name so it doesn't collide with
	// other tests and cleans up cleanly.
	contactRepo := repository.NewContactRepository(database.Queries)
	crmContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "tg-discovery-upsert-match-contact",
		LastContacted: &now,
	})
	require.NoError(t, err)
	defer func() {
		_, _ = database.Queries.DeleteContactsByNamePrefix(
			ctx,
			pgtype.Text{String: "tg-discovery-upsert-match-contact", Valid: true},
		)
	}()

	row, err := repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID: "tg-discovery-upsert-match",
		SyncedAt: &now,
	})
	require.NoError(t, err)

	_, err = repo.UpdateMatch(ctx, row.ID, &crmContact.ID, repository.MatchStatusMatched)
	require.NoError(t, err)

	_, err = repo.UpsertTelegramDiscoveryCandidate(ctx, repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:  "tg-discovery-upsert-match",
		FirstName: strPtr("Dale"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.GetBySource(ctx, "telegram", "tg-discovery-upsert-match", nil)
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
	repo, _, cleanup := setupDiscoveryUpsertTest(t)
	defer cleanup()

	ctx := context.Background()
	now := accelerated.GetCurrentTime()

	_, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:    "gcontacts",
		SourceID:  "tg-discovery-upsert-shared-overwrite",
		FirstName: strPtr("Alice"),
		SyncedAt:  &now,
	})
	require.NoError(t, err)

	got, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "gcontacts",
		SourceID: "tg-discovery-upsert-shared-overwrite",
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
	require.NoError(t, db.RunMigrations(context.Background(), databaseURL, getMigrationsPath()))

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	const testPeerID int64 = 99001
	const testChatID int64 = 9900
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
			pgtype.Text{String: "99001", Valid: true},
		)
		// External_identity row created by MatchOrCreate — also keyed by source_id.
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(
			ctx,
			pgtype.Text{String: "99001", Valid: true},
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

	got, err := externalRepo.GetBySource(ctx, "telegram", "99001", nil)
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

	got, err = externalRepo.GetBySource(ctx, "telegram", "99001", nil)
	require.NoError(t, err)
	require.NotNil(t, got.FirstName)
	assert.Equal(t, "Dale", *got.FirstName, "re-running discovery must not clobber stored names")
	assert.Equal(t, "@daledobeck", got.Metadata["username"])
}
