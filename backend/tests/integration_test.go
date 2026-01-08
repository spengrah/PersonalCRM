package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

// getMigrationsPath returns the absolute path to the migrations directory
func getMigrationsPath() string {
	// If MIGRATIONS_PATH is set as absolute path, use it
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}

	// Otherwise, compute path relative to this test file
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename)
	return filepath.Join(testDir, "..", "migrations")
}

// TestRunMigrations_Integration tests the migration runner
func TestRunMigrations_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	migrationsPath := getMigrationsPath()

	t.Run("RunMigrations_NoChange", func(t *testing.T) {
		// Running migrations on an already-migrated database should succeed
		// and return nil (ErrNoChange is handled internally)
		err := db.RunMigrations(databaseURL, migrationsPath)
		assert.NoError(t, err)
	})

	t.Run("RunMigrations_InvalidPath", func(t *testing.T) {
		// Invalid migrations path should return error
		err := db.RunMigrations(databaseURL, "/nonexistent/path")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create migration instance")
	})

	t.Run("RunMigrations_InvalidDatabaseURL", func(t *testing.T) {
		// Invalid database URL should return error
		err := db.RunMigrations("postgres://invalid:invalid@localhost:9999/invalid?sslmode=disable", migrationsPath)
		assert.Error(t, err)
	})
}

// TestContactRepository_Integration tests the contact repository with a real database
// This test requires a running PostgreSQL database with the DATABASE_URL environment variable set
func TestContactRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Check if DATABASE_URL is set for integration testing
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Get test config and override DATABASE_URL if set in environment
	cfg := config.TestConfig()
	if databaseURL != "" {
		cfg.Database.URL = databaseURL
	}

	// Connect to database
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	// Create repository
	repo := repository.NewContactRepository(database.Queries)

	t.Run("CreateAndGetContact", func(t *testing.T) {
		// Create a contact
		req := repository.CreateContactRequest{
			FullName: "Integration Test User",
			Location: stringPtr("Test City"),
			Cadence:  stringPtr("monthly"),
		}

		createdContact, err := repo.CreateContact(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, createdContact)

		// Verify the created contact
		assert.Equal(t, "Integration Test User", createdContact.FullName)
		assert.Equal(t, "Test City", *createdContact.Location)
		assert.Equal(t, "monthly", *createdContact.Cadence)
		assert.NotEqual(t, uuid.Nil, createdContact.ID)

		// Get the contact by ID
		foundContact, err := repo.GetContact(ctx, createdContact.ID)
		require.NoError(t, err)
		require.NotNil(t, foundContact)

		assert.Equal(t, createdContact.ID, foundContact.ID)
		assert.Equal(t, createdContact.FullName, foundContact.FullName)

		// Clean up - delete the test contact
		err = repo.HardDeleteContact(ctx, createdContact.ID)
		require.NoError(t, err)
	})

	t.Run("ListContacts", func(t *testing.T) {
		// Create test contacts with unique emails to avoid conflicts
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Integration List Test User 1",
		})
		require.NoError(t, err)
		require.NotNil(t, contact1)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Integration List Test User 2",
		})
		require.NoError(t, err)
		require.NotNil(t, contact2)

		// List contacts
		contacts, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:  100,
			Offset: 0,
		})
		require.NoError(t, err)

		// Verify our test contacts are in the list
		foundContact1 := false
		foundContact2 := false
		for _, c := range contacts {
			if c.ID == contact1.ID {
				foundContact1 = true
				assert.Equal(t, "Integration List Test User 1", c.FullName)
			}
			if c.ID == contact2.ID {
				foundContact2 = true
				assert.Equal(t, "Integration List Test User 2", c.FullName)
			}
		}
		assert.True(t, foundContact1, "Contact 1 should be in the list")
		assert.True(t, foundContact2, "Contact 2 should be in the list")

		// Clean up
		err = repo.HardDeleteContact(ctx, contact1.ID)
		require.NoError(t, err)
		err = repo.HardDeleteContact(ctx, contact2.ID)
		require.NoError(t, err)
	})
}

// TestContactMethodRepository_Integration tests contact method CRUD
func TestContactMethodRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	if databaseURL != "" {
		cfg.Database.URL = databaseURL
	}

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Contact Method Test",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	method1, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      string(repository.ContactMethodEmailPersonal),
		Value:     "method.test@example.com",
		IsPrimary: true,
	})
	require.NoError(t, err)
	require.NotNil(t, method1)

	method2, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      string(repository.ContactMethodPhone),
		Value:     "+1-555-0100",
		IsPrimary: false,
	})
	require.NoError(t, err)
	require.NotNil(t, method2)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, methods, 2)
	assert.True(t, methods[0].IsPrimary)
	assert.Equal(t, string(repository.ContactMethodEmailPersonal), methods[0].Type)

	err = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)

	afterDelete, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, afterDelete, 0)
}

// TestSyncRepository_Integration tests the sync repository with a real database
func TestSyncRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Run migrations first
	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewSyncRepository(database.Queries)

	t.Run("CreateAndGetSyncState", func(t *testing.T) {
		// Create a sync state
		req := repository.CreateSyncStateRequest{
			Source:    "test_provider",
			AccountID: stringPtr("test@example.com"),
			Strategy:  repository.SyncStrategyContactDriven,
			Enabled:   true,
		}

		createdState, err := repo.CreateSyncState(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, createdState)

		// Verify the created state
		assert.Equal(t, "test_provider", createdState.Source)
		assert.Equal(t, "test@example.com", *createdState.AccountID)
		assert.Equal(t, repository.SyncStatusIdle, createdState.Status)
		assert.Equal(t, repository.SyncStrategyContactDriven, createdState.Strategy)
		assert.True(t, createdState.Enabled)
		assert.NotEqual(t, uuid.Nil, createdState.ID)

		// Get the state by ID
		foundState, err := repo.GetSyncState(ctx, createdState.ID)
		require.NoError(t, err)
		require.NotNil(t, foundState)

		assert.Equal(t, createdState.ID, foundState.ID)
		assert.Equal(t, createdState.Source, foundState.Source)

		// Clean up
		err = repo.DeleteSyncState(ctx, createdState.ID)
		require.NoError(t, err)
	})

	t.Run("GetSyncStateBySource", func(t *testing.T) {
		// Create a sync state
		req := repository.CreateSyncStateRequest{
			Source:    "gmail",
			AccountID: stringPtr("user@gmail.com"),
			Strategy:  repository.SyncStrategyFetchAll,
		}

		createdState, err := repo.CreateSyncState(ctx, req)
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, createdState.ID) }()

		// Get by source and account
		accountID := "user@gmail.com"
		foundState, err := repo.GetSyncStateBySource(ctx, "gmail", &accountID)
		require.NoError(t, err)
		require.NotNil(t, foundState)
		assert.Equal(t, createdState.ID, foundState.ID)

		// Try with wrong account - should not find
		wrongAccount := "other@gmail.com"
		_, err = repo.GetSyncStateBySource(ctx, "gmail", &wrongAccount)
		assert.Error(t, err)
	})

	t.Run("ListSyncStates", func(t *testing.T) {
		// Create multiple sync states
		state1, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "provider1",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state1.ID) }()

		state2, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "provider2",
			Strategy: repository.SyncStrategyFetchAll,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state2.ID) }()

		// List all states
		states, err := repo.ListSyncStates(ctx)
		require.NoError(t, err)

		// Verify our test states are in the list
		foundState1, foundState2 := false, false
		for _, s := range states {
			if s.ID == state1.ID {
				foundState1 = true
			}
			if s.ID == state2.ID {
				foundState2 = true
			}
		}
		assert.True(t, foundState1, "State 1 should be in the list")
		assert.True(t, foundState2, "State 2 should be in the list")
	})

	t.Run("UpdateSyncStateStatus", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "status_test",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Update to syncing
		_, err = repo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusSyncing, nil)
		require.NoError(t, err)

		updated, err := repo.GetSyncState(ctx, state.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.SyncStatusSyncing, updated.Status)

		// Update to error with message
		errMsg := "connection timeout"
		_, err = repo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusError, &errMsg)
		require.NoError(t, err)

		updated, err = repo.GetSyncState(ctx, state.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.SyncStatusError, updated.Status)
		assert.NotNil(t, updated.ErrorMessage)
		assert.Equal(t, "connection timeout", *updated.ErrorMessage)
	})

	t.Run("UpdateSyncStateEnabled", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "enable_test",
			Strategy: repository.SyncStrategyContactDriven,
			Enabled:  true,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Initially enabled
		assert.True(t, state.Enabled)

		// Disable
		updated, err := repo.UpdateSyncStateEnabled(ctx, state.ID, false)
		require.NoError(t, err)
		assert.False(t, updated.Enabled)

		// Re-enable
		updated, err = repo.UpdateSyncStateEnabled(ctx, state.ID, true)
		require.NoError(t, err)
		assert.True(t, updated.Enabled)
	})

	t.Run("SyncLogLifecycle", func(t *testing.T) {
		// Create a sync state first
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "log_test",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Create a sync log
		log, err := repo.CreateSyncLog(ctx, state)
		require.NoError(t, err)
		require.NotNil(t, log)
		assert.Equal(t, state.ID, log.SyncStateID)
		assert.Equal(t, "running", string(log.Status))

		// Complete the log successfully
		_, err = repo.CompleteSyncLog(ctx, log.ID, repository.CompleteSyncLogResult{
			Status:         "success",
			ItemsProcessed: 100,
			ItemsMatched:   50,
			ItemsCreated:   10,
		})
		require.NoError(t, err)

		// List logs for this state
		logs, err := repo.ListSyncLogsByState(ctx, state.ID, 10, 0)
		require.NoError(t, err)
		require.Len(t, logs, 1)
		assert.Equal(t, log.ID, logs[0].ID)
		assert.Equal(t, "success", string(logs[0].Status))
		assert.Equal(t, int32(100), logs[0].ItemsProcessed)
		assert.Equal(t, int32(50), logs[0].ItemsMatched)
		assert.Equal(t, int32(10), logs[0].ItemsCreated)
	})

	t.Run("SyncLogWithError", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "log_error_test",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		log, err := repo.CreateSyncLog(ctx, state)
		require.NoError(t, err)

		// Complete with error
		errMsg := "API rate limit exceeded"
		_, err = repo.CompleteSyncLog(ctx, log.ID, repository.CompleteSyncLogResult{
			Status:         "error",
			ItemsProcessed: 25,
			ErrorMessage:   &errMsg,
		})
		require.NoError(t, err)

		logs, err := repo.ListSyncLogsByState(ctx, state.ID, 10, 0)
		require.NoError(t, err)
		require.Len(t, logs, 1)
		assert.Equal(t, "error", string(logs[0].Status))
		assert.NotNil(t, logs[0].ErrorMessage)
		assert.Equal(t, "API rate limit exceeded", *logs[0].ErrorMessage)
	})

	t.Run("CountSyncLogs", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "count_test",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Create multiple logs
		for i := 0; i < 3; i++ {
			log, err := repo.CreateSyncLog(ctx, state)
			require.NoError(t, err)
			_, err = repo.CompleteSyncLog(ctx, log.ID, repository.CompleteSyncLogResult{
				Status:         "success",
				ItemsProcessed: 10,
				ItemsMatched:   5,
				ItemsCreated:   1,
			})
			require.NoError(t, err)
		}

		count, err := repo.CountSyncLogsByState(ctx, state.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(3), count)
	})

	t.Run("ListRecentSyncLogs", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "recent_test",
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Create a few logs
		for i := 0; i < 2; i++ {
			log, err := repo.CreateSyncLog(ctx, state)
			require.NoError(t, err)
			_, err = repo.CompleteSyncLog(ctx, log.ID, repository.CompleteSyncLogResult{
				Status:         "success",
				ItemsProcessed: 10,
				ItemsMatched:   5,
				ItemsCreated:   1,
			})
			require.NoError(t, err)
		}

		// Get recent logs across all sources
		logs, err := repo.ListRecentSyncLogs(ctx, 10)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(logs), 2)
	})

	t.Run("DeleteSyncStatesByAccountID", func(t *testing.T) {
		// Create sync states for different accounts
		account1 := "account1@example.com"
		account2 := "account2@example.com"

		// Create multiple sync states for account1
		state1a, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcontacts",
			AccountID: &account1,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)

		state1b, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcal",
			AccountID: &account1,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)

		// Create a sync state for account2 (should NOT be deleted)
		state2, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcontacts",
			AccountID: &account2,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state2.ID) }()

		// Create a sync state with NULL account_id (e.g., iMessage - should NOT be deleted)
		stateNull, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "imessage",
			AccountID: nil, // NULL account_id
			Strategy:  repository.SyncStrategyContactDriven,
			Enabled:   true,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, stateNull.ID) }()

		// Delete all sync states for account1
		err = repo.DeleteSyncStatesByAccountID(ctx, account1)
		require.NoError(t, err)

		// Verify account1 sync states are deleted
		_, err = repo.GetSyncState(ctx, state1a.ID)
		assert.Error(t, err, "state1a should be deleted")

		_, err = repo.GetSyncState(ctx, state1b.ID)
		assert.Error(t, err, "state1b should be deleted")

		// Verify account2 sync state still exists
		foundState2, err := repo.GetSyncState(ctx, state2.ID)
		require.NoError(t, err)
		assert.Equal(t, state2.ID, foundState2.ID, "account2 state should NOT be deleted")

		// Verify NULL account_id sync state still exists
		foundStateNull, err := repo.GetSyncState(ctx, stateNull.ID)
		require.NoError(t, err)
		assert.Equal(t, stateNull.ID, foundStateNull.ID, "NULL account state (iMessage) should NOT be deleted")
	})

	t.Run("DeleteSyncStatesByAccountID_NoMatches", func(t *testing.T) {
		// Deleting sync states for a non-existent account should succeed (no-op)
		err := repo.DeleteSyncStatesByAccountID(ctx, "nonexistent@example.com")
		require.NoError(t, err, "Deleting non-existent account should not error")
	})
}

// TestOAuthRepository_Integration tests the OAuth repository with a real database
func TestOAuthRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Run migrations first
	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewOAuthRepository(database.Queries)

	t.Run("UpsertAndGetCredential", func(t *testing.T) {
		// Create a credential
		accountName := "Test User"
		expiresAt := timeNow().Add(1 * time.Hour)
		req := repository.UpsertOAuthCredentialRequest{
			Provider:              "google",
			AccountID:             "test-upsert@example.com",
			AccountName:           &accountName,
			AccessTokenEncrypted:  []byte("encrypted-access-token"),
			RefreshTokenEncrypted: []byte("encrypted-refresh-token"),
			EncryptionNonce:       []byte("12-byte-nonce"),
			TokenType:             "Bearer",
			ExpiresAt:             &expiresAt,
			Scopes:                []string{"gmail.readonly", "calendar.readonly"},
		}

		cred, err := repo.Upsert(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, cred)
		defer func() { _ = repo.Delete(ctx, cred.ID) }()

		// Verify the created credential
		assert.Equal(t, "google", cred.Provider)
		assert.Equal(t, "test-upsert@example.com", cred.AccountID)
		assert.Equal(t, "Test User", *cred.AccountName)
		assert.Equal(t, []byte("encrypted-access-token"), cred.AccessTokenEncrypted)
		assert.NotEqual(t, uuid.Nil, cred.ID)

		// Get by provider and account
		found, err := repo.GetByProviderAndAccount(ctx, "google", "test-upsert@example.com")
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, cred.ID, found.ID)
		assert.Equal(t, cred.AccountID, found.AccountID)

		// Get by ID
		foundByID, err := repo.GetByID(ctx, cred.ID)
		require.NoError(t, err)
		require.NotNil(t, foundByID)
		assert.Equal(t, cred.ID, foundByID.ID)
	})

	t.Run("UpsertUpdatesExisting", func(t *testing.T) {
		// Create initial credential
		req := repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "test-update@example.com",
			AccessTokenEncrypted: []byte("initial-token"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly"},
		}

		initial, err := repo.Upsert(ctx, req)
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, initial.ID) }()

		// Upsert again with updated token
		req.AccessTokenEncrypted = []byte("updated-token")
		updated, err := repo.Upsert(ctx, req)
		require.NoError(t, err)

		// Should be same ID (upsert behavior)
		assert.Equal(t, initial.ID, updated.ID)
		assert.Equal(t, []byte("updated-token"), updated.AccessTokenEncrypted)
	})

	t.Run("ListByProvider", func(t *testing.T) {
		// Create multiple credentials
		cred1, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "list-test1@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred1.ID) }()

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "list-test2@example.com",
			AccessTokenEncrypted: []byte("token2"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"calendar.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred2.ID) }()

		// Create one for a different provider
		cred3, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "microsoft",
			AccountID:            "list-test@outlook.com",
			AccessTokenEncrypted: []byte("token3"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"mail.read"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred3.ID) }()

		// List google credentials
		googleCreds, err := repo.ListByProvider(ctx, "google")
		require.NoError(t, err)

		foundCred1, foundCred2 := false, false
		for _, c := range googleCreds {
			if c.ID == cred1.ID {
				foundCred1 = true
			}
			if c.ID == cred2.ID {
				foundCred2 = true
			}
			// Should not contain microsoft credential
			assert.NotEqual(t, cred3.ID, c.ID)
		}
		assert.True(t, foundCred1, "Cred1 should be in the list")
		assert.True(t, foundCred2, "Cred2 should be in the list")
	})

	t.Run("ListStatusesByProvider", func(t *testing.T) {
		accountName := "Status Test User"
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "status-test@example.com",
			AccountName:          &accountName,
			AccessTokenEncrypted: []byte("encrypted-token"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly", "calendar.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred.ID) }()

		statuses, err := repo.ListStatusesByProvider(ctx, "google")
		require.NoError(t, err)

		var foundStatus *repository.OAuthCredentialStatus
		for i, s := range statuses {
			if s.ID == cred.ID {
				foundStatus = &statuses[i]
				break
			}
		}
		require.NotNil(t, foundStatus, "Should find status for created credential")
		assert.Equal(t, "status-test@example.com", foundStatus.AccountID)
		assert.Equal(t, "Status Test User", *foundStatus.AccountName)
		assert.Len(t, foundStatus.Scopes, 2)
	})

	t.Run("GetStatus", func(t *testing.T) {
		accountName := "Get Status User"
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "get-status@example.com",
			AccountName:          &accountName,
			AccessTokenEncrypted: []byte("token"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"people.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred.ID) }()

		status, err := repo.GetStatus(ctx, cred.ID)
		require.NoError(t, err)
		require.NotNil(t, status)

		assert.Equal(t, cred.ID, status.ID)
		assert.Equal(t, "get-status@example.com", status.AccountID)
		assert.Equal(t, "Get Status User", *status.AccountName)
	})

	t.Run("UpdateTokens", func(t *testing.T) {
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "token-update@example.com",
			AccessTokenEncrypted: []byte("old-token"),
			EncryptionNonce:      []byte("old-nonce-123"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred.ID) }()

		// Update tokens
		newExpiry := timeNow().Add(2 * time.Hour)
		updated, err := repo.UpdateTokens(ctx, cred.ID, repository.UpdateOAuthTokensRequest{
			AccessTokenEncrypted:  []byte("new-token"),
			RefreshTokenEncrypted: []byte("new-refresh"),
			EncryptionNonce:       []byte("new-nonce-123"),
			ExpiresAt:             &newExpiry,
		})
		require.NoError(t, err)
		require.NotNil(t, updated)

		assert.Equal(t, []byte("new-token"), updated.AccessTokenEncrypted)
		assert.Equal(t, []byte("new-refresh"), updated.RefreshTokenEncrypted)
		assert.Equal(t, []byte("new-nonce-123"), updated.EncryptionNonce)
	})

	t.Run("Delete", func(t *testing.T) {
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "delete-test@example.com",
			AccessTokenEncrypted: []byte("token"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly"},
		})
		require.NoError(t, err)

		err = repo.Delete(ctx, cred.ID)
		require.NoError(t, err)

		// Should not find after delete
		_, err = repo.GetByID(ctx, cred.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("DeleteByProvider", func(t *testing.T) {
		// Create credentials for a test provider
		cred1, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "test_provider",
			AccountID:            "delete-by-provider1@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope1"},
		})
		require.NoError(t, err)

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "test_provider",
			AccountID:            "delete-by-provider2@example.com",
			AccessTokenEncrypted: []byte("token2"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope2"},
		})
		require.NoError(t, err)

		// Delete all credentials for the provider
		err = repo.DeleteByProvider(ctx, "test_provider")
		require.NoError(t, err)

		// Both should be gone
		_, err = repo.GetByID(ctx, cred1.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)

		_, err = repo.GetByID(ctx, cred2.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("Count", func(t *testing.T) {
		// Create credentials for counting
		cred1, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "count_test_provider",
			AccountID:            "count1@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred1.ID) }()

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "count_test_provider",
			AccountID:            "count2@example.com",
			AccessTokenEncrypted: []byte("token2"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred2.ID) }()

		count, err := repo.Count(ctx, "count_test_provider")
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})

	t.Run("NotFoundErrors", func(t *testing.T) {
		// Get non-existent by provider/account
		_, err := repo.GetByProviderAndAccount(ctx, "google", "nonexistent@example.com")
		assert.ErrorIs(t, err, db.ErrNotFound)

		// Get non-existent by ID
		_, err = repo.GetByID(ctx, uuid.New())
		assert.ErrorIs(t, err, db.ErrNotFound)

		// Get status of non-existent
		_, err = repo.GetStatus(ctx, uuid.New())
		assert.ErrorIs(t, err, db.ErrNotFound)

		// Update tokens of non-existent
		_, err = repo.UpdateTokens(ctx, uuid.New(), repository.UpdateOAuthTokensRequest{
			AccessTokenEncrypted: []byte("token"),
			EncryptionNonce:      []byte("nonce"),
		})
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}

// Helper function to get current time (for tests)
func timeNow() time.Time {
	return accelerated.GetCurrentTime()
}
