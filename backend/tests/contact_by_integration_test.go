// Package tests contains integration tests for contact_by functionality.
//
// Design Note: contact_by is an internal field used for overdue calculation.
// It is NOT displayed in the frontend UI - users see "last_contacted" and
// "cadence" instead. The overdue status badge and sorting are derived from
// contact_by on the backend and returned to the frontend as part of the
// overdue contacts API response.
package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContactBy_CreateWithCadence verifies that contact_by is correctly set when creating a contact with cadence.
func TestContactBy_CreateWithCadence(t *testing.T) {
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

	contactRepo := repository.NewContactRepository(database.Queries)

	// Test: Create contact with weekly cadence
	t.Run("weekly cadence sets contact_by to created_at + 7 days", func(t *testing.T) {
		weeklyStr := "weekly"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Weekly Contact",
			Cadence:  &weeklyStr,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		require.NotNil(t, contact.ContactBy, "contact_by should be set")

		// Verify contact_by is created_at + 7 days (using date-only comparison)
		expectedDate := cadence.CalculateContactBy(contact.CreatedAt, cadence.CadenceWeekly)
		assert.Equal(t, cadence.DateOnly(expectedDate).Year(), contact.ContactBy.Year())
		assert.Equal(t, cadence.DateOnly(expectedDate).Month(), contact.ContactBy.Month())
		assert.Equal(t, cadence.DateOnly(expectedDate).Day(), contact.ContactBy.Day())
	})

	t.Run("monthly cadence sets contact_by to created_at + 30 days", func(t *testing.T) {
		monthlyStr := "monthly"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Monthly Contact",
			Cadence:  &monthlyStr,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		require.NotNil(t, contact.ContactBy, "contact_by should be set")

		expectedDate := cadence.CalculateContactBy(contact.CreatedAt, cadence.CadenceMonthly)
		assert.Equal(t, cadence.DateOnly(expectedDate).Year(), contact.ContactBy.Year())
		assert.Equal(t, cadence.DateOnly(expectedDate).Month(), contact.ContactBy.Month())
		assert.Equal(t, cadence.DateOnly(expectedDate).Day(), contact.ContactBy.Day())
	})

	t.Run("no cadence leaves contact_by nil", func(t *testing.T) {
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test No Cadence Contact",
			Cadence:  nil,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		assert.Nil(t, contact.ContactBy, "contact_by should be nil when no cadence")
	})

	t.Run("with last_contacted uses last_contacted as base", func(t *testing.T) {
		weeklyStr := "weekly"
		lastContacted := time.Date(2024, 1, 15, 10, 30, 0, 0, time.Local)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Test With LastContacted",
			Cadence:       &weeklyStr,
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		require.NotNil(t, contact.ContactBy, "contact_by should be set")

		// Verify contact_by is last_contacted + 7 days (not created_at + 7 days)
		expectedDate := cadence.CalculateContactBy(lastContacted, cadence.CadenceWeekly)
		assert.Equal(t, cadence.DateOnly(expectedDate).Year(), contact.ContactBy.Year())
		assert.Equal(t, cadence.DateOnly(expectedDate).Month(), contact.ContactBy.Month())
		assert.Equal(t, cadence.DateOnly(expectedDate).Day(), contact.ContactBy.Day())
	})
}

// TestContactBy_UpdateLastContacted verifies that contact_by is recalculated when last_contacted is updated.
func TestContactBy_UpdateLastContacted(t *testing.T) {
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

	contactRepo := repository.NewContactRepository(database.Queries)

	t.Run("updating last_contacted recalculates contact_by", func(t *testing.T) {
		weeklyStr := "weekly"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Update LastContacted",
			Cadence:  &weeklyStr,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		originalContactBy := contact.ContactBy
		require.NotNil(t, originalContactBy)

		// Update last_contacted to a new date
		newLastContacted := accelerated.GetCurrentTime()
		expectedContactBy := cadence.CalculateContactBy(newLastContacted, cadence.CadenceWeekly)

		err = contactRepo.UpdateContactLastContacted(ctx, contact.ID, newLastContacted, &expectedContactBy)
		require.NoError(t, err)

		// Verify contact_by was recalculated
		updatedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy)

		assert.Equal(t, cadence.DateOnly(expectedContactBy).Year(), updatedContact.ContactBy.Year())
		assert.Equal(t, cadence.DateOnly(expectedContactBy).Month(), updatedContact.ContactBy.Month())
		assert.Equal(t, cadence.DateOnly(expectedContactBy).Day(), updatedContact.ContactBy.Day())
	})

	t.Run("updating last_contacted without cadence keeps contact_by nil", func(t *testing.T) {
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Update LastContacted No Cadence",
			Cadence:  nil,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		assert.Nil(t, contact.ContactBy)

		// Update last_contacted (without contact_by since no cadence)
		newLastContacted := accelerated.GetCurrentTime()
		err = contactRepo.UpdateContactLastContacted(ctx, contact.ID, newLastContacted, nil)
		require.NoError(t, err)

		// Verify contact_by is still nil
		updatedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Nil(t, updatedContact.ContactBy)
	})
}

// TestContactBy_UpdateContactLastContactedIfLater verifies the conditional update logic.
func TestContactBy_UpdateContactLastContactedIfLater(t *testing.T) {
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

	contactRepo := repository.NewContactRepository(database.Queries)

	t.Run("updates contact_by when new date is later", func(t *testing.T) {
		weeklyStr := "weekly"
		initialDate := time.Date(2024, 1, 1, 12, 0, 0, 0, time.Local)
		initialContactBy := cadence.CalculateContactBy(initialDate, cadence.CadenceWeekly)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Test IfLater Update",
			Cadence:       &weeklyStr,
			LastContacted: &initialDate,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		originalContactBy := contact.ContactBy
		require.NotNil(t, originalContactBy)
		assert.Equal(t, cadence.DateOnly(initialContactBy).Day(), originalContactBy.Day())

		// Try to update with a later date
		laterDate := initialDate.Add(7 * 24 * time.Hour) // 7 days later
		err = contactRepo.UpdateContactLastContactedIfLater(ctx, contact.ID, laterDate)
		require.NoError(t, err)

		// Verify contact_by was recalculated
		updatedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy)

		expectedContactBy := cadence.CalculateContactBy(laterDate, cadence.CadenceWeekly)
		assert.Equal(t, cadence.DateOnly(expectedContactBy).Day(), updatedContact.ContactBy.Day())
	})

	t.Run("does not update contact_by when new date is earlier", func(t *testing.T) {
		weeklyStr := "weekly"
		initialDate := time.Date(2024, 1, 15, 12, 0, 0, 0, time.Local)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Test IfLater No Update",
			Cadence:       &weeklyStr,
			LastContacted: &initialDate,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		originalContactBy := contact.ContactBy
		require.NotNil(t, originalContactBy)

		// Try to update with an earlier date
		earlierDate := initialDate.Add(-7 * 24 * time.Hour) // 7 days earlier
		err = contactRepo.UpdateContactLastContactedIfLater(ctx, contact.ID, earlierDate)
		require.NoError(t, err)

		// Verify contact_by was NOT changed
		updatedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy)

		// contact_by should still be based on initialDate, not earlierDate
		assert.Equal(t, originalContactBy.Day(), updatedContact.ContactBy.Day())
	})

	t.Run("handles contact without cadence (sets contact_by to NULL)", func(t *testing.T) {
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test IfLater No Cadence",
			Cadence:  nil,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		assert.Nil(t, contact.ContactBy)

		// Update last_contacted
		newDate := accelerated.GetCurrentTime()
		err = contactRepo.UpdateContactLastContactedIfLater(ctx, contact.ID, newDate)
		require.NoError(t, err)

		// Verify contact_by is still nil (no cadence to calculate from)
		updatedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Nil(t, updatedContact.ContactBy)
	})

	// Note: Empty string cadence is rejected by the database check constraint
	// (contact_cadence_check). The API layer should treat empty string as nil,
	// which is tested in handler validation tests. The SQL in
	// UpdateContactLastContactedIfLater correctly handles the case where
	// cadence != '' in its CASE statement, ensuring empty string is treated
	// as no cadence if it somehow got into the database.
}

// TestContactBy_CadenceStateTransitions verifies contact_by behavior when
// cadence changes. Post-PR-8 cutover the repository.UpdateContact query is
// profile-only; contact_by mutations route through
// ContactService.UpdateContact → CadenceUpdater.ApplyContactByOverride, so
// this test exercises the service layer to cover the end-to-end cadence
// edit path (plan Step 15 retargeting).
func TestContactBy_CadenceStateTransitions(t *testing.T) {
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)
	wireCadenceUpdaterForTest(t, database, contactService)

	t.Run("set cadence -> clear cadence -> set cadence", func(t *testing.T) {
		weeklyStr := "weekly"
		monthlyStr := "monthly"

		// Step 1: Create contact WITH cadence
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Cadence State Transitions",
			Cadence:  &weeklyStr,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Verify contact_by is set initially
		require.NotNil(t, contact.ContactBy, "contact_by should be set when cadence is set")
		initialContactBy := *contact.ContactBy

		// Step 2: Update contact to CLEAR cadence. The service recomputes
		// contact_by from the new cadence (nil → clear) and routes the
		// contact_by write through CadenceUpdater.ApplyContactByOverride.
		updatedContact, _, err := contactService.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: contact.FullName,
			Cadence:  nil, // Clear cadence
		}, nil, false)
		require.NoError(t, err)
		assert.Nil(t, updatedContact.ContactBy, "contact_by should be nil when cadence is cleared")

		// Verify by re-fetching
		fetchedContact, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Nil(t, fetchedContact.ContactBy, "contact_by should remain nil after fetch")
		// Round-2 blocker guard: the service returned struct's updated_at
		// must match the committed row's updated_at (both the profile
		// UpdateContact and the ApplyContactByOverride bump updated_at;
		// the service used to return the former's stale value).
		assert.Equal(t, fetchedContact.UpdatedAt, updatedContact.UpdatedAt,
			"returned updated_at should reflect post-override committed state")

		// Step 3: Update contact to SET cadence again (different cadence)
		updatedContact, _, err = contactService.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: contact.FullName,
			Cadence:  &monthlyStr,
		}, nil, false)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy, "contact_by should be set when cadence is re-enabled")

		// Verify the new contact_by is different from the initial one
		// (it should be based on monthly cadence now, not weekly)
		assert.NotEqual(t, initialContactBy.Day(), updatedContact.ContactBy.Day(),
			"new contact_by should differ from initial (different cadence)")
	})

	t.Run("no cadence -> set cadence", func(t *testing.T) {
		weeklyStr := "weekly"

		// Create contact WITHOUT cadence
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test No Cadence To Set",
			Cadence:  nil,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		assert.Nil(t, contact.ContactBy, "contact_by should be nil initially")

		// Update to add cadence through the service layer.
		updatedContact, _, err := contactService.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: contact.FullName,
			Cadence:  &weeklyStr,
		}, nil, false)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy, "contact_by should be set when cadence is added")
	})

	t.Run("change cadence type updates contact_by accordingly", func(t *testing.T) {
		weeklyStr := "weekly"
		annualStr := "annual"
		lastContacted := time.Date(2024, 6, 15, 12, 0, 0, 0, time.Local)

		// Create with weekly cadence + a fixed last_contacted so the
		// service-layer contact_by recompute has a stable base.
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Test Change Cadence Type",
			Cadence:       &weeklyStr,
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		require.NotNil(t, contact.ContactBy)
		weeklyContactBy := *contact.ContactBy

		// Change to annual cadence; the service recomputes contact_by
		// from (new cadence, existing last_contacted).
		updatedContact, _, err := contactService.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: contact.FullName,
			Cadence:  &annualStr,
		}, nil, false)
		require.NoError(t, err)
		require.NotNil(t, updatedContact.ContactBy)

		// Annual contact_by should be much later than weekly
		daysDiff := updatedContact.ContactBy.Sub(weeklyContactBy).Hours() / 24
		assert.Greater(t, daysDiff, float64(300), "annual contact_by should be ~358 days later than weekly")
	})
}

// TestContactBy_ListOverdueContacts verifies the overdue contacts query.
func TestContactBy_ListOverdueContacts(t *testing.T) {
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

	contactRepo := repository.NewContactRepository(database.Queries)

	// Create test contacts with different contact_by dates
	weeklyStr := "weekly"

	// Contact that is overdue (contact_by in the past)
	pastDate := time.Date(2024, 1, 1, 12, 0, 0, 0, time.Local)
	overdueContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Overdue Contact",
		Cadence:       &weeklyStr,
		LastContacted: &pastDate, // contact_by will be 2024-01-08
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, overdueContact.ID) }()

	// Contact that is not overdue (contact_by in the future)
	futureDate := accelerated.GetCurrentTime().AddDate(0, 0, 7)
	notOverdueContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Not Overdue Contact",
		Cadence:       &weeklyStr,
		LastContacted: &futureDate, // contact_by will be in the future
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, notOverdueContact.ID) }()

	// Contact without cadence (no contact_by)
	noCadenceContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "No Cadence Contact",
		Cadence:  nil,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, noCadenceContact.ID) }()

	t.Run("returns only overdue contacts", func(t *testing.T) {
		today := cadence.Today(accelerated.GetCurrentTime())

		overdueContacts, err := contactRepo.ListOverdueContacts(ctx, today, 100)
		require.NoError(t, err)

		// Find our test contacts in the results
		var foundOverdue, foundNotOverdue, foundNoCadence bool
		for _, c := range overdueContacts {
			if c.ID == overdueContact.ID {
				foundOverdue = true
			}
			if c.ID == notOverdueContact.ID {
				foundNotOverdue = true
			}
			if c.ID == noCadenceContact.ID {
				foundNoCadence = true
			}
		}

		assert.True(t, foundOverdue, "overdue contact should be returned")
		assert.False(t, foundNotOverdue, "not-overdue contact should not be returned")
		assert.False(t, foundNoCadence, "contact without cadence should not be returned")
	})

	t.Run("results are ordered by contact_by ASC (most overdue first)", func(t *testing.T) {
		// Create another overdue contact with an even older contact_by
		veryOldDate := time.Date(2023, 6, 1, 12, 0, 0, 0, time.Local)
		veryOverdueContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Very Overdue Contact",
			Cadence:       &weeklyStr,
			LastContacted: &veryOldDate, // contact_by will be 2023-06-08
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, veryOverdueContact.ID) }()

		today := cadence.Today(accelerated.GetCurrentTime())
		overdueContacts, err := contactRepo.ListOverdueContacts(ctx, today, 100)
		require.NoError(t, err)

		// Find positions of our test contacts
		var veryOverdueIdx, overdueIdx = -1, -1
		for i, c := range overdueContacts {
			if c.ID == veryOverdueContact.ID {
				veryOverdueIdx = i
			}
			if c.ID == overdueContact.ID {
				overdueIdx = i
			}
		}

		// Very overdue should come before regular overdue
		if veryOverdueIdx != -1 && overdueIdx != -1 {
			assert.Less(t, veryOverdueIdx, overdueIdx, "very overdue contact should come first")
		}
	})
}
