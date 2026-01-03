package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReminderRepository_SourceField tests that reminders can be created with source field
func TestReminderRepository_SourceField(t *testing.T) {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)

	// Create a test contact
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Reminder Source Test Contact",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	t.Run("CreateReminder_DefaultsToManual", func(t *testing.T) {
		reminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
			ContactID:   &contact.ID,
			Title:       "Manual Reminder",
			DueDate:     accelerated.GetCurrentTime().Add(24 * time.Hour),
			Description: stringPtr("Test description"),
		})
		require.NoError(t, err)
		require.NotNil(t, reminder)
		defer func() { _ = reminderRepo.HardDeleteReminder(ctx, reminder.ID) }()

		// Source should default to manual when not specified
		require.NotNil(t, reminder.Source)
		assert.Equal(t, repository.ReminderSourceManual, *reminder.Source)
	})

	t.Run("CreateReminder_WithAutoSource", func(t *testing.T) {
		source := repository.ReminderSourceAuto
		reminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
			ContactID: &contact.ID,
			Title:     "Auto Reminder",
			DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
			Source:    &source,
		})
		require.NoError(t, err)
		require.NotNil(t, reminder)
		defer func() { _ = reminderRepo.HardDeleteReminder(ctx, reminder.ID) }()

		require.NotNil(t, reminder.Source)
		assert.Equal(t, repository.ReminderSourceAuto, *reminder.Source)
	})

	t.Run("CreateReminder_WithManualSource", func(t *testing.T) {
		source := repository.ReminderSourceManual
		reminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
			ContactID: &contact.ID,
			Title:     "Explicit Manual Reminder",
			DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
			Source:    &source,
		})
		require.NoError(t, err)
		require.NotNil(t, reminder)
		defer func() { _ = reminderRepo.HardDeleteReminder(ctx, reminder.ID) }()

		require.NotNil(t, reminder.Source)
		assert.Equal(t, repository.ReminderSourceManual, *reminder.Source)
	})
}

// TestReminderRepository_CompleteAutoRemindersForContact tests the new repository method
func TestReminderRepository_CompleteAutoRemindersForContact(t *testing.T) {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)

	// Create a test contact
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Complete Auto Reminders Test Contact",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	autoSource := repository.ReminderSourceAuto
	manualSource := repository.ReminderSourceManual

	// Create auto reminder 1 (incomplete)
	autoReminder1, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Auto Reminder 1",
		DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
		Source:    &autoSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, autoReminder1.ID) }()

	// Create auto reminder 2 (incomplete)
	autoReminder2, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Auto Reminder 2",
		DueDate:   accelerated.GetCurrentTime().Add(48 * time.Hour),
		Source:    &autoSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, autoReminder2.ID) }()

	// Create manual reminder (should NOT be completed)
	manualReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Manual Reminder",
		DueDate:   accelerated.GetCurrentTime().Add(72 * time.Hour),
		Source:    &manualSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, manualReminder.ID) }()

	// Complete auto reminders for this contact
	err = reminderRepo.CompleteAutoRemindersForContact(ctx, contact.ID)
	require.NoError(t, err)

	// Verify auto reminders are completed
	reminder1After, err := reminderRepo.GetReminder(ctx, autoReminder1.ID)
	require.NoError(t, err)
	assert.True(t, reminder1After.Completed, "Auto reminder 1 should be completed")
	assert.NotNil(t, reminder1After.CompletedAt, "Auto reminder 1 should have completed_at set")

	reminder2After, err := reminderRepo.GetReminder(ctx, autoReminder2.ID)
	require.NoError(t, err)
	assert.True(t, reminder2After.Completed, "Auto reminder 2 should be completed")
	assert.NotNil(t, reminder2After.CompletedAt, "Auto reminder 2 should have completed_at set")

	// Verify manual reminder is NOT completed
	manualAfter, err := reminderRepo.GetReminder(ctx, manualReminder.ID)
	require.NoError(t, err)
	assert.False(t, manualAfter.Completed, "Manual reminder should NOT be completed")
	assert.Nil(t, manualAfter.CompletedAt, "Manual reminder should NOT have completed_at set")
}

// TestReminderRepository_SoftDeleteRemindersForContact tests soft-deleting all reminders for a contact
func TestReminderRepository_SoftDeleteRemindersForContact(t *testing.T) {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)

	// Create a test contact
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Soft Delete Reminders Test Contact",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	autoSource := repository.ReminderSourceAuto
	manualSource := repository.ReminderSourceManual

	// Create reminders with both sources
	autoReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Auto Reminder for Soft Delete",
		DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
		Source:    &autoSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, autoReminder.ID) }()

	manualReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Manual Reminder for Soft Delete",
		DueDate:   accelerated.GetCurrentTime().Add(48 * time.Hour),
		Source:    &manualSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, manualReminder.ID) }()

	// Verify reminders exist
	reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, reminders, 2)

	// Soft delete all reminders for this contact
	err = reminderRepo.SoftDeleteRemindersForContact(ctx, contact.ID)
	require.NoError(t, err)

	// Verify reminders are soft-deleted (not returned by ListRemindersByContact)
	remindersAfter, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, remindersAfter, 0, "All reminders should be soft-deleted")
}

// TestContactService_MarkAsContactedCompletesAutoReminders tests the service integration
func TestContactService_MarkAsContactedCompletesAutoReminders(t *testing.T) {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)

	// Create a test contact
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Service Mark Contacted Test",
		Cadence:  stringPtr("weekly"),
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	autoSource := repository.ReminderSourceAuto
	manualSource := repository.ReminderSourceManual

	// Create auto and manual reminders
	autoReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Reach out to Service Mark Contacted Test (weekly)",
		DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
		Source:    &autoSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, autoReminder.ID) }()

	manualReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Custom reminder for service test",
		DueDate:   accelerated.GetCurrentTime().Add(48 * time.Hour),
		Source:    &manualSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, manualReminder.ID) }()

	// Call the service method to mark as contacted
	updatedContact, err := contactService.UpdateContactLastContacted(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedContact)
	assert.NotNil(t, updatedContact.LastContacted, "LastContacted should be set")

	// Verify auto reminder is completed
	autoAfter, err := reminderRepo.GetReminder(ctx, autoReminder.ID)
	require.NoError(t, err)
	assert.True(t, autoAfter.Completed, "Auto reminder should be completed after marking as contacted")

	// Verify manual reminder is NOT completed
	manualAfter, err := reminderRepo.GetReminder(ctx, manualReminder.ID)
	require.NoError(t, err)
	assert.False(t, manualAfter.Completed, "Manual reminder should NOT be completed after marking as contacted")
}

// TestContactService_DeleteContactSoftDeletesReminders tests that deleting a contact soft-deletes its reminders
func TestContactService_DeleteContactSoftDeletesReminders(t *testing.T) {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)

	// Create a test contact
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Service Delete Contact Test",
	})
	require.NoError(t, err)
	// No defer for contact cleanup since we're deleting it in the test

	autoSource := repository.ReminderSourceAuto
	manualSource := repository.ReminderSourceManual

	// Create reminders
	autoReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Auto Reminder for Delete Test",
		DueDate:   accelerated.GetCurrentTime().Add(24 * time.Hour),
		Source:    &autoSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, autoReminder.ID) }()

	manualReminder, err := reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
		ContactID: &contact.ID,
		Title:     "Manual Reminder for Delete Test",
		DueDate:   accelerated.GetCurrentTime().Add(48 * time.Hour),
		Source:    &manualSource,
	})
	require.NoError(t, err)
	defer func() { _ = reminderRepo.HardDeleteReminder(ctx, manualReminder.ID) }()

	// Verify reminders exist before deletion
	remindersBefore, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, remindersBefore, 2)

	// Delete the contact
	err = contactService.DeleteContact(ctx, contact.ID)
	require.NoError(t, err)

	// Verify all reminders are soft-deleted
	remindersAfter, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, remindersAfter, 0, "All reminders should be soft-deleted when contact is deleted")

	// Cleanup - hard delete the contact since it's only soft-deleted
	_ = contactRepo.HardDeleteContact(ctx, contact.ID)
}
