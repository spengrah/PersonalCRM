package api_test

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestDB sets up a test database connection
func setupTestDB(t *testing.T) (*db.Database, func()) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	return database, func() { database.Close() }
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}

func TestContactMerge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Create repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)

	// Create service
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)

	t.Run("GetMergePreview_BasicCounts", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Target",
			Location: stringPtr("New York"),
			Cadence:  stringPtr("monthly"),
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Source",
			Location: stringPtr("Los Angeles"),
			Cadence:  stringPtr("weekly"),
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) }()

		// Create notepad note for source
		_, err = noteRepo.CreateNotepad(ctx, source.ID, "Source notes")
		require.NoError(t, err)

		// Add methods to source
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "email",
			Value:     "source@example.com",
		})
		require.NoError(t, err)

		// Get merge preview
		preview, err := contactService.GetMergePreview(ctx, source.ID, target.ID)
		require.NoError(t, err)

		assert.Equal(t, target.ID, preview.TargetContact.ID)
		assert.Equal(t, source.ID, preview.SourceContact.ID)
		assert.Equal(t, int64(1), preview.MethodsToTransfer)
		assert.Equal(t, int64(0), preview.DuplicateMethods)
		assert.Equal(t, int64(1), preview.NotesToTransfer) // notepad note created above
	})

	t.Run("GetMergePreview_WithDuplicateMethods", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Target Dupe",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Source Dupe",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) }()

		// Add same email to both
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: target.ID,
			Type:      "email",
			Value:     "shared@example.com",
		})
		require.NoError(t, err)

		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "email",
			Value:     "shared@example.com",
		})
		require.NoError(t, err)

		// Add unique email to source
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "email",
			Value:     "unique@example.com",
		})
		require.NoError(t, err)

		// Get merge preview
		preview, err := contactService.GetMergePreview(ctx, source.ID, target.ID)
		require.NoError(t, err)

		assert.Equal(t, int64(1), preview.MethodsToTransfer) // unique@example.com
		assert.Equal(t, int64(1), preview.DuplicateMethods)  // shared@example.com
	})

	t.Run("MergeContacts_TransfersAllData", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Target Full",
			Cadence:  stringPtr("monthly"),
		})
		require.NoError(t, err)

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Merge Source Full",
			Location: stringPtr("Boston"),
		})
		require.NoError(t, err)

		// Create notepad for source
		_, err = noteRepo.CreateNotepad(ctx, source.ID, "Important notes")
		require.NoError(t, err)

		// Add method to source
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "phone",
			Value:     "+1-555-0100",
		})
		require.NoError(t, err)

		// Execute merge
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
			FieldSelections: service.MergeFieldSelections{
				Location: "source",
			},
		})
		require.NoError(t, err)

		// Verify merged contact
		assert.Equal(t, target.ID, merged.ID)
		assert.Equal(t, "Merge Target Full", merged.FullName) // target name preserved
		assert.Equal(t, "Boston", *merged.Location)           // source location selected

		// Verify notepad was transferred
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		require.NotNil(t, mergedNotepad)
		assert.Contains(t, mergedNotepad.Body, "Important notes")

		// Verify methods transferred
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, merged.ID)
		require.NoError(t, err)
		assert.Len(t, methods, 1)
		assert.Equal(t, "+1-555-0100", methods[0].Value)

		// Verify source is soft-deleted
		_, err = contactRepo.GetContact(ctx, source.ID)
		assert.Error(t, err)

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})

	t.Run("MergeContacts_NameOverride", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Target Name",
		})
		require.NoError(t, err)

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Source Name",
		})
		require.NoError(t, err)

		// Execute merge with name override
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
			NewName:         stringPtr("Custom Merged Name"),
		})
		require.NoError(t, err)

		assert.Equal(t, "Custom Merged Name", merged.FullName)

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})

	t.Run("MergeContacts_DeduplicatesMethods", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Dedup Target",
		})
		require.NoError(t, err)

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Dedup Source",
		})
		require.NoError(t, err)

		// Add same phone to both (different formatting)
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: target.ID,
			Type:      "phone",
			Value:     "555-123-4567",
			IsPrimary: true,
		})
		require.NoError(t, err)

		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "phone",
			Value:     "(555) 123-4567", // Same normalized value
		})
		require.NoError(t, err)

		// Execute merge
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
		})
		require.NoError(t, err)

		// Verify only one phone number exists
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, merged.ID)
		require.NoError(t, err)
		assert.Len(t, methods, 1)
		assert.Equal(t, "phone", methods[0].Type)
		assert.True(t, methods[0].IsPrimary) // Target's primary status preserved

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})

	t.Run("MergeContacts_CombinesNotes", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Notes Target",
		})
		require.NoError(t, err)

		// Create notepad for target
		_, err = noteRepo.CreateNotepad(ctx, target.ID, "Target notes here")
		require.NoError(t, err)

		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Notes Source",
		})
		require.NoError(t, err)

		// Create notepad for source
		_, err = noteRepo.CreateNotepad(ctx, source.ID, "Source notes here")
		require.NoError(t, err)

		// Execute merge
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
		})
		require.NoError(t, err)

		// Verify notes are combined
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		require.NotNil(t, mergedNotepad)
		assert.Contains(t, mergedNotepad.Body, "Target notes here")
		assert.Contains(t, mergedNotepad.Body, "Source notes here")

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})

	t.Run("MergeContacts_InvalidTargetID", func(t *testing.T) {
		// Create source contact
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Invalid Test Source",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) }()

		// Try to merge with non-existent target
		_, err = contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: uuid.New(),
			SourceContactID: source.ID,
		})
		assert.Error(t, err)
	})

	t.Run("MergeContacts_InvalidSourceID", func(t *testing.T) {
		// Create target contact
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Invalid Test Target",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Try to merge with non-existent source
		_, err = contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: uuid.New(),
		})
		assert.Error(t, err)
	})

	t.Run("MergeContacts_SameSourceAndTarget", func(t *testing.T) {
		// Create a contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Self Merge Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Try to merge with itself
		_, err = contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: contact.ID,
			SourceContactID: contact.ID,
		})
		assert.Error(t, err)
	})

	t.Run("MergeContacts_FieldSelectionsPreserveCadence", func(t *testing.T) {
		// Create target contact with weekly cadence
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Cadence Target",
			Cadence:  stringPtr("weekly"),
		})
		require.NoError(t, err)

		// Create source contact with quarterly cadence
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Cadence Source",
			Cadence:  stringPtr("quarterly"),
		})
		require.NoError(t, err)

		// Merge selecting source cadence
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
			FieldSelections: service.MergeFieldSelections{
				Cadence: "source",
			},
		})
		require.NoError(t, err)

		assert.Equal(t, "quarterly", *merged.Cadence)

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})

	t.Run("MergeContacts_FieldSelectionsPreserveTarget", func(t *testing.T) {
		// Create target contact with monthly cadence and NYC location
		target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Keep Target",
			Cadence:  stringPtr("monthly"),
			Location: stringPtr("New York"),
		})
		require.NoError(t, err)

		// Create source contact with different values
		source, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Ignore Source",
			Cadence:  stringPtr("annual"),
			Location: stringPtr("Miami"),
		})
		require.NoError(t, err)

		// Merge keeping target values (default)
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
			FieldSelections: service.MergeFieldSelections{
				Cadence:  "target",
				Location: "target",
			},
		})
		require.NoError(t, err)

		assert.Equal(t, "monthly", *merged.Cadence)
		assert.Equal(t, "New York", *merged.Location)

		// Cleanup
		_ = contactRepo.HardDeleteContact(ctx, target.ID)
	})
}
