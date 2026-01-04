package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
