package tests

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// TestMain lives in testmain_integration_test.go (build-tagged) and clones a
// per-package template database via testdb.SetupPackage. The getMigrationsPath
// helper below is shared between the tagged bridge and the migration tests in
// this package, so it stays untagged here.

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
		err := db.RunMigrations(context.Background(), databaseURL, migrationsPath)
		assert.NoError(t, err)
	})

	t.Run("RunMigrations_InvalidPath", func(t *testing.T) {
		// Invalid migrations path should return error
		err := db.RunMigrations(context.Background(), databaseURL, "/nonexistent/path")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create migration instance")
	})

	t.Run("RunMigrations_InvalidDatabaseURL", func(t *testing.T) {
		// Invalid database URL should return error
		err := db.RunMigrations(context.Background(), "postgres://invalid:invalid@localhost:9999/invalid?sslmode=disable", migrationsPath)
		assert.Error(t, err)
	})
}

func TestMigration020_UnifyEmailDedup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
	})

	_, err = tx.Exec(ctx, `ALTER TABLE contact_method DROP CONSTRAINT IF EXISTS contact_method_type_check`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `ALTER TABLE contact_method DISABLE TRIGGER set_contact_method_value_normalized`)
	require.NoError(t, err)

	var contactID uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO contact (full_name) VALUES ('Migration 020 Test') RETURNING id`).Scan(&contactID)
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `
		INSERT INTO contact_method (contact_id, type, value, value_normalized, is_primary)
		VALUES ($1, 'email_personal', 'User@Example.com', 'user@example.com', FALSE),
		       ($1, 'email_work', 'user@example.com', 'user@example.com', TRUE)
	`, contactID)
	require.NoError(t, err)

	migrationPath := filepath.Join(getMigrationsPath(), "020_unify_email_contact_method_type.up.sql")
	migrationSQL, err := os.ReadFile(migrationPath)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(t, string(migrationSQL)) {
		_, err = tx.Exec(ctx, statement)
		require.NoErrorf(t, err, "migration statement failed: %s", statement)
	}

	rows, err := tx.Query(ctx, `
		SELECT type, value, is_primary, value_normalized
		FROM contact_method
		WHERE contact_id = $1
	`, contactID)
	require.NoError(t, err)
	defer rows.Close()

	type row struct {
		methodType      string
		value           string
		isPrimary       bool
		valueNormalized string
	}

	results := []row{}
	for rows.Next() {
		var r row
		err = rows.Scan(&r.methodType, &r.value, &r.isPrimary, &r.valueNormalized)
		require.NoError(t, err)
		results = append(results, r)
	}
	require.NoError(t, rows.Err())

	require.Len(t, results, 1)
	assert.Equal(t, "email", results[0].methodType)
	assert.Equal(t, "user@example.com", results[0].valueNormalized)
	assert.True(t, results[0].isPrimary)
	assert.Equal(t, "user@example.com", results[0].value)
}

func splitSQLStatements(t *testing.T, sqlText string) []string {
	t.Helper()

	var builder strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(sqlText))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	require.NoError(t, scanner.Err())

	raw := builder.String()
	parts := strings.Split(raw, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement == "" {
			continue
		}
		statements = append(statements, statement)
	}
	return statements
}

// TestContactRepository_Integration tests the contact repository with a real database
// This test requires a running PostgreSQL database with the DATABASE_URL environment variable set
func TestContactRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()
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
		// Namespaced names so the FullName assertions don't collide with a
		// parallel copy. Membership keys on contact.ID, which is always scoped.
		ns := uuid.New().String()[:8]
		name1 := "Integration List Test User 1 " + ns
		name2 := "Integration List Test User 2 " + ns
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name1,
		})
		require.NoError(t, err)
		require.NotNil(t, contact1)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name2,
		})
		require.NoError(t, err)
		require.NotNil(t, contact2)

		// List contacts. A high limit keeps both rows in the window even as the
		// shared test DB accumulates state across runs.
		contacts, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:  100000,
			Offset: 0,
		})
		require.NoError(t, err)

		// Verify our test contacts are in the list (membership by ID)
		foundContact1 := false
		foundContact2 := false
		for _, c := range contacts {
			if c.ID == contact1.ID {
				foundContact1 = true
				assert.Equal(t, name1, c.FullName)
			}
			if c.ID == contact2.ID {
				foundContact2 = true
				assert.Equal(t, name2, c.FullName)
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

	t.Parallel()
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
		Type:      string(repository.ContactMethodEmail),
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
	assert.Equal(t, string(repository.ContactMethodEmail), methods[0].Type)

	duplicate, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      string(repository.ContactMethodEmail),
		Value:     " Method.Test@Example.com ",
		IsPrimary: false,
	})
	assert.Nil(t, duplicate)
	assert.Error(t, err)

	err = methodRepo.UpdateContactMethod(ctx, method2.ID, repository.UpdateContactMethodRequest{
		Type:  method2.Type,
		Value: " (555) 0100 ",
	})
	require.NoError(t, err)

	methods, err = methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	for _, method := range methods {
		if method.ID == method2.ID {
			assert.Equal(t, "+5550100", method.ValueNormalized)
		}
	}

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

	t.Parallel()
	ctx := context.Background()

	// Migrations are applied once by TestMain.
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewSyncRepository(database.Queries)

	// Per-test-run unique suffix so the (source, account_id) unique constraint
	// and the account-scoped delete don't collide with a parallel copy.
	ns := uuid.New().String()[:8]

	t.Run("CreateAndGetSyncState", func(t *testing.T) {
		// Create a sync state
		source := "test_provider_" + ns
		account := "test." + ns + "@example.com"
		req := repository.CreateSyncStateRequest{
			Source:    source,
			AccountID: stringPtr(account),
			Strategy:  repository.SyncStrategyContactDriven,
			Enabled:   true,
		}

		createdState, err := repo.CreateSyncState(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, createdState)

		// Verify the created state
		assert.Equal(t, source, createdState.Source)
		assert.Equal(t, account, *createdState.AccountID)
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
		gmailSource := "gmail_" + ns
		account := "user." + ns + "@gmail.com"
		req := repository.CreateSyncStateRequest{
			Source:    gmailSource,
			AccountID: stringPtr(account),
			Strategy:  repository.SyncStrategyFetchAll,
		}

		createdState, err := repo.CreateSyncState(ctx, req)
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, createdState.ID) }()

		// Get by source and account
		foundState, err := repo.GetSyncStateBySource(ctx, gmailSource, &account)
		require.NoError(t, err)
		require.NotNil(t, foundState)
		assert.Equal(t, createdState.ID, foundState.ID)

		// Try with wrong account - should not find
		wrongAccount := "other." + ns + "@gmail.com"
		_, err = repo.GetSyncStateBySource(ctx, gmailSource, &wrongAccount)
		assert.Error(t, err)
	})

	t.Run("ListSyncStates", func(t *testing.T) {
		// Create multiple sync states
		state1, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "provider1_" + ns,
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state1.ID) }()

		state2, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "provider2_" + ns,
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
			Source:   "status_test_" + ns,
			Strategy: repository.SyncStrategyContactDriven,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state.ID) }()

		// Update to error with message — exercises the status + error_message
		// round-trip. The legacy 'syncing' value is no longer written by any
		// Go code path, so the repository round-trip is exercised with the
		// 'error' status instead.
		errMsg := "connection timeout"
		_, err = repo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusError, &errMsg)
		require.NoError(t, err)

		updated, err := repo.GetSyncState(ctx, state.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.SyncStatusError, updated.Status)
		assert.NotNil(t, updated.ErrorMessage)
		assert.Equal(t, "connection timeout", *updated.ErrorMessage)
	})

	t.Run("UpdateSyncStateEnabled", func(t *testing.T) {
		state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "enable_test_" + ns,
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
			Source:   "log_test_" + ns,
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
			Source:   "log_error_test_" + ns,
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
			Source:   "count_test_" + ns,
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
			Source:   "recent_test_" + ns,
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
		// Per-test-unique accounts so the account-scoped delete only touches this
		// test's rows, not a parallel copy's.
		account1 := "account1." + ns + "@example.com"
		account2 := "account2." + ns + "@example.com"

		// Create multiple sync states for account1
		state1a, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcontacts_" + ns,
			AccountID: &account1,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)

		state1b, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcal_" + ns,
			AccountID: &account1,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)

		// Create a sync state for account2 (should NOT be deleted)
		state2, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "gcontacts2_" + ns,
			AccountID: &account2,
			Strategy:  repository.SyncStrategyFetchAll,
			Enabled:   true,
		})
		require.NoError(t, err)
		defer func() { _ = repo.DeleteSyncState(ctx, state2.ID) }()

		// Create a sync state with NULL account_id (e.g., iMessage - should NOT be deleted)
		stateNull, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    "imessage_" + ns,
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

	t.Parallel()
	ctx := context.Background()

	// Migrations are applied once by TestMain.
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewOAuthRepository(database.Queries)

	// Per-test-run unique suffix so the (provider, account_id) upsert key, the
	// provider-scoped Count, and DeleteByProvider don't collide with a parallel
	// copy.
	ns := uuid.New().String()[:8]

	t.Run("UpsertAndGetCredential", func(t *testing.T) {
		// Create a credential
		account := "test-upsert." + ns + "@example.com"
		accountName := "Test User"
		expiresAt := timeNow().Add(1 * time.Hour)
		req := repository.UpsertOAuthCredentialRequest{
			Provider:              "google",
			AccountID:             account,
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
		assert.Equal(t, account, cred.AccountID)
		assert.Equal(t, "Test User", *cred.AccountName)
		assert.Equal(t, []byte("encrypted-access-token"), cred.AccessTokenEncrypted)
		assert.NotEqual(t, uuid.Nil, cred.ID)

		// Get by provider and account
		found, err := repo.GetByProviderAndAccount(ctx, "google", account)
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
			AccountID:            "test-update." + ns + "@example.com",
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
			AccountID:            "list-test1." + ns + "@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"gmail.readonly"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred1.ID) }()

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "list-test2." + ns + "@example.com",
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
			AccountID:            "list-test." + ns + "@outlook.com",
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
		statusAccount := "status-test." + ns + "@example.com"
		accountName := "Status Test User"
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            statusAccount,
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
		assert.Equal(t, statusAccount, foundStatus.AccountID)
		assert.Equal(t, "Status Test User", *foundStatus.AccountName)
		assert.Len(t, foundStatus.Scopes, 2)
	})

	t.Run("GetStatus", func(t *testing.T) {
		getStatusAccount := "get-status." + ns + "@example.com"
		accountName := "Get Status User"
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            getStatusAccount,
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
		assert.Equal(t, getStatusAccount, status.AccountID)
		assert.Equal(t, "Get Status User", *status.AccountName)
	})

	t.Run("UpdateTokens", func(t *testing.T) {
		cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             "google",
			AccountID:            "token-update." + ns + "@example.com",
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
			AccountID:            "delete-test." + ns + "@example.com",
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
		// Use a per-test-unique provider so DeleteByProvider only removes this
		// test's rows, not a parallel copy's.
		deleteProvider := "test_provider_" + ns
		cred1, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             deleteProvider,
			AccountID:            "delete-by-provider1." + ns + "@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope1"},
		})
		require.NoError(t, err)

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             deleteProvider,
			AccountID:            "delete-by-provider2." + ns + "@example.com",
			AccessTokenEncrypted: []byte("token2"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope2"},
		})
		require.NoError(t, err)

		// Delete all credentials for the provider
		err = repo.DeleteByProvider(ctx, deleteProvider)
		require.NoError(t, err)

		// Both should be gone
		_, err = repo.GetByID(ctx, cred1.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)

		_, err = repo.GetByID(ctx, cred2.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("Count", func(t *testing.T) {
		// Per-test-unique provider so the exact Count == 2 is over this test's
		// own rows only.
		countProvider := "count_test_provider_" + ns
		cred1, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             countProvider,
			AccountID:            "count1." + ns + "@example.com",
			AccessTokenEncrypted: []byte("token1"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred1.ID) }()

		cred2, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             countProvider,
			AccountID:            "count2." + ns + "@example.com",
			AccessTokenEncrypted: []byte("token2"),
			EncryptionNonce:      []byte("12-byte-nonce"),
			TokenType:            "Bearer",
			Scopes:               []string{"scope"},
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, cred2.ID) }()

		count, err := repo.Count(ctx, countProvider)
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

// TestFindSimilarContactsBatch_Integration tests the batch matching query
func TestFindSimilarContactsBatch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	ctx := context.Background()

	// Migrations are applied once by TestMain.
	// Create database connection
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8, // mirrors the lowered TestConfig() ceiling for parallel tests
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	t.Run("EmptyInput", func(t *testing.T) {
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{}, 0.3, 5)
		require.NoError(t, err)
		assert.Empty(t, results)
	})

	t.Run("SingleCandidate_WithMatch", func(t *testing.T) {
		// Create a contact to match against
		uniqueSuffix := uuid.New().String()[:8]
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Jane Smith " + uniqueSuffix,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Add email method
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      "email",
			Value:     "jane." + uniqueSuffix + "@example.com",
		})
		require.NoError(t, err)

		// Search with batch method
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{
			{
				CandidateID:   "test-candidate-1",
				CandidateName: "Jane Smith " + uniqueSuffix,
			},
		}, 0.3, 5)

		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "test-candidate-1", results[0].CandidateID)
		require.NotEmpty(t, results[0].Matches)
		assert.Equal(t, contact.ID, results[0].Matches[0].Contact.ID)
		assert.True(t, results[0].Matches[0].Similarity > 0.9)
		// Verify methods are included
		assert.NotEmpty(t, results[0].Matches[0].Contact.Methods)
	})

	t.Run("MultipleCandidates_MixedResults", func(t *testing.T) {
		// Create contacts to match against
		uniqueSuffix := uuid.New().String()[:8]

		contact1, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Alice Johnson " + uniqueSuffix,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact1.ID) }()

		contact2, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Bob Williams " + uniqueSuffix,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact2.ID) }()

		// Search with batch method - one should match, one should not
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{
			{
				CandidateID:   "candidate-alice",
				CandidateName: "Alice Johnson " + uniqueSuffix,
			},
			{
				CandidateID:   "candidate-unknown",
				CandidateName: "Zzz Completely Different Name Xyz",
			},
			{
				CandidateID:   "candidate-bob",
				CandidateName: "Bob Williams " + uniqueSuffix,
			},
		}, 0.3, 5)

		require.NoError(t, err)
		require.Len(t, results, 3)

		// First candidate should match
		assert.Equal(t, "candidate-alice", results[0].CandidateID)
		require.NotEmpty(t, results[0].Matches)
		assert.Equal(t, contact1.ID, results[0].Matches[0].Contact.ID)

		// Second candidate should not match
		assert.Equal(t, "candidate-unknown", results[1].CandidateID)
		assert.Empty(t, results[1].Matches)

		// Third candidate should match
		assert.Equal(t, "candidate-bob", results[2].CandidateID)
		require.NotEmpty(t, results[2].Matches)
		assert.Equal(t, contact2.ID, results[2].Matches[0].Contact.ID)
	})

	t.Run("BelowThreshold_NoMatch", func(t *testing.T) {
		// Create a contact
		uniqueSuffix := uuid.New().String()[:8]
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Very Specific Name " + uniqueSuffix,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Search with a name that won't match (below 0.3 threshold)
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{
			{
				CandidateID:   "candidate-nomatch",
				CandidateName: "Completely Different",
			},
		}, 0.3, 5)

		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "candidate-nomatch", results[0].CandidateID)
		assert.Empty(t, results[0].Matches)
	})

	t.Run("LimitPerCandidate", func(t *testing.T) {
		// Create multiple similar contacts
		uniqueSuffix := uuid.New().String()[:8]
		var contactIDs []uuid.UUID

		for i := 0; i < 5; i++ {
			contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
				FullName: "Similar Person " + uniqueSuffix,
			})
			require.NoError(t, err)
			contactIDs = append(contactIDs, contact.ID)
		}
		defer func() {
			for _, id := range contactIDs {
				_ = contactRepo.HardDeleteContact(ctx, id)
			}
		}()

		// Search with limit of 3
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{
			{
				CandidateID:   "candidate-limit",
				CandidateName: "Similar Person " + uniqueSuffix,
			},
		}, 0.3, 3)

		require.NoError(t, err)
		require.Len(t, results, 1)
		// Should return at most 3 matches
		assert.LessOrEqual(t, len(results[0].Matches), 3)
	})

	t.Run("OrderPreserved", func(t *testing.T) {
		// Search with multiple candidates
		results, err := contactRepo.FindSimilarContactsBatch(ctx, []repository.BatchContactInput{
			{CandidateID: "first", CandidateName: "ZZZ No Match 1"},
			{CandidateID: "second", CandidateName: "ZZZ No Match 2"},
			{CandidateID: "third", CandidateName: "ZZZ No Match 3"},
		}, 0.3, 5)

		require.NoError(t, err)
		require.Len(t, results, 3)
		// Verify order is preserved
		assert.Equal(t, "first", results[0].CandidateID)
		assert.Equal(t, "second", results[1].CandidateID)
		assert.Equal(t, "third", results[2].CandidateID)
	})
}
