package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
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
	calendarEventRepo := repository.NewCalendarEventRepository(database.Queries)

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

	t.Run("GetMergePreview_InteractionsAndCalendarEvents", func(t *testing.T) {
		// spec: CON-037[0]
		// Create target contact
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Target Events " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		// Create source contact
		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Merge Source Events " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) }()

		// Seed three interactions on the source contact. A count that differs
		// from the calendar-event count below (2) also catches a field-swap
		// defect, not just a dropped-to-zero one.
		now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
		_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  source.ID,
			Source:     repository.InteractionSourceManual,
			OccurredAt: now,
			Direction:  repository.InteractionDirectionOutbound,
		})
		require.NoError(t, err)
		_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  source.ID,
			Source:     repository.InteractionSourceManual,
			OccurredAt: now.Add(-time.Hour),
			Direction:  repository.InteractionDirectionInbound,
		})
		require.NoError(t, err)
		_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  source.ID,
			Source:     repository.InteractionSourceManual,
			OccurredAt: now.Add(-2 * time.Hour),
			Direction:  repository.InteractionDirectionOutbound,
		})
		require.NoError(t, err)

		// Seed two calendar events that match the source contact. Calendar
		// events are not FK-linked to contact (matched_contact_ids is a plain
		// uuid[] column, no ON DELETE CASCADE), so they must be hard-deleted
		// explicitly rather than relying on the source contact's cleanup.
		title := "Merge Preview Event"
		event1, err := calendarEventRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID:       "merge-preview-" + ns + "-1",
			GcalCalendarID:    "merge-preview-" + ns + "-cal",
			GoogleAccountID:   "merge-preview-" + ns + "-acct",
			Title:             &title,
			StartTime:         now,
			EndTime:           now.Add(time.Hour),
			Status:            "confirmed",
			Attendees:         []repository.Attendee{},
			MatchedContactIDs: []uuid.UUID{source.ID},
			SyncedAt:          now,
		})
		require.NoError(t, err)
		defer func() { _ = calendarEventRepo.TestHardDeleteByID(ctx, event1.ID) }()

		event2, err := calendarEventRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID:       "merge-preview-" + ns + "-2",
			GcalCalendarID:    "merge-preview-" + ns + "-cal",
			GoogleAccountID:   "merge-preview-" + ns + "-acct",
			Title:             &title,
			StartTime:         now.Add(24 * time.Hour),
			EndTime:           now.Add(25 * time.Hour),
			Status:            "confirmed",
			Attendees:         []repository.Attendee{},
			MatchedContactIDs: []uuid.UUID{source.ID},
			SyncedAt:          now,
		})
		require.NoError(t, err)
		defer func() { _ = calendarEventRepo.TestHardDeleteByID(ctx, event2.ID) }()

		// Get merge preview
		preview, err := contactService.GetMergePreview(ctx, source.ID, target.ID)
		require.NoError(t, err)

		assert.Equal(t, int64(3), preview.InteractionsToTransfer)
		assert.Equal(t, int64(2), preview.CalendarEventsToUpdate)
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

// setupContactMergePreviewTestRouter mirrors setupContactValidationTestRouter's
// wiring, but only registers the merge-preview route (GET /:id/merge/preview,
// where :id is the TARGET and source_id is a query param) plus a bare CreateContact
// route needed to seed fixtures through the HTTP layer's own DB handle.
func setupContactMergePreviewTestRouter() (*gin.Engine, func()) {
	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	if err != nil {
		panic("Failed to connect to test database: " + err.Error())
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForAPITest(nil, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(nil, database, nil)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil, cadenceUpdater, assertSvc, cache, nil)
	contactHandler := handlers.NewContactHandler(contactService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("/:id/merge/preview", contactHandler.GetMergePreview)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

func TestContactMergePreviewAPI_Errors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactMergePreviewTestRouter()
	defer cleanup()

	ns := uuid.New().String()[:8]

	// createContactViaAPI seeds a fixture contact through the router's own
	// CreateContact route, matching the DB handle the merge-preview route
	// reads from.
	createContactViaAPI := func(t *testing.T, name string) uuid.UUID {
		t.Helper()
		body, err := json.Marshal(handlers.CreateContactRequest{FullName: name})
		require.NoError(t, err)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		data, err := json.Marshal(response.Data)
		require.NoError(t, err)
		var created handlers.ContactResponse
		require.NoError(t, json.Unmarshal(data, &created))
		id, err := uuid.Parse(created.ID)
		require.NoError(t, err)
		return id
	}

	t.Run("GetMergePreview_UnknownSourceID_NotFound", func(t *testing.T) {
		// spec: CON-037[1]
		target := createContactViaAPI(t, "Preview Errors Target A "+ns)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+target.String()+"/merge/preview?source_id="+uuid.New().String(), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("GetMergePreview_UnknownTargetID_NotFound", func(t *testing.T) {
		// spec: CON-037[1]
		source := createContactViaAPI(t, "Preview Errors Source B "+ns)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+uuid.New().String()+"/merge/preview?source_id="+source.String(), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("GetMergePreview_MissingSourceID_ValidationError", func(t *testing.T) {
		// spec: CON-037[2]
		target := createContactViaAPI(t, "Preview Errors Target C "+ns)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+target.String()+"/merge/preview", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("GetMergePreview_MalformedSourceID_ValidationError", func(t *testing.T) {
		// spec: CON-037[2]
		target := createContactViaAPI(t, "Preview Errors Target D "+ns)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+target.String()+"/merge/preview?source_id=not-a-uuid", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})
}

// TestContactMergePreviewAPI_SuccessCounts proves the success path of the
// merge-preview route over HTTP: the JSON response must carry all five
// counters the spec names, with values matching seeded data — a service-level
// preview test cannot catch an omitted or misnamed response field.
func TestContactMergePreviewAPI_SuccessCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, rcleanup := setupContactMergePreviewTestRouter()
	defer rcleanup()

	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	ns := uuid.New().String()[:8]

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)
	calendarEventRepo := repository.NewCalendarEventRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil, cadenceUpdater, assertSvc, cache, nil)

	t.Run("GetMergePreview_AllFiveCountersInResponse", func(t *testing.T) {
		// spec: CON-037[0]
		target, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Preview Counts Target " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

		source, err := seedContactForMerge(ctx, contactService, repository.CreateContactRequest{
			FullName: "Preview Counts Source " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) }()

		// All five counters are pairwise-distinct (3/2/1/4/5) so swapping any
		// two DTO JSON tags produces a detectably different payload. A contact
		// has at most one notepad note, so notes_to_transfer anchors the value
		// 1 and every other counter avoids it. Target owns two shared methods,
		// making two of the source's five methods duplicates:
		// methods_to_transfer=3, duplicate_methods=2.
		for _, dupValue := range []string{"dup1-" + ns + "@example.com", "dup2-" + ns + "@example.com"} {
			_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
				ContactID: target.ID, Type: "email", Value: dupValue,
			})
			require.NoError(t, err)
			_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
				ContactID: source.ID, Type: "email", Value: dupValue,
			})
			require.NoError(t, err)
		}
		for _, v := range []string{"a-" + ns + "@example.com", "b-" + ns + "@example.com", "c-" + ns + "@example.com"} {
			_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
				ContactID: source.ID, Type: "email", Value: v,
			})
			require.NoError(t, err)
		}

		_, err = noteRepo.CreateNotepad(ctx, source.ID, "Preview counts notepad "+ns)
		require.NoError(t, err)

		now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
		for i := 0; i < 4; i++ {
			_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
				ContactID:  source.ID,
				Source:     repository.InteractionSourceManual,
				OccurredAt: now.Add(-time.Duration(i) * time.Hour),
				Direction:  repository.InteractionDirectionOutbound,
			})
			require.NoError(t, err)
		}

		title := "Preview Counts Event"
		for _, suffix := range []string{"1", "2", "3", "4", "5"} {
			event, err := calendarEventRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
				GcalEventID:       "preview-counts-" + ns + "-" + suffix,
				GcalCalendarID:    "preview-counts-" + ns + "-cal",
				GoogleAccountID:   "preview-counts-" + ns + "-acct",
				Title:             &title,
				StartTime:         now,
				EndTime:           now.Add(time.Hour),
				Status:            "confirmed",
				Attendees:         []repository.Attendee{},
				MatchedContactIDs: []uuid.UUID{source.ID},
				SyncedAt:          now,
			})
			require.NoError(t, err)
			eventID := event.ID
			defer func() { _ = calendarEventRepo.TestHardDeleteByID(ctx, eventID) }()
		}

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+target.ID.String()+"/merge/preview?source_id="+source.ID.String(), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.True(t, response.Success)

		// Assert the literal wire keys rather than decoding into the
		// production MergePreviewResponse struct: decoding with the same tags
		// that serialized the payload would round-trip a renamed or swapped
		// key and leave every assertion green.
		preview, ok := response.Data.(map[string]interface{})
		require.True(t, ok, "preview payload must be a JSON object")

		sourceObj, ok := preview["source_contact"].(map[string]interface{})
		require.True(t, ok, "response must carry a source_contact object")
		assert.Equal(t, source.ID.String(), sourceObj["id"])
		targetObj, ok := preview["target_contact"].(map[string]interface{})
		require.True(t, ok, "response must carry a target_contact object")
		assert.Equal(t, target.ID.String(), targetObj["id"])

		counters := map[string]float64{
			"methods_to_transfer":       3,
			"duplicate_methods":         2,
			"notes_to_transfer":         1,
			"interactions_to_transfer":  4,
			"calendar_events_to_update": 5,
		}
		for key, want := range counters {
			got, present := preview[key]
			require.True(t, present, "response must carry the %q key", key)
			assert.Equal(t, want, got, "counter %q", key)
		}
	})
}
