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
			Email:    stringPtr("integration@example.com"),
			Phone:    stringPtr("+1234567890"),
			Location: stringPtr("Test City"),
			Cadence:  stringPtr("monthly"),
		}

		createdContact, err := repo.CreateContact(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, createdContact)

		// Verify the created contact
		assert.Equal(t, "Integration Test User", createdContact.FullName)
		assert.Equal(t, "integration@example.com", *createdContact.Email)
		assert.Equal(t, "+1234567890", *createdContact.Phone)
		assert.Equal(t, "Test City", *createdContact.Location)
		assert.Equal(t, "monthly", *createdContact.Cadence)
		assert.NotEqual(t, uuid.Nil, createdContact.ID)

		// Get the contact by ID
		foundContact, err := repo.GetContact(ctx, createdContact.ID)
		require.NoError(t, err)
		require.NotNil(t, foundContact)

		assert.Equal(t, createdContact.ID, foundContact.ID)
		assert.Equal(t, createdContact.FullName, foundContact.FullName)
		assert.Equal(t, *createdContact.Email, *foundContact.Email)

		// Clean up - delete the test contact
		err = repo.HardDeleteContact(ctx, createdContact.ID)
		require.NoError(t, err)
	})

	t.Run("ListContacts", func(t *testing.T) {
		// Create test contacts with unique emails to avoid conflicts
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Integration List Test User 1",
			Email:    stringPtr("integration_list_test1@example.com"),
		})
		require.NoError(t, err)
		require.NotNil(t, contact1)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Integration List Test User 2",
			Email:    stringPtr("integration_list_test2@example.com"),
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

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
