package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	tg "personal-crm/backend/internal/telegram"

	"github.com/gotd/td/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupTelegramSession(t *testing.T, queries db.Querier) {
	t.Helper()
	_ = queries.DeleteTelegramSession(context.Background())
}

func TestSessionRepository_UpsertAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(context.Background(), databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramSessionRepository(database.Queries)
	cleanupTelegramSession(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramSession(t, database.Queries) })

	phone := "+15551234567"
	var userID int64 = 12345
	username := "testuser"

	sess, err := repo.UpsertSession(ctx, repository.UpsertTelegramSessionParams{
		SessionDataEncrypted: []byte("encrypted-data"),
		EncryptionNonce:      []byte("nonce"),
		PhoneNumber:          &phone,
		TelegramUserID:       &userID,
		Username:             &username,
		AuthState:            "connected",
	})
	require.NoError(t, err)
	assert.Equal(t, "connected", sess.AuthState)
	assert.Equal(t, &phone, sess.PhoneNumber)
	assert.Equal(t, &userID, sess.TelegramUserID)
	assert.Equal(t, &username, sess.Username)

	// Get it back
	got, err := repo.GetSession(ctx)
	require.NoError(t, err)
	assert.Equal(t, "connected", got.AuthState)
	assert.Equal(t, []byte("encrypted-data"), got.SessionDataEncrypted)
}

func TestSessionRepository_UpdateAuthState(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(context.Background(), databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramSessionRepository(database.Queries)
	cleanupTelegramSession(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramSession(t, database.Queries) })

	// Create session in disconnected state
	_, err = repo.UpsertSession(ctx, repository.UpsertTelegramSessionParams{
		SessionDataEncrypted: []byte("data"),
		EncryptionNonce:      []byte("nonce"),
		AuthState:            "disconnected",
	})
	require.NoError(t, err)

	// Transition to connected
	updated, err := repo.UpdateAuthState(ctx, "connected")
	require.NoError(t, err)
	assert.Equal(t, "connected", updated.AuthState)
}

func TestSessionRepository_Delete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(context.Background(), databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramSessionRepository(database.Queries)
	cleanupTelegramSession(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramSession(t, database.Queries) })

	_, err = repo.UpsertSession(ctx, repository.UpsertTelegramSessionParams{
		SessionDataEncrypted: []byte("data"),
		EncryptionNonce:      []byte("nonce"),
		AuthState:            "disconnected",
	})
	require.NoError(t, err)

	err = repo.DeleteSession(ctx)
	require.NoError(t, err)

	_, err = repo.GetSession(ctx)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestDatabaseSessionStorage_EncryptDecrypt(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(context.Background(), databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramSessionRepository(database.Queries)
	cleanupTelegramSession(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramSession(t, database.Queries) })

	encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
	require.NoError(t, err)

	storage := tg.NewDatabaseSessionStorage(repo, encryptor)

	// Store session data
	testData := []byte(`{"version":1,"data":{"dc":2,"addr":"149.154.167.50:443","auth_key":"test"}}`)
	err = storage.StoreSession(ctx, testData)
	require.NoError(t, err)

	// Load it back — should match
	loaded, err := storage.LoadSession(ctx)
	require.NoError(t, err)
	assert.Equal(t, testData, loaded)
}

func TestDatabaseSessionStorage_NoSession(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	err := db.RunMigrations(context.Background(), databaseURL, getMigrationsPath())
	require.NoError(t, err)

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	repo := repository.NewTelegramSessionRepository(database.Queries)
	cleanupTelegramSession(t, database.Queries)
	t.Cleanup(func() { cleanupTelegramSession(t, database.Queries) })

	encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
	require.NoError(t, err)

	storage := tg.NewDatabaseSessionStorage(repo, encryptor)

	_, err = storage.LoadSession(ctx)
	assert.ErrorIs(t, err, session.ErrNotFound)
}
