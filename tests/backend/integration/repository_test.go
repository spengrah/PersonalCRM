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

// TestContactRepository_FindSimilarContacts tests fuzzy matching with pg_trgm
func TestContactRepository_FindSimilarContacts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	database, err := db.NewDatabase(ctx)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	t.Run("FindsSimilarContactsAboveThreshold", func(t *testing.T) {
		// Create test contacts
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "John Smith",
		})
		require.NoError(t, err)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "John Smyth", // Similar but different spelling
		})
		require.NoError(t, err)

		contact3, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Jane Doe", // Not similar
		})
		require.NoError(t, err)

		// Search for similar names
		matches, err := repo.FindSimilarContacts(ctx, "John Smith", 0.3, 10)
		require.NoError(t, err)

		// Should find John Smith and John Smyth but not Jane Doe
		assert.GreaterOrEqual(t, len(matches), 2)

		// Verify matches are sorted by similarity descending
		if len(matches) >= 2 {
			assert.GreaterOrEqual(t, matches[0].Similarity, matches[1].Similarity)
		}

		// Find our specific contacts
		foundContact1 := false
		foundContact2 := false
		for _, match := range matches {
			if match.Contact.ID == contact1.ID {
				foundContact1 = true
				assert.Equal(t, "John Smith", match.Contact.FullName)
			}
			if match.Contact.ID == contact2.ID {
				foundContact2 = true
				assert.Equal(t, "John Smyth", match.Contact.FullName)
			}
		}
		assert.True(t, foundContact1, "Should find exact match John Smith")
		assert.True(t, foundContact2, "Should find similar match John Smyth")

		// Clean up
		err = repo.HardDeleteContact(ctx, contact1.ID)
		require.NoError(t, err)
		err = repo.HardDeleteContact(ctx, contact2.ID)
		require.NoError(t, err)
		err = repo.HardDeleteContact(ctx, contact3.ID)
		require.NoError(t, err)
	})

	t.Run("ExcludesBelowThreshold", func(t *testing.T) {
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Alice Johnson",
		})
		require.NoError(t, err)

		// Search with high threshold for a very different name
		matches, err := repo.FindSimilarContacts(ctx, "Bob Williams", 0.8, 10)
		require.NoError(t, err)

		// Should not find Alice Johnson with high threshold
		for _, match := range matches {
			if match.Contact.ID == contact.ID {
				t.Error("Should not find Alice Johnson when searching for Bob Williams with high threshold")
			}
		}

		// Clean up
		err = repo.HardDeleteContact(ctx, contact.ID)
		require.NoError(t, err)
	})

	t.Run("SortsBySimilarityDescending", func(t *testing.T) {
		// Create contacts with varying similarity to "John Smith"
		exactMatch, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "John Smith",
		})
		require.NoError(t, err)

		similarMatch, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "John Smyth",
		})
		require.NoError(t, err)

		lessMatch, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Jon Smith",
		})
		require.NoError(t, err)

		matches, err := repo.FindSimilarContacts(ctx, "John Smith", 0.3, 10)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(matches), 3)

		// Verify sorted descending
		for i := 0; i < len(matches)-1; i++ {
			assert.GreaterOrEqual(t, matches[i].Similarity, matches[i+1].Similarity,
				"Results should be sorted by similarity in descending order")
		}

		// Clean up
		err = repo.HardDeleteContact(ctx, exactMatch.ID)
		require.NoError(t, err)
		err = repo.HardDeleteContact(ctx, similarMatch.ID)
		require.NoError(t, err)
		err = repo.HardDeleteContact(ctx, lessMatch.ID)
		require.NoError(t, err)
	})

	t.Run("ParsesContactMethodsFromJSON", func(t *testing.T) {
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Sarah Johnson",
		})
		require.NoError(t, err)

		// Add contact methods
		_, err = methodRepo.Create(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      "email_personal",
			Value:     "sarah@example.com",
		})
		require.NoError(t, err)

		_, err = methodRepo.Create(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      "phone",
			Value:     "555-1234",
		})
		require.NoError(t, err)

		matches, err := repo.FindSimilarContacts(ctx, "Sarah Johnson", 0.3, 10)
		require.NoError(t, err)

		// Find our contact in matches
		var foundMatch *repository.ContactMatch
		for _, match := range matches {
			if match.Contact.ID == contact.ID {
				foundMatch = &match
				break
			}
		}
		require.NotNil(t, foundMatch, "Should find Sarah Johnson")

		// Verify contact methods were parsed
		assert.Len(t, foundMatch.Contact.Methods, 2)

		hasEmail := false
		hasPhone := false
		for _, method := range foundMatch.Contact.Methods {
			if method.Type == "email_personal" && method.Value == "sarah@example.com" {
				hasEmail = true
			}
			if method.Type == "phone" && method.Value == "555-1234" {
				hasPhone = true
			}
		}
		assert.True(t, hasEmail, "Should have email method")
		assert.True(t, hasPhone, "Should have phone method")

		// Clean up
		err = repo.HardDeleteContact(ctx, contact.ID)
		require.NoError(t, err)
	})

	t.Run("RespectsLimitParameter", func(t *testing.T) {
		// Create multiple similar contacts
		var contactIDs []uuid.UUID
		for i := 0; i < 5; i++ {
			contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
				FullName: "John Smith",
			})
			require.NoError(t, err)
			contactIDs = append(contactIDs, contact.ID)
		}

		// Query with limit of 3
		matches, err := repo.FindSimilarContacts(ctx, "John Smith", 0.3, 3)
		require.NoError(t, err)

		assert.LessOrEqual(t, len(matches), 3, "Should respect limit parameter")

		// Clean up
		for _, id := range contactIDs {
			err = repo.HardDeleteContact(ctx, id)
			require.NoError(t, err)
		}
	})

	t.Run("HandlesNoMatches", func(t *testing.T) {
		// Search for name that doesn't exist
		matches, err := repo.FindSimilarContacts(ctx, "Zyzzyva Xylophone", 0.3, 10)
		require.NoError(t, err)
		assert.NotNil(t, matches, "Should return empty slice, not nil")
	})

	t.Run("IgnoresDeletedContacts", func(t *testing.T) {
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Michael Johnson",
		})
		require.NoError(t, err)

		// Soft delete the contact
		err = repo.SoftDeleteContact(ctx, contact.ID)
		require.NoError(t, err)

		// Search should not find soft-deleted contact
		matches, err := repo.FindSimilarContacts(ctx, "Michael Johnson", 0.3, 10)
		require.NoError(t, err)

		for _, match := range matches {
			if match.Contact.ID == contact.ID {
				t.Error("Should not find soft-deleted contacts")
			}
		}

		// Clean up
		err = repo.HardDeleteContact(ctx, contact.ID)
		require.NoError(t, err)
	})
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
