package tests

import (
	"context"
	"encoding/hex"
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

// chatIDFromSuffix maps a random hex suffix to a deterministic int64
// chat ID. Uses the first 4 bytes (8 hex chars) of the suffix to
// produce a 0-4G range so HardDeleteByChatIDRange cleans up only this
// test's rows even across parallel runs.
func chatIDFromSuffix(suffix string) int64 {
	raw, err := hex.DecodeString(suffix)
	if err != nil || len(raw) < 4 {
		return 1_000_000_000 // fallback; unlikely to collide for one test
	}
	return int64(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]))
}

// telegramClaimSetup gives us a fresh test contact and a chat-ID range
// scoped to the test so HardDeleteByChatIDRange cleans up only our rows.
func telegramClaimSetup(t *testing.T) (context.Context, *repository.TelegramMessageRepository, *repository.ContactRepository, *repository.Contact, int64, func()) {
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

	repo := repository.NewTelegramMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test Claim " + suffix,
	})
	require.NoError(t, err)

	// Per-test chat_id (large randomized value so we don't collide
	// with other tests in the shared DB). The hex suffix is converted
	// to int64 via the low 4 bytes — enough entropy for parallel test
	// isolation. The chatID is used as the cleanup range [chatID, chatID].
	chatID := chatIDFromSuffix(suffix)

	cleanup := func() {
		_ = repo.HardDeleteByChatIDRange(ctx, chatID, chatID)
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
		database.Close()
	}
	return ctx, repo, contactRepo, contact, chatID, cleanup
}

func TestTelegramMessageRepository_ClaimMessages_SetsClaimColumns(t *testing.T) {
	ctx, repo, _, contact, chatID, cleanup := telegramClaimSetup(t)
	defer cleanup()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(11111),
	})
	require.NoError(t, err)

	// Link the message to the contact so the unprocessed-by-contact
	// list-query returns it.
	require.NoError(t, repo.UpdateMessageContact(ctx, *msg.PeerUserID, contact.ID))

	claimed, err := repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "tg:claim:ref")
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, msg.ID, claimed[0])

	got, err := repo.GetMessage(ctx, chatID, 80001)
	require.NoError(t, err)
	require.NotNil(t, got.ClaimedAt)
	require.NotNil(t, got.ClaimedSessionRef)
	assert.Equal(t, "tg:claim:ref", *got.ClaimedSessionRef)
}

func TestTelegramMessageRepository_ListUnprocessedByContact_ExcludesActiveClaim(t *testing.T) {
	ctx, repo, _, contact, chatID, cleanup := telegramClaimSetup(t)
	defer cleanup()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80002,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(22222),
	})
	require.NoError(t, err)
	require.NoError(t, repo.UpdateMessageContact(ctx, *msg.PeerUserID, contact.ID))

	// Initially unprocessed AND unclaimed → listed.
	list, err := repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Claim → excluded.
	_, err = repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "active")
	require.NoError(t, err)
	list, err = repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, list, 0, "actively-claimed row excluded")
}

func TestTelegramMessageRepository_ListUnprocessedByContact_IncludesStaleClaim(t *testing.T) {
	ctx, repo, _, contact, chatID, cleanup := telegramClaimSetup(t)
	defer cleanup()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80003,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(33333),
	})
	require.NoError(t, err)
	require.NoError(t, repo.UpdateMessageContact(ctx, *msg.PeerUserID, contact.ID))

	_, err = repo.ClaimMessages(ctx, []uuid.UUID{msg.ID}, "stale-ref")
	require.NoError(t, err)
	require.NoError(t, repo.BackdateClaim(ctx, []uuid.UUID{msg.ID}))

	list, err := repo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].ClaimedSessionRef)
	assert.Equal(t, "stale-ref", *list[0].ClaimedSessionRef,
		"stale claim retained on row for engine recovery-detection")
}

func TestTelegramMessageRepository_ClaimMessages_PartialClaim(t *testing.T) {
	ctx, repo, _, contact, chatID, cleanup := telegramClaimSetup(t)
	defer cleanup()

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	m1, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80004, TelegramChatID: chatID, ChatType: "private",
		MessageType: "text", SentAt: sentAt, IsOutgoing: false, PeerUserID: ptrInt64(44441),
	})
	require.NoError(t, err)
	m2, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80005, TelegramChatID: chatID, ChatType: "private",
		MessageType: "text", SentAt: sentAt.Add(time.Second), IsOutgoing: false, PeerUserID: ptrInt64(44441),
	})
	require.NoError(t, err)
	require.NoError(t, repo.UpdateMessageContact(ctx, 44441, contact.ID))

	// Pre-claim m1 only. Request [m1, m2] → return just [m2].
	_, err = repo.ClaimMessages(ctx, []uuid.UUID{m1.ID}, "prior")
	require.NoError(t, err)
	claimed, err := repo.ClaimMessages(ctx, []uuid.UUID{m1.ID, m2.ID}, "new")
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, m2.ID, claimed[0])
}
