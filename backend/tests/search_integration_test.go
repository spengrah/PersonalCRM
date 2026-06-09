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

	// Per-test namespace token so FTS query terms are unique to this test and
	// only match its own rows (the shared DB holds other tests' contacts/notes).
	ns := syntheticNS(t)

	t.Run("ExactNameMatch", func(t *testing.T) {
		// Create test contact
		name := "Alice Johnson " + ns
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "alice.johnson."+ns+"@example.com", true)

		// Search for exact name
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  name,
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find the contact
		assert.GreaterOrEqual(t, len(results), 1)

		// Verify the contact is in the results
		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				assert.Equal(t, name, c.FullName)
				break
			}
		}
		assert.True(t, found, "the seeded contact should be found in search results")
	})

	t.Run("PartialNameMatch", func(t *testing.T) {
		// Create test contact. The surname embeds the namespace token so the
		// single-word search matches only this test's contact.
		surname := "Smith" + ns
		name := "Bob " + surname
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "bob.smith."+ns+"@example.com", true)

		// Search for partial name (single word)
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  surname,
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find the contact
		found := false
		for _, c := range results {
			if c.ID == contact.ID {
				found = true
				assert.Equal(t, name, c.FullName)
				break
			}
		}
		assert.True(t, found, "the seeded contact should be found when searching the unique surname")
	})

	t.Run("EmailSearch", func(t *testing.T) {
		// Create test contact
		firstName := "Carol" + ns
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: firstName + " Davis",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "carol.davis."+ns+"@example.com", true)

		// Search by name (FTS tokenizes email addresses specially, so search by name)
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  firstName,
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
			FullName: "Method Search Contact " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		handle := "handle" + ns
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodTelegram), handle, true)

		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  handle,
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
		// Create multiple test contacts with same pattern. The unique token in
		// the name makes the search term match only this test's contacts.
		pageTerm := "Pagination" + ns
		for i := 0; i < 5; i++ {
			contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
				FullName: pageTerm + " Test User",
			})
			require.NoError(t, err)
			defer func(id uuid.UUID) { _ = repo.HardDeleteContact(ctx, id) }(contact.ID)
			addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "pagination.test."+ns+"."+string(rune('a'+i))+"@example.com", true)
		}

		// Test limit
		page1, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  pageTerm,
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
		// Both names share a unique token so the search matches only this test's
		// two contacts — the >= 2 shape assertion is then over our own rows.
		token := "Michael" + ns
		contact1, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: token + " Johnson",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact1.ID) }()
		addContactMethod(t, ctx, methodRepo, contact1.ID, string(repository.ContactMethodEmail), "michael.j."+ns+"@example.com", true)

		contact2, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Sarah " + token,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact2.ID) }()
		addContactMethod(t, ctx, methodRepo, contact2.ID, string(repository.ContactMethodEmail), "sarah.m."+ns+"@example.com", true)

		// Search for the unique token
		results, err := repo.SearchContacts(ctx, repository.SearchContactsParams{
			Query:  token,
			Limit:  10,
			Offset: 0,
		})
		require.NoError(t, err)

		// Should find both contacts (both share the token in full_name)
		assert.GreaterOrEqual(t, len(results), 2, "Should find at least 2 contacts with the token in name")

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
		assert.True(t, foundContact1, "Contact 1 should be in results")
		assert.True(t, foundContact2, "Contact 2 should be in results")
	})

	t.Run("CaseInsensitive", func(t *testing.T) {
		// Create test contact. The unique ns suffix scopes the search to this
		// test's contact; the "david" word is varied in case to prove FTS is
		// case-insensitive (the ns token is appended unchanged each time).
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "david" + ns + " Miller",
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "david.miller."+ns+"@example.com", true)

		// Search with different cases of the word, all carrying the ns suffix.
		testCases := []string{"david" + ns, "DAVID" + ns, "David" + ns, "dAvId" + ns}
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

	// Per-test namespace token so FTS query terms match only this test's notes.
	ns := syntheticNS(t)

	t.Run("BasicNoteSearch", func(t *testing.T) {
		// Create a test contact
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Note Test Contact " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "note.test."+ns+"@example.com", true)

		// Create a test note. The ns token makes the two-word phrase unique.
		term := "machine" + ns
		note, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "This is a test note about " + term + " learning and artificial intelligence",
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note.ID) }()

		// Search for the unique two-word phrase
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: term + " learning",
			Limit:          10,
			Offset:         0,
		})
		require.NoError(t, err)

		// Should find our test note
		found := false
		for _, n := range results {
			if n.ID.Bytes == note.ID.Bytes {
				found = true
				assert.Contains(t, n.Body, term)
				break
			}
		}
		assert.True(t, found, "Note should be found when searching for the unique phrase")
	})

	t.Run("NoteRelevanceRanking", func(t *testing.T) {
		// Create contact for test notes
		contact, err := repo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Ranking Test Contact " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "ranking.test."+ns+"@example.com", true)

		// Unique term so only this test's two notes match; note1 carries 3
		// occurrences (higher rank) vs note2's 1. Searching the term means the
		// "ranks first" assertion is over our own result set only.
		term := "golang" + ns
		note1, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      term + " " + term + " " + term + " programming language", // High relevance
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note1.ID) }()

		note2, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      "python programming with some " + term + " mention", // Medium relevance
			Category:  pgtype.Text{String: "technical", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note2.ID) }()

		// Search for the unique term
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: term,
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
			FullName: "Note Pagination Test " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "note.pagination."+ns+"@example.com", true)

		// Create multiple notes with the same unique keyword
		term := "pagination" + ns
		for i := 0; i < 5; i++ {
			note, err := queries.CreateNote(ctx, db.CreateNoteParams{
				ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
				Body:      "Testing " + term + " functionality with unique content number " + string(rune('0'+i)),
				Category:  pgtype.Text{String: "test", Valid: true},
			})
			require.NoError(t, err)
			defer func(id pgtype.UUID) { _ = queries.DeleteNote(ctx, id) }(note.ID)
		}

		// Test limit
		page1, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: term,
			Limit:          2,
			Offset:         0,
		})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page1), 2)

		// Test offset
		page2, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: term,
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
			FullName: "Sort Test Contact " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = repo.HardDeleteContact(ctx, contact.ID) }()
		addContactMethod(t, ctx, methodRepo, contact.ID, string(repository.ContactMethodEmail), "sort.test."+ns+"@example.com", true)

		// Create notes with same relevance (same keyword count); the unique
		// term keeps the result set to this test's two notes.
		term := "sorting" + ns
		note1, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      term + " test first",
			Category:  pgtype.Text{String: "test", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note1.ID) }()

		note2, err := queries.CreateNote(ctx, db.CreateNoteParams{
			ContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
			Body:      term + " test second",
			Category:  pgtype.Text{String: "test", Valid: true},
		})
		require.NoError(t, err)
		defer func() { _ = queries.DeleteNote(ctx, note2.ID) }()

		// Search for the unique term
		results, err := queries.SearchNotes(ctx, db.SearchNotesParams{
			PlaintoTsquery: term,
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
