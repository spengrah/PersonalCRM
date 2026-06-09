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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// telegramTestChat returns a per-test-unique telegram chat ID and registers a
// chat-range cleanup that deletes only this test's own rows. Per-test chat IDs
// (vs the old fixed 12345/-100555/77777 shared across funcs) let the funcs run
// under t.Parallel() without their shared message-ID cleanup deleting each
// other's rows. The message IDs themselves stay as small literals — they are
// unique per (chat_id, message_id), and the chat_id is now unique per test.
func telegramTestChat(t *testing.T, repo *repository.TelegramMessageRepository) int64 {
	t.Helper()
	_, ns := migrationGenerator(t)
	base, _ := uniqueTestIDs(t, ns)
	clean := func() { _ = repo.HardDeleteByChatIDRange(context.Background(), base, base) }
	clean()
	t.Cleanup(clean)
	return base
}

func TestTelegramMessage_UpsertAndGet(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	chatID := telegramTestChat(t, repo)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "Hello, world!"
	username := "testuser"
	firstName := "Test"

	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        true,
		PeerUserID:        ptrInt64(67890),
		PeerUsername:      &username,
		PeerFirstName:     &firstName,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(90001), msg.TelegramMessageID)
	assert.Equal(t, chatID, msg.TelegramChatID)
	assert.Equal(t, "private", msg.ChatType)
	assert.Equal(t, &text, msg.MessageText)
	assert.Equal(t, "text", msg.MessageType)
	assert.True(t, msg.IsOutgoing)
	assert.Equal(t, ptrInt64(67890), msg.PeerUserID)
	assert.Equal(t, &username, msg.PeerUsername)
	assert.Equal(t, &firstName, msg.PeerFirstName)

	// Get it back
	got, err := repo.GetMessage(ctx, chatID, 90001)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, got.ID)
	assert.Equal(t, &text, got.MessageText)
}

func TestTelegramMessage_UpsertIdempotent(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	chatID := telegramTestChat(t, repo)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text1 := "First version"

	msg1, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90002,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text1,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Upsert again with same IDs — should update text, not create duplicate
	text2 := "Updated version"
	msg2, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90002,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text2,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)
	assert.Equal(t, msg1.ID, msg2.ID) // same row
	assert.Equal(t, &text2, msg2.MessageText)
}

func TestTelegramMessage_UpsertEdit(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	chatID := telegramTestChat(t, repo)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "Original"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90003,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        true,
	})
	require.NoError(t, err)

	// Upsert with edit
	editedText := "Edited text"
	editedAt := accelerated.GetCurrentTime().Add(time.Minute).Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90003,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &editedText,
		MessageType:       "text",
		SentAt:            sentAt,
		EditedAt:          &editedAt,
		IsOutgoing:        true,
	})
	require.NoError(t, err)
	assert.Equal(t, &editedText, msg.MessageText)
	assert.NotNil(t, msg.EditedAt)
}

func TestTelegramMessage_SoftDelete(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	chatID := telegramTestChat(t, repo)
	// SoftDeleteMessages is DB-wide by message_id (no chat filter), so use a
	// per-test-unique message ID derived from the unique chat base — otherwise a
	// parallel copy's row with the same fixed message ID would be soft-deleted.
	msgID := int32(chatID % 2_000_000_000)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "To be deleted"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: msgID,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Soft delete
	err = repo.SoftDeleteMessages(ctx, []int32{msgID})
	require.NoError(t, err)

	// Verify deleted — GetMessage filters deleted_at IS NULL, so should return not found
	_, err = repo.GetMessage(ctx, chatID, msgID)
	assert.Error(t, err)
}

func TestTelegramMessage_SoftDeleteChannel(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	// Channels use a negative chat ID. telegramTestChat registers cleanup on the
	// positive base range; this test stores under the negated id, so register a
	// matching negative-range cleanup too.
	base := telegramTestChat(t, repo)
	chatID := -base
	t.Cleanup(func() { _ = repo.HardDeleteByChatIDRange(context.Background(), chatID, chatID) })

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "Channel message"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90005,
		TelegramChatID:    chatID,
		ChatType:          "group",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Soft delete with chat_id filter
	err = repo.SoftDeleteChannelMessages(ctx, chatID, []int32{90005})
	require.NoError(t, err)

	_, err = repo.GetMessage(ctx, chatID, 90005)
	assert.Error(t, err)
}

func TestTelegramMessage_ListUnprocessed(t *testing.T) {
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
	// Close via t.Cleanup (LIFO) so it runs AFTER telegramTestChat's row-delete
	// cleanup — a defer would close the pool first and the deletes would no-op.
	t.Cleanup(func() { database.Close() })

	repo := repository.NewTelegramMessageRepository(database.Queries)

	chatID := telegramTestChat(t, repo)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "Unprocessed msg"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Scoped to this test's own unique chat, so >= 1 holds deterministically.
	msgs, err := repo.ListUnprocessedByChat(ctx, chatID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(msgs), 1)
}

func ptrInt64(v int64) *int64 { return &v }
