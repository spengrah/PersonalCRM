package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	tg "personal-crm/backend/internal/telegram"

	"github.com/gotd/td/telegram/updates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUserID int64 = 99999
const testChannelID int64 = 88888

func cleanupTelegramState(t *testing.T, queries db.Querier) {
	t.Helper()
	ctx := context.Background()
	_ = queries.DeleteTelegramUpdateState(ctx, testUserID)
	_ = queries.DeleteTelegramChannelState(ctx, testChannelID)
	_ = queries.DeleteTelegramChannelState(ctx, testChannelID+1)
}

func TestStateStorage_GetSetState(t *testing.T) {
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

	repo := repository.NewTelegramUpdateStateRepository(database.Queries)
	storage := tg.NewPostgresStateStorage(repo)

	cleanupTelegramState(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramState(t, database.Queries) })

	// Initially no state
	_, found, err := storage.GetState(ctx, testUserID)
	require.NoError(t, err)
	assert.False(t, found)

	// Set state
	err = storage.SetState(ctx, testUserID, updates.State{Pts: 100, Qts: 50, Seq: 10, Date: 1234})
	require.NoError(t, err)

	// Get state back
	state, found, err := storage.GetState(ctx, testUserID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 100, state.Pts)
	assert.Equal(t, 50, state.Qts)
	assert.Equal(t, 10, state.Seq)
	assert.Equal(t, 1234, state.Date)
}

func TestStateStorage_IndividualSetters(t *testing.T) {
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

	repo := repository.NewTelegramUpdateStateRepository(database.Queries)
	storage := tg.NewPostgresStateStorage(repo)

	cleanupTelegramState(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramState(t, database.Queries) })

	// Create initial state
	err = storage.SetState(ctx, testUserID, updates.State{Pts: 1, Qts: 1, Seq: 1, Date: 1})
	require.NoError(t, err)

	// Update individual fields
	require.NoError(t, storage.SetPts(ctx, testUserID, 200))
	require.NoError(t, storage.SetQts(ctx, testUserID, 300))
	require.NoError(t, storage.SetSeq(ctx, testUserID, 400))
	require.NoError(t, storage.SetDate(ctx, testUserID, 500))

	state, found, err := storage.GetState(ctx, testUserID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 200, state.Pts)
	assert.Equal(t, 300, state.Qts)
	assert.Equal(t, 400, state.Seq)
	assert.Equal(t, 500, state.Date)
}

func TestChannelHasher_GetSetAccessHash(t *testing.T) {
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

	repo := repository.NewTelegramUpdateStateRepository(database.Queries)
	hasher := tg.NewPostgresChannelHasher(repo)

	cleanupTelegramState(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramState(t, database.Queries) })

	// Initially not found
	_, found, err := hasher.GetChannelAccessHash(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.False(t, found)

	// Set access hash
	err = hasher.SetChannelAccessHash(ctx, testUserID, testChannelID, 9876543210)
	require.NoError(t, err)

	// Get it back
	hash, found, err := hasher.GetChannelAccessHash(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(9876543210), hash)
}

func TestChannelHasher_SetAccessHash_PreservesPts(t *testing.T) {
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

	repo := repository.NewTelegramUpdateStateRepository(database.Queries)
	hasher := tg.NewPostgresChannelHasher(repo)
	storage := tg.NewPostgresStateStorage(repo)

	cleanupTelegramState(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramState(t, database.Queries) })

	// Create channel with access hash and pts
	err = hasher.SetChannelAccessHash(ctx, testUserID, testChannelID, 1111)
	require.NoError(t, err)
	err = storage.SetChannelPts(ctx, testUserID, testChannelID, 999)
	require.NoError(t, err)

	// Verify pts is set
	pts, found, err := storage.GetChannelPts(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 999, pts)

	// Update access hash — should NOT reset pts
	err = hasher.SetChannelAccessHash(ctx, testUserID, testChannelID, 2222)
	require.NoError(t, err)

	// Verify pts is preserved
	pts, found, err = storage.GetChannelPts(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 999, pts, "pts should be preserved after access hash update")

	// Verify access hash was updated
	hash, found, err := hasher.GetChannelAccessHash(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(2222), hash)
}

func TestStateStorage_ChannelPts(t *testing.T) {
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

	repo := repository.NewTelegramUpdateStateRepository(database.Queries)
	storage := tg.NewPostgresStateStorage(repo)

	cleanupTelegramState(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramState(t, database.Queries) })

	// Not found initially
	_, found, err := storage.GetChannelPts(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.False(t, found)

	// SetChannelPts is an UPDATE — no row yet, so create via hasher first
	hasher := tg.NewPostgresChannelHasher(repo)
	err = hasher.SetChannelAccessHash(ctx, testUserID, testChannelID, 111)
	require.NoError(t, err)

	// Now set pts
	err = storage.SetChannelPts(ctx, testUserID, testChannelID, 42)
	require.NoError(t, err)

	pts, found, err := storage.GetChannelPts(ctx, testUserID, testChannelID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 42, pts)
}
