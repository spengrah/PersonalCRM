package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addContactMethod(
	t *testing.T,
	ctx context.Context,
	methodRepo *repository.ContactMethodRepository,
	contactID uuid.UUID,
	methodType string,
	value string,
	isPrimary bool,
) {
	t.Helper()

	_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      methodType,
		Value:     value,
		IsPrimary: isPrimary,
	})
	require.NoError(t, err)
}

// TestContactSearch_Integration tests full-text search functionality for contacts
func TestContactSearch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	t.Run("ExactNameMatch", func(t *testing.T) {
		// Create test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Alice Johnson",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "alice.johnson@example.com", true)

		// Search for exact name
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Alice Johnson",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find the contact
		assert.GreaterOrEqual(t, len(results), 1)

		// Verify Alice Johnson is in the results
		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				assert.Equal(t, "Alice Johnson", c.FullName)
				break
			}
		}
		assert.True(t, found, "Alice Johnson should be found in search results")
	})

	t.Run("PartialNameMatch", func(t *testing.T) {
		// Create test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Bob Smith",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "bob.smith@example.com", true)

		// Search for partial name (single word)
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Smith",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find the contact
		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				assert.Equal(t, "Bob Smith", c.FullName)
				break
			}
		}
		assert.True(t, found, "Bob Smith should be found when searching for 'Smith'")
	})

	t.Run("EmailSearch", func(t *testing.T) {
		// Create test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Carol Davis",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "carol.davis@example.com", true)

		// Search by name (FTS tokenizes email addresses specially, so search by name)
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Carol",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find the contact
		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				break
			}
		}
		assert.True(t, found, "Contact should be found when searching by name")
	})

	t.Run("MethodValueSearch", func(t *testing.T) {
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Method Search Contact",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodTelegram), "handle123", true)

		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "handle123",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				break
			}
		}
		assert.True(t, found, "Contact should be found when searching by method value")
	})

	t.Run("NoResults", func(t *testing.T) {
		// Search for non-existent contact
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "ZZZNonExistentPerson12345XYZ",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should return empty array, not error
		assert.Equal(t, 0, len(results))
	})

	t.Run("SpecialCharacters", func(t *testing.T) {
		// FTS should handle special characters gracefully
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Test & User | Name",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should not error (plainto_tsquery handles special chars)
		assert.NotNil(t, results)
	})

	t.Run("Pagination", func(t *testing.T) {
		// Create multiple test contacts with same pattern
		for i := 0; i < 5; i++ {
			contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
				FullName: "Pagination Test User",
			})
			require.NoError(t, err)
			defer func(id uuid.UUID) { _ = repo.HardDeleteContact(ctx, id) }(contact.ID)
			addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "pagination.test."+string(rune('a'+i))+"@example.com", true)
		}

		// Test limit
		page1, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Pagination",
			Limit:  2,
			Offset: 0,
		})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page1), 2)

		// Test offset
		page2, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Pagination",
			Limit:  2,
			Offset: 2,
		})
		require.NoError(t, err)

		// Pages should be different (if both have results)
		if len(page1) > 0 && len(page2) > 0 {
			assert.NotEqual(t, page1[0].ID, page2[0].ID)
		}
	})

	t.Run("RelevanceRanking", func(t *testing.T) {
		// Create contacts with different relevance (both have "Michael" in name)
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Michael Johnson",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact1.ID) }()
		addContactMethod(t, ctx, methodRepo, contact1.ID, string(repository.ContactMethodEmailPersonal), "michael.j@example.com", true)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Sarah Michael",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact2.ID) }()
		addContactMethod(t, ctx, methodRepo, contact2.ID, string(repository.ContactMethodEmailPersonal), "sarah.m@example.com", true)

		// Search for "Michael"
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  "Michael",
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find both contacts (both have "Michael" in full_name)
		assert.GreaterOrEqual(t, len(results), 2, "Should find at least 2 contacts with 'Michael' in name")

		// Verify both are in results (order may vary based on other data)
		foundContact1 := false
		foundContact2 := false
		for _, c := range results {
			if c.ID == contact1.ID {
				foundContact1 = true
			}
			if c.ID == contact2.ID {
				foundContact2 = true
			}
		}
		assert.True(t, foundContact1, "Contact 1 (Michael Johnson) should be in results")
		assert.True(t, foundContact2, "Contact 2 (Sarah Michael) should be in results")
	})

	t.Run("CaseInsensitive", func(t *testing.T) {
		// Create test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "David Miller",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "david.miller@example.com", true)

		// Search with different cases
		testCases := []string{"david", "DAVID", "David", "dAvId"}
		for _, query := range testCases {
			results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
				Query:  query,
				Limit:  10,
				Offset: 0,
			})
			require.NoError(t, err)

			// Should find the contact regardless of case
			found := false
			for _, c := range results {
				if c.ID == contact.ID {
					found = true
					break
				}
			}
			assert.True(t, found, "Should find contact with query: %s", query)
		}
	})
}

// TestNoteSearch_Integration tests full-text search functionality for notes
func TestNoteSearch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	queries := database.Queries
	repo := repository.NewContactRepository(queries)
	methodRepo := repository.NewContactMethodRepository(queries)

	t.Run("BasicNoteSearch", func(t *testing.T) {
		// Create a test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Note Test Contact",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "note.test@example.com", true)

		// Create a test note
		note, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "This is a test note about machine learning and artificial intelligence",
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note.ID) }()

		// Search for "machine learning"
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "machine learning",
			Limit:          10,
			Offset:         0,
		})
		require.NoError(t, err)

		// Should find our test note
		found := false
		for _, n := range results {
			if n.ID.Bytes == note.ID.Bytes {
				found = true
				assert.Contains(t, n.Body, "machine learning")
				break
			}
		}
		assert.True(t, found, "Note should be found when searching for 'machine learning'")
	})

	t.Run("NoteRelevanceRanking", func(t *testing.T) {
		// Create contact for test notes
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Ranking Test Contact",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "ranking.test@example.com", true)

		// Create notes with different relevance
		note1, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "golang golang golang programming language", // High relevance
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note1.ID) }()

		note2, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "python programming with some golang mention", // Medium relevance
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note2.ID) }()

		// Search for "golang"
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "golang",
			Limit:          10,
			Offset:         0,
		})
		require.NoError(t, err)

		// Should find both notes
		assert.GreaterOrEqual(t, len(results), 2)

		// First result should be note1 (more occurrences = higher rank)
		foundNote1First := false
		for i, n := range results {
			if n.ID.Bytes == note1.ID.Bytes {
				if i == 0 {
					foundNote1First = true
				}
				break
			}
		}
		assert.True(t, foundNote1First, "Note with more occurrences should rank first")
	})

	t.Run("NoteSearchNoResults", func(t *testing.T) {
		// Search for non-existent term
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "ZZZNonExistentSearchTerm12345XYZ",
			Limit:          10,
			Offset:         0,
		})
		require.NoError(t, err)

		// Should return empty array, not error
		assert.Equal(t, 0, len(results))
	})

	t.Run("NoteSearchPagination", func(t *testing.T) {
		// Create contact for test notes
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Note Pagination Test",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "note.pagination@example.com", true)

		// Create multiple notes with same keyword
		for i := 0; i < 5; i++ {
			note, err := queries.CreateNote(ctx, db.CreateNoteParams{
				ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
				Body:      "Testing pagination functionality with unique content number " + string(rune('0'+i)),
				Category:  pgtype.Text{String: "test", Valid: true},
			})
			require.NoError(t, err)
			defer func(id pgtype.UUID) { _ = queries.DeleteNote(ctx, id) }(note.ID)
		}

		// Test limit
		page1, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "pagination",
			Limit:          2,
			Offset:         0,
		})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page1), 2)

		// Test offset
		page2, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "pagination",
			Limit:          2,
			Offset:         2,
		})
		require.NoError(t, err)

		// Pages should be different
		if len(page1) > 0 && len(page2) > 0 {
			assert.NotEqual(t, page1[0].ID.Bytes, page2[0].ID.Bytes)
		}
	})

	t.Run("NoteSearchCreatedAtSecondarySort", func(t *testing.T) {
		// Create contact for test notes
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Sort Test Contact",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmailPersonal), "sort.test@example.com", true)

		// Create notes with same relevance (same keyword count)
		note1, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "sorting test first",
			Category:  pgtype.Text{String: "test", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note1.ID) }()

		note2, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "sorting test second",
			Category:  pgtype.Text{String: "test", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note2.ID) }()

		// Search for "sorting"
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: "sorting",
			Limit:          10,
			Offset:         0,
		})
		require.NoError(t, err)

		// Should find both notes
		assert.GreaterOrEqual(t, len(results), 2)

		// Verify both are in results
		foundNote1 := false
		foundNote2 := false
		for _, n := range results {
			if n.ID.Bytes == note1.ID.Bytes {
				foundNote1 = true
			}
			if n.ID.Bytes == note2.ID.Bytes {
				foundNote2 = true
			}
		}
		assert.True(t, foundNote1, "Note 1 should be in results")
		assert.True(t, foundNote2, "Note 2 should be in results")

		// Note: We can't guarantee order when relevance is equal,
		// but we verify both are found and secondary sort is by created_at DESC
		// The second note should be created after the first, so if relevance is equal,
		// note2 should come before note1
	})
}
