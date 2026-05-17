package tests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// randomSuffix returns a 12-char hex string for per-test isolation.
// Required because the test DB is shared across runs; the gotcha table
// warns that fixed identifiers cause cross-run pollution.
func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// setupMessagesMessageTest provisions a per-test mac_host row + repo
// pair. Cleanup hard-deletes scoped by mac_host_id (upsert does not
// clear deleted_at on conflict, so soft-delete would resurrect rows
// across runs per the gotcha table).
func setupMessagesMessageTest(t *testing.T) (context.Context, *repository.MessagesMessageRepository, *repository.ContactRepository, uuid.UUID, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	repo := repository.NewMessagesMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	// Per-test mac_host so HardDeleteByMacHost scopes our cleanup. We
	// seed a pre-revoked host so the singleton index
	// idx_mac_host_singleton (which only constrains rows WHERE
	// api_key_revoked_at IS NULL) never collides with a parallel
	// tests/api package that holds the live singleton slot. The tests
	// here only need a valid mac_host UUID as an FK target; they don't
	// exercise auth or pairing state.
	macHostRepo := repository.NewMacHostRepository(database.Queries)
	suffix := randomSuffix(t)
	host, err := macHostRepo.SeedRevokedHostForTest(ctx,
		"test-host-"+suffix, "test-version", 1, "test-hash-"+suffix)
	require.NoError(t, err)

	cleanup := func() {
		_ = repo.HardDeleteByMacHost(ctx, host.ID)
		database.Close()
	}
	return ctx, repo, contactRepo, host.ID, cleanup
}

func TestMessagesMessageRepository_UpsertAndGet(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Upsert " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "hello messages"
	guid := "guid-upsert-" + suffix

	msg, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid:             guid,
		ChatGuid:         "chat-" + suffix,
		PeerHandle:       "+15551234567",
		Text:             &text,
		MessageType:      "text",
		SentAt:           sentAt,
		IsOutgoing:       false,
		IsGroupChat:      false,
		MatchedContactID: &contact.ID,
		MacHostID:        &hostID,
	})
	require.NoError(t, err)
	assert.Equal(t, guid, msg.Guid)
	assert.Equal(t, "text", msg.MessageType)
	assert.NotNil(t, msg.MatchedContactID)
	assert.Equal(t, contact.ID, *msg.MatchedContactID)

	got, err := repo.GetMessage(ctx, guid)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, got.ID)
}

func TestMessagesMessageRepository_UpsertConflictIsNoOp(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test NoOp " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	originalText := "original"
	guid := "guid-conflict-" + suffix

	first, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: guid, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		Text: &originalText, MessageType: "text", SentAt: sentAt,
		MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	// Second upsert with different text — no-op on conflict.
	newText := "second push"
	second, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: guid, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		Text: &newText, MessageType: "text", SentAt: sentAt,
		MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "ID is stable across conflict")
	require.NotNil(t, second.Text)
	assert.Equal(t, "original", *second.Text, "ON CONFLICT preserves original text (no overwrite)")
}

func TestMessagesMessageRepository_ListUnprocessedByContact_ExcludesProcessed(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	databaseURL := os.Getenv("DATABASE_URL")
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Excl Proc " + suffix})
	require.NoError(t, err)
	defer func() {
		// Hard-delete the interaction we'll insert so it doesn't trip
		// the migration test's data-loss guard (see
		// interaction_source_messages_check_test.go for the same pattern).
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceMessages, "msgs-excl-proc-%")
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	}()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-proc-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt,
		MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	// Initially unprocessed → listed.
	list, err := repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Create a real interaction so the staging row's interaction_id FK
	// constraint is satisfied when MarkMessagesProcessed sets it.
	ref := "msgs-excl-proc-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceMessages,
		SourceRef:  &ref,
		OccurredAt: sentAt,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	// Mark processed (non-tx variant; no session scope needed for
	// processed-row filter testing).
	require.NoError(t, repo.MarkMessagesProcessed(ctx, []uuid.UUID{msg.ID}, interaction.ID))

	// Processed row → excluded from the unprocessed-by-contact list.
	list, err = repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "processed row must be excluded from unprocessed list")

	// Verify the row state on disk.
	got, err := repo.GetMessage(ctx, "guid-proc-"+suffix)
	require.NoError(t, err)
	require.NotNil(t, got.ProcessedAt)
	require.NotNil(t, got.InteractionID)
	assert.Equal(t, interaction.ID, *got.InteractionID)
}

func TestMessagesMessageRepository_ListUnprocessedByContact_ExcludesActiveClaim(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Excl Claim " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-claim-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt,
		MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	// Pre-claim a row using the non-tx Claim helper.
	claimed, err := repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "test:session:ref")
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// Active claim → list excludes the row.
	list, err := repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, list, 0, "actively-claimed row excluded from unprocessed list")
}

func TestMessagesMessageRepository_ListUnprocessedByContact_IncludesStaleClaim(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Stale " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-stale-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt,
		MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	// Claim then backdate past TTL.
	_, err = repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "test:stale:ref")
	require.NoError(t, err)
	require.NoError(t, repo.BackdateClaim(ctx, []uuid.UUID{msg.ID}))

	// Stale claim → row eligible for re-claim (listed).
	list, err := repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].ClaimedAt)
	require.NotNil(t, list[0].ClaimedSessionRef)
	assert.Equal(t, "test:stale:ref", *list[0].ClaimedSessionRef)
}

func TestMessagesMessageRepository_ClaimMessages_PartialClaim(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Partial " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	m1, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-p1-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt, MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)
	m2, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-p2-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt.Add(time.Second), MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	// Pre-claim m1 only. ClaimMessages requesting [m1, m2] returns just [m2].
	_, err = repo.ClaimMessages(ctx, []uuid.UUID{m1.ID}, "prior")
	require.NoError(t, err)

	claimed, err := repo.ClaimMessages(ctx, []uuid.UUID{m1.ID, m2.ID}, "new")
	require.NoError(t, err)
	assert.Len(t, claimed, 1)
	assert.Equal(t, m2.ID, claimed[0])
}

func TestMessagesMessageRepository_ClearStaleClaimTx(t *testing.T) {
	ctx, repo, contactRepo, hostID, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Clear " + suffix})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid: "guid-clear-" + suffix, ChatGuid: "chat-" + suffix, PeerHandle: "h",
		MessageType: "text", SentAt: sentAt, MatchedContactID: &contact.ID, MacHostID: &hostID,
	})
	require.NoError(t, err)

	_, err = repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "stale-ref")
	require.NoError(t, err)

	// Re-open a fresh DB connection / tx via the same pool.
	databaseURL := os.Getenv("DATABASE_URL")
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)

	err = repo.ClearStaleClaimTx(ctx, tx, []uuid.UUID{msg.ID}, "stale-ref")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// Row should now be unclaimed.
	got, err := repo.GetMessage(ctx, "guid-clear-"+suffix)
	require.NoError(t, err)
	assert.Nil(t, got.ClaimedAt)
	assert.Nil(t, got.ClaimedSessionRef)

	// Wrong expected ref → no-op.
	_, err = repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "other-ref")
	require.NoError(t, err)
	tx2, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	err = repo.ClearStaleClaimTx(ctx, tx2, []uuid.UUID{msg.ID}, "stale-ref") // doesn't match
	require.NoError(t, err)
	require.NoError(t, tx2.Commit(ctx))
	got2, err := repo.GetMessage(ctx, "guid-clear-"+suffix)
	require.NoError(t, err)
	require.NotNil(t, got2.ClaimedSessionRef)
	assert.Equal(t, "other-ref", *got2.ClaimedSessionRef, "wrong expected ref does not clear")
}

func TestMessagesMessageRepository_GetMessage_NotFound(t *testing.T) {
	ctx, repo, _, _, cleanup := setupMessagesMessageTest(t)
	defer cleanup()

	_, err := repo.GetMessage(ctx, "non-existent-guid-"+randomSuffix(t))
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
}
