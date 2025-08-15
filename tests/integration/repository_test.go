package integration

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// Connect to database
	database, err := db.NewDatabase(ctx)
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
		// Get initial count
		initialCount, err := repo.CountContacts(ctx)
		require.NoError(t, err)

		// Create test contacts
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test User 1",
			Email:    stringPtr("test1@example.com"),
		})
		require.NoError(t, err)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test User 2",
			Email:    stringPtr("test2@example.com"),
		})
		require.NoError(t, err)

		// List contacts
		contacts, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Verify count increased
		finalCount, err := repo.CountContacts(ctx)
		require.NoError(t, err)
		assert.Equal(t, initialCount+2, finalCount)

		// Verify we have at least our test contacts
		assert.GreaterOrEqual(t, len(contacts), 2)

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
