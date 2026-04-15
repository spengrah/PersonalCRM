package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testChatID1 int64 = 70001
const testChatID2 int64 = 70002
const testChatID3 int64 = 70003

func cleanupTelegramChatConfigs(t *testing.T, queries db.Querier) {
	t.Helper()
	ctx := context.Background()
	_ = queries.DeleteTelegramChatConfig(ctx, testChatID1)
	_ = queries.DeleteTelegramChatConfig(ctx, testChatID2)
	_ = queries.DeleteTelegramChatConfig(ctx, testChatID3)
}

func TestChatConfig_UpsertAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	title := "Test Group"
	mc := int32(5)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID1,
		ChatTitle:      &title,
		ChatType:       "group",
		MemberCount:    &mc,
		Status:         "auto",
	})
	require.NoError(t, err)

	cfgs, err := repo.ListConfigs(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(cfgs), 1)
}

func TestChatConfig_UpdateStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	title := "Status Test"
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID1,
		ChatTitle:      &title,
		ChatType:       "group",
		Status:         "auto",
	})
	require.NoError(t, err)

	updated, err := repo.UpdateStatus(ctx, testChatID1, "ignored")
	require.NoError(t, err)
	assert.Equal(t, "ignored", updated.Status)
}

func TestChatConfig_BackfillCursorAndComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID2,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)

	// Update cursor
	err = repo.UpdateBackfillCursor(ctx, testChatID2, 500)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, testChatID2)
	require.NoError(t, err)
	require.NotNil(t, got.BackfillCursor)
	assert.Equal(t, int32(500), *got.BackfillCursor)
	assert.False(t, got.BackfillComplete)

	// Mark complete
	err = repo.UpdateBackfillComplete(ctx, testChatID2)
	require.NoError(t, err)

	got, err = repo.GetConfig(ctx, testChatID2)
	require.NoError(t, err)
	assert.True(t, got.BackfillComplete)
	assert.Nil(t, got.BackfillCursor) // cleared on complete
}

func TestChatConfig_ResetBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID3,
		ChatType:       "group",
		Status:         "auto",
	})
	require.NoError(t, err)

	// Complete it
	err = repo.UpdateBackfillComplete(ctx, testChatID3)
	require.NoError(t, err)

	// Reset
	err = repo.ResetBackfill(ctx, testChatID3)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, testChatID3)
	require.NoError(t, err)
	assert.False(t, got.BackfillComplete)
	assert.Nil(t, got.BackfillCursor)
}

func TestChatConfig_ListForBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	// Create one incomplete and one complete
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID1,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID2,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillComplete(ctx, testChatID2)
	require.NoError(t, err)

	// Only the incomplete one should appear
	chats, err := repo.ListForBackfill(ctx)
	require.NoError(t, err)

	found := false
	for _, c := range chats {
		if c.TelegramChatID == testChatID1 {
			found = true
		}
		assert.False(t, c.BackfillComplete)
	}
	assert.True(t, found, "testChatID1 should be in backfill list")
}

func TestChatConfig_UpdateMemberCount(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	mc := int32(5)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID1,
		ChatType:       "group",
		MemberCount:    &mc,
		Status:         "auto",
	})
	require.NoError(t, err)

	err = repo.UpdateMemberCount(ctx, testChatID1, 25)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, testChatID1)
	require.NoError(t, err)
	require.NotNil(t, got.MemberCount)
	assert.Equal(t, int32(25), *got.MemberCount)
}

func TestChatConfig_BackfillResume(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	cleanupTelegramChatConfigs(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramChatConfigs(t, database.Queries) })

	// Chat 1: has cursor (interrupted mid-backfill)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID1,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillCursor(ctx, testChatID1, 750)
	require.NoError(t, err)

	// Chat 2: reset (retroactive backfill)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: testChatID2,
		ChatType:       "group",
		Status:         "tracked",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillComplete(ctx, testChatID2)
	require.NoError(t, err)
	err = repo.ResetBackfill(ctx, testChatID2)
	require.NoError(t, err)

	// Both should appear in ListForBackfill
	chats, err := repo.ListForBackfill(ctx)
	require.NoError(t, err)

	var foundChat1, foundChat2 bool
	for _, c := range chats {
		if c.TelegramChatID == testChatID1 {
			foundChat1 = true
			require.NotNil(t, c.BackfillCursor)
			assert.Equal(t, int32(750), *c.BackfillCursor, "cursor should be preserved")
		}
		if c.TelegramChatID == testChatID2 {
			foundChat2 = true
			assert.Nil(t, c.BackfillCursor, "cursor should be nil after reset")
			assert.False(t, c.BackfillComplete)
		}
	}
	assert.True(t, foundChat1, "interrupted chat should be in backfill list")
	assert.True(t, foundChat2, "reset chat should be in backfill list")
}
