package api

import (
	"context"
	"os"
	"testing"
	"time"

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

// seedContactForMerge creates a contact through the ContactService write path so
// location/birthday/how_met are persisted as assertions and reflected in the
// cache columns (the repository's CreateContact no longer writes those columns
// post-cutover). Matches the repo's 2-value return shape so the merge fixtures
// read unchanged.
func seedContactForMerge(ctx context.Context, svc *service.ContactService, req repository.CreateContactRequest) (*repository.Contact, error) {
	c, _, err := svc.CreateContact(ctx, req, nil)
	return c, err
}

func TestContactMerge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Per-test namespace so fixed contact names cannot collide with concurrent
	// siblings under fuzzy-match (trigram) pollution.
	ns := uuid.New().String()[:8]

	// Create repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)

	// Create service
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil, cadenceUpdater, assertSvc, cache, nil)

	t.Run("GetMergePreview_BasicCounts", func(t *testing.T) {
		// Create target contact
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Target " + ns,
			Location: stringPtr("New York"),
			Cadence:  stringPtr("monthly"),
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Source " + ns,
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
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Target Dupe " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Source Dupe " + ns,
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
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Target Full " + ns,
			Cadence:  stringPtr("monthly"),
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Source Full " + ns,
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
		assert.Equal(t, "Merge Target Full "+ns, merged.FullName) // target name preserved
		assert.Equal(t, "Boston", *merged.Location)               // source location selected

		// Verify notepad was transferred verbatim (no separator artifacts)
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		require.NotNil(t, mergedNotepad)
		assert.Equal(t, "Important notes", mergedNotepad.Body)

		// Verify methods transferred
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, merged.ID)
		require.NoError(t, err)
		assert.Len(t, methods, 1)
		assert.Equal(t, "+1-555-0100", methods[0].Value)

		// Verify source is soft-deleted
		_, err = contactRepo.GetContact(ctx, source.ID)
		assert.Error(t, err)

	})

	t.Run("MergeContacts_NameOverride", func(t *testing.T) {
		// Create target contact
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Target Name " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Source Name " + ns,
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

	})

	t.Run("MergeContacts_DeduplicatesMethods", func(t *testing.T) {
		// Create target contact
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Dedup Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Dedup Source " + ns,
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

	})

	t.Run("MergeContacts_CrossTypeDualPrimaries", func(t *testing.T) {
		// CON-049: source and target each hold a primary of a DIFFERENT method
		// type. The one-primary rule is per contact (idx_contact_method_primary
		// on (contact_id) WHERE is_primary), so the source's primary must be
		// demoted regardless of type or the transfer violates the index.
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "CrossType Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "CrossType Source " + ns,
		})
		require.NoError(t, err)

		// Target: primary email. Source: primary phone (different type).
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: target.ID,
			Type:      "email",
			Value:     "crosstype-target@example.com",
			IsPrimary: true,
		})
		require.NoError(t, err)

		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: source.ID,
			Type:      "phone",
			Value:     "+1-555-0177",
			IsPrimary: true,
		})
		require.NoError(t, err)

		// The merge must succeed, not fail on the unique partial index
		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
		})
		require.NoError(t, err)

		// Target's existing primary is preserved; the source's arrives demoted
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, merged.ID)
		require.NoError(t, err)
		require.Len(t, methods, 2)
		byType := map[string]bool{}
		for _, m := range methods {
			byType[m.Type] = m.IsPrimary
		}
		assert.True(t, byType["email"], "target's primary email preserved")
		assert.False(t, byType["phone"], "source's phone transferred demoted")
	})

	t.Run("MergeContacts_CombinesNotes", func(t *testing.T) {
		// Create target contact
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Notes Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create notepad for target
		_, err = noteRepo.CreateNotepad(ctx, target.ID, "Target notes here")
		require.NoError(t, err)

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Notes Source " + ns,
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

		// Verify notes are combined: target first, then separator, then source
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		require.NotNil(t, mergedNotepad)
		assert.Equal(t, "Target notes here\n\n---\n\n"+"Source notes here", mergedNotepad.Body)

	})

	t.Run("MergeContacts_NoNotepads_CreatesNone", func(t *testing.T) {
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "No Notepad Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "No Notepad Source " + ns,
		})
		require.NoError(t, err)

		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
		})
		require.NoError(t, err)

		// Neither contact had a notepad, so the merge must not mint one
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		assert.Nil(t, mergedNotepad)
	})

	t.Run("MergeContacts_TargetOnlyNotepad_PreservedVerbatim", func(t *testing.T) {
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Target Notepad Only Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		_, err = noteRepo.CreateNotepad(ctx, target.ID, "Target only notes")
		require.NoError(t, err)

		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Target Notepad Only Source " + ns,
		})
		require.NoError(t, err)

		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
		})
		require.NoError(t, err)

		// Source had no notepad: target's notepad must survive unchanged,
		// with no separator or empty content appended
		mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
		require.NoError(t, err)
		require.NotNil(t, mergedNotepad)
		assert.Equal(t, "Target only notes", mergedNotepad.Body)
	})

	t.Run("MergeContacts_EmptyBodySourceNotepad", func(t *testing.T) {
		// The pre-fix merge bug minted empty-body notepad rows onto merge
		// winners; when such a contact is later merged as a SOURCE, its empty
		// notepad must contribute nothing — no note minted on a bare target,
		// no separator appended to a target with content, and no unique-index
		// collision from TransferNotes repointing the leftover row.
		t.Run("BareTarget", func(t *testing.T) {
			target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
				FullName: "EmptyNotepad Bare Target " + ns,
			})
			require.NoError(t, err)
			defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

			source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
				FullName: "EmptyNotepad Bare Source " + ns,
			})
			require.NoError(t, err)

			_, err = noteRepo.CreateNotepad(ctx, source.ID, "")
			require.NoError(t, err)

			merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
				TargetContactID: target.ID,
				SourceContactID: source.ID,
			})
			require.NoError(t, err)

			mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
			require.NoError(t, err)
			assert.Nil(t, mergedNotepad)
		})

		t.Run("TargetWithContent", func(t *testing.T) {
			target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
				FullName: "EmptyNotepad Content Target " + ns,
			})
			require.NoError(t, err)
			defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

			_, err = noteRepo.CreateNotepad(ctx, target.ID, "Keep me")
			require.NoError(t, err)

			source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
				FullName: "EmptyNotepad Content Source " + ns,
			})
			require.NoError(t, err)

			_, err = noteRepo.CreateNotepad(ctx, source.ID, "")
			require.NoError(t, err)

			merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
				TargetContactID: target.ID,
				SourceContactID: source.ID,
			})
			require.NoError(t, err)

			mergedNotepad, err := noteRepo.GetContactNotepad(ctx, merged.ID)
			require.NoError(t, err)
			require.NotNil(t, mergedNotepad)
			assert.Equal(t, "Keep me", mergedNotepad.Body)
		})
	})

	t.Run("MergeContacts_InvalidTargetID", func(t *testing.T) {
		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Invalid Test Source " + ns,
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
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Invalid Test Target " + ns,
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
		contact, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Self Merge Test " + ns,
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
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Cadence Target " + ns,
			Cadence:  stringPtr("weekly"),
		})
		require.NoError(t, err)

		// Create source contact with quarterly cadence
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Cadence Source " + ns,
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

	})

	t.Run("MergeContacts_FieldSelectionsPreserveTarget", func(t *testing.T) {
		// Create target contact with monthly cadence and NYC location
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Keep Target " + ns,
			Cadence:  stringPtr("monthly"),
			Location: stringPtr("New York"),
		})
		require.NoError(t, err)

		// Create source contact with different values
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Ignore Source " + ns,
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

	})

	t.Run("MergeContacts_ReturnsFreshStructAfterBulkApply", func(t *testing.T) {
		// Stale-struct guard: MergeContacts runs two UPDATEs on the
		// target row (profile-only UpdateContact, then BulkApply for
		// cadence columns). Both bump updated_at; both can move
		// cadence timestamps. The returned struct must reflect the
		// post-BulkApply committed state, not the stale post-profile
		// value.
		initialLastContacted := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName:      "Refetch Target " + ns,
			Cadence:       stringPtr("weekly"),
			LastContacted: &initialLastContacted,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Source has a NEWER last_contacted. BulkApply's forward-max
		// should advance target.last_contacted — a value the profile-
		// only UpdateContact cannot write — so the returned struct
		// must show the new timestamp.
		sourceLastContacted := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName:      "Refetch Source " + ns,
			Cadence:       stringPtr("weekly"),
			LastContacted: &sourceLastContacted,
		})
		require.NoError(t, err)

		merged, err := contactService.MergeContacts(ctx, service.MergeContactsRequest{
			TargetContactID: target.ID,
			SourceContactID: source.ID,
			FieldSelections: service.MergeFieldSelections{Cadence: "target"},
		})
		require.NoError(t, err)

		// Refetch from the DB — the returned struct must match the
		// committed row across updated_at AND cadence fields.
		committed, err := contactRepo.GetContact(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, committed.UpdatedAt, merged.UpdatedAt,
			"returned updated_at should reflect post-BulkApply committed state")
		require.NotNil(t, committed.LastContacted)
		require.NotNil(t, merged.LastContacted)
		assert.Equal(t, committed.LastContacted.UTC(), merged.LastContacted.UTC(),
			"returned last_contacted should reflect post-BulkApply committed state")
		require.NotNil(t, merged.ContactBy)
		require.NotNil(t, committed.ContactBy)
		assert.Equal(t, committed.ContactBy.UTC(), merged.ContactBy.UTC(),
			"returned contact_by should reflect post-BulkApply committed state")
		// Sanity: last_contacted did actually advance to the source
		// value, confirming BulkApply ran and the refetch picked it up.
		assert.Equal(t, sourceLastContacted.UTC(), merged.LastContacted.UTC(),
			"BulkApply should forward-max last_contacted to the source value")
	})
}
