package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupTelegramMessages(t *testing.T, queries db.Querier) {
	t.Helper()
	ctx := context.Background()
	// Clean up messages by soft-deleting them, or use direct SQL
	_ = queries.SoftDeleteTelegramMessages(ctx, []int32{90001, 90002, 90003, 90004, 90005})
}

func TestTelegramMessage_UpsertAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text := "Hello, world!"
	username := "testuser"
	firstName := "Test"

	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90001,
		TelegramChatID:    12345,
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
	assert.Equal(t, int64(12345), msg.TelegramChatID)
	assert.Equal(t, "private", msg.ChatType)
	assert.Equal(t, &text, msg.MessageText)
	assert.Equal(t, "text", msg.MessageType)
	assert.True(t, msg.IsOutgoing)
	assert.Equal(t, ptrInt64(67890), msg.PeerUserID)
	assert.Equal(t, &username, msg.PeerUsername)
	assert.Equal(t, &firstName, msg.PeerFirstName)

	// Get it back
	got, err := repo.GetMessage(ctx, 12345, 90001)
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

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text1 := "First version"

	msg1, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90002,
		TelegramChatID:    12345,
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
		TelegramChatID:    12345,
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

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text := "Original"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90003,
		TelegramChatID:    12345,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        true,
	})
	require.NoError(t, err)

	// Upsert with edit
	editedText := "Edited text"
	editedAt := time.Now().Add(time.Minute).Truncate(time.Microsecond)
	msg, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90003,
		TelegramChatID:    12345,
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

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text := "To be deleted"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90004,
		TelegramChatID:    12345,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Soft delete
	err = repo.SoftDeleteMessages(ctx, []int32{90004})
	require.NoError(t, err)

	// Verify deleted
	msg, err := repo.GetMessage(ctx, 12345, 90004)
	require.NoError(t, err)
	assert.NotNil(t, msg.DeletedAt)
}

func TestTelegramMessage_SoftDeleteChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text := "Channel message"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90005,
		TelegramChatID:    -100555,
		ChatType:          "group",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	// Soft delete with chat_id filter
	err = repo.SoftDeleteChannelMessages(ctx, -100555, []int32{90005})
	require.NoError(t, err)

	msg, err := repo.GetMessage(ctx, -100555, 90005)
	require.NoError(t, err)
	assert.NotNil(t, msg.DeletedAt)
}

func TestTelegramMessage_ListUnprocessed(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramMessageRepository(database.Queries)

	cleanupTelegramMessages(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramMessages(t, database.Queries) })

	sentAt := time.Now().Truncate(time.Microsecond)
	text := "Unprocessed msg"

	_, err = repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90001,
		TelegramChatID:    77777,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        false,
	})
	require.NoError(t, err)

	msgs, err := repo.ListUnprocessedByChat(ctx, 77777)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(msgs), 1)
}

func ptrInt64(v int64) *int64 { return &v }
