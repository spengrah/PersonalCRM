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

// chatConfigIDs returns three per-test-unique telegram chat IDs and registers a
// cleanup that deletes exactly those rows. Per-test IDs (vs the old fixed
// 70001-70003 package consts shared by every func) let the funcs run under
// t.Parallel() without clobbering each other's rows.
func chatConfigIDs(t *testing.T, queries db.Querier) (id1, id2, id3 int64) {
	t.Helper()
	_, ns := migrationGenerator(t)
	base, _ := uniqueTestIDs(t, ns)
	id1, id2, id3 = base, base+1, base+2
	clean := func() {
		ctx := context.Background()
		_ = queries.DeleteTelegramChatConfig(ctx, id1)
		_ = queries.DeleteTelegramChatConfig(ctx, id2)
		_ = queries.DeleteTelegramChatConfig(ctx, id3)
	}
	clean()
	t.Cleanup(clean)
	return id1, id2, id3
}

func TestChatConfig_UpsertAndList(t *testing.T) {
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
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	chatID1, _, _ := chatConfigIDs(t, database.Queries)

	title := "Test Group"
	mc := int32(5)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID1,
		ChatTitle:      &title,
		ChatType:       "group",
		MemberCount:    &mc,
		Status:         "auto",
	})
	require.NoError(t, err)

	// Scope the ListConfigs assertion to this test's own chat ID (a DB-wide len
	// check is shared with parallel tests).
	cfgs, err := repo.ListConfigs(ctx)
	require.NoError(t, err)
	found := false
	for _, c := range cfgs {
		if c.TelegramChatID == chatID1 {
			found = true
			break
		}
	}
	assert.True(t, found, "this test's chat config should be in ListConfigs")
}

func TestChatConfig_UpdateStatus(t *testing.T) {
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
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	chatID1, _, _ := chatConfigIDs(t, database.Queries)

	title := "Status Test"
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID1,
		ChatTitle:      &title,
		ChatType:       "group",
		Status:         "auto",
	})
	require.NoError(t, err)

	updated, err := repo.UpdateStatus(ctx, chatID1, "ignored")
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

	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	_, chatID2, _ := chatConfigIDs(t, database.Queries)

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID2,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)

	// Update cursor
	err = repo.UpdateBackfillCursor(ctx, chatID2, 500)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, chatID2)
	require.NoError(t, err)
	require.NotNil(t, got.BackfillCursor)
	assert.Equal(t, int32(500), *got.BackfillCursor)
	assert.False(t, got.BackfillComplete)

	// Mark complete
	err = repo.UpdateBackfillComplete(ctx, chatID2)
	require.NoError(t, err)

	got, err = repo.GetConfig(ctx, chatID2)
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

	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	_, _, chatID3 := chatConfigIDs(t, database.Queries)

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID3,
		ChatType:       "group",
		Status:         "auto",
	})
	require.NoError(t, err)

	// Complete it
	err = repo.UpdateBackfillComplete(ctx, chatID3)
	require.NoError(t, err)

	// Reset
	err = repo.ResetBackfill(ctx, chatID3)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, chatID3)
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

	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	chatID1, chatID2, _ := chatConfigIDs(t, database.Queries)

	// Create one incomplete and one complete
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID1,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)

	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID2,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillComplete(ctx, chatID2)
	require.NoError(t, err)

	// Only the incomplete one should appear. ListForBackfill returns only
	// incomplete chats, so the BackfillComplete=false check holds for every
	// returned row (including other parallel tests'); membership is scoped to
	// this test's own incomplete chat.
	chats, err := repo.ListForBackfill(ctx)
	require.NoError(t, err)

	found := false
	for _, c := range chats {
		if c.TelegramChatID == chatID1 {
			found = true
		}
		assert.False(t, c.BackfillComplete)
		assert.NotEqual(t, chatID2, c.TelegramChatID, "completed chat must not appear")
	}
	assert.True(t, found, "this test's incomplete chat should be in backfill list")
}

func TestChatConfig_UpdateMemberCount(t *testing.T) {
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
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	chatID1, _, _ := chatConfigIDs(t, database.Queries)

	mc := int32(5)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID1,
		ChatType:       "group",
		MemberCount:    &mc,
		Status:         "auto",
	})
	require.NoError(t, err)

	err = repo.UpdateMemberCount(ctx, chatID1, 25)
	require.NoError(t, err)

	got, err := repo.GetConfig(ctx, chatID1)
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

	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramChatConfigRepository(database.Queries)

	chatID1, chatID2, _ := chatConfigIDs(t, database.Queries)

	// Chat 1: has cursor (interrupted mid-backfill)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID1,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillCursor(ctx, chatID1, 750)
	require.NoError(t, err)

	// Chat 2: reset (retroactive backfill)
	_, err = repo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: chatID2,
		ChatType:       "group",
		Status:         "tracked",
	})
	require.NoError(t, err)
	err = repo.UpdateBackfillComplete(ctx, chatID2)
	require.NoError(t, err)
	err = repo.ResetBackfill(ctx, chatID2)
	require.NoError(t, err)

	// Both should appear in ListForBackfill
	chats, err := repo.ListForBackfill(ctx)
	require.NoError(t, err)

	var foundChat1, foundChat2 bool
	for _, c := range chats {
		if c.TelegramChatID == chatID1 {
			foundChat1 = true
			require.NotNil(t, c.BackfillCursor)
			assert.Equal(t, int32(750), *c.BackfillCursor, "cursor should be preserved")
		}
		if c.TelegramChatID == chatID2 {
			foundChat2 = true
			assert.Nil(t, c.BackfillCursor, "cursor should be nil after reset")
			assert.False(t, c.BackfillComplete)
		}
	}
	assert.True(t, foundChat1, "interrupted chat should be in backfill list")
	assert.True(t, foundChat2, "reset chat should be in backfill list")
}
