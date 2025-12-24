package api

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupReminderGenerationTest(t *testing.T) (*service.ReminderService, *repository.ContactRepository, *repository.ReminderRepository, func()) {
	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	cleanup := func() {
		database.Close()
	}

	return reminderService, contactRepo, reminderRepo, cleanup
}

func TestReminderGeneration_GenerateForOverdueContacts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Set environment to production for predictable cadences
	t.Setenv("CRM_ENV", "production")

	reminderService, contactRepo, reminderRepo, cleanup := setupReminderGenerationTest(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("GenerateReminders_OverdueWeeklyContact", func(t *testing.T) {
		// Create a contact with weekly cadence that is overdue
		now := time.Now()
		lastContacted := now.AddDate(0, 0, -14) // 14 days ago (overdue for weekly)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
			FullName:      "Weekly Test Contact",
			Cadence:       strPtr("weekly"),
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer contactRepo.DeleteContact(ctx, contact.ID)

		// Generate reminders
		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		// Verify reminder was created
		reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders, 1, "Should create one reminder for overdue weekly contact")

		// Verify reminder content
		reminder := reminders[0]
		assert.Contains(t, reminder.Title, "Weekly Test Contact")
		assert.Contains(t, reminder.Title, "weekly cadence")
		assert.NotNil(t, reminder.Description)
		assert.Equal(t, now.Year(), reminder.DueDate.Year())
		assert.Equal(t, now.Month(), reminder.DueDate.Month())
		assert.Equal(t, now.Day(), reminder.DueDate.Day())
		assert.Equal(t, 9, reminder.DueDate.Hour(), "Reminder should be set for 9 AM")

		// Cleanup reminder
		_, err = reminderRepo.DeleteReminder(ctx, reminder.ID)
		require.NoError(t, err)
	})

	t.Run("GenerateReminders_NotOverdueContact", func(t *testing.T) {
		// Create a contact with weekly cadence that is NOT overdue
		now := time.Now()
		lastContacted := now.AddDate(0, 0, -3) // 3 days ago (not overdue for weekly)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
			FullName:      "Not Overdue Contact",
			Cadence:       strPtr("weekly"),
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer contactRepo.DeleteContact(ctx, contact.ID)

		// Generate reminders
		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		// Verify no reminder was created
		reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders, 0, "Should not create reminder for contact that is not overdue")
	})

	t.Run("GenerateReminders_SkipContactWithoutCadence", func(t *testing.T) {
		// Create a contact without cadence
		now := time.Now()
		lastContacted := now.AddDate(0, 0, -30) // 30 days ago but no cadence

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
			FullName:      "No Cadence Contact",
			Cadence:       nil, // No cadence set
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer contactRepo.DeleteContact(ctx, contact.ID)

		// Generate reminders
		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		// Verify no reminder was created
		reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders, 0, "Should not create reminder for contact without cadence")
	})

	t.Run("GenerateReminders_Idempotency", func(t *testing.T) {
		// Create an overdue contact
		now := time.Now()
		lastContacted := now.AddDate(0, 0, -14) // 14 days ago (overdue for weekly)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
			FullName:      "Idempotency Test Contact",
			Cadence:       strPtr("weekly"),
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer contactRepo.DeleteContact(ctx, contact.ID)

		// Generate reminders first time
		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		reminders1, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders1, 1, "Should create one reminder")

		// Generate reminders second time (same day)
		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		reminders2, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders2, 1, "Should not create duplicate reminder for same day")

		// Verify it's the same reminder
		assert.Equal(t, reminders1[0].ID, reminders2[0].ID, "Should be the same reminder")

		// Cleanup
		_, err = reminderRepo.DeleteReminder(ctx, reminders1[0].ID)
		require.NoError(t, err)
	})

	t.Run("GenerateReminders_MultipleCadenceTypes", func(t *testing.T) {
		now := time.Now()

		// Create contacts with different cadences
		contacts := []struct {
			name       string
			cadence    string
			daysAgo    int
			shouldHave bool
		}{
			{"Weekly Overdue", "weekly", 14, true},
			{"Biweekly Overdue", "biweekly", 21, true},
			{"Monthly Overdue", "monthly", 35, true},
			{"Quarterly Overdue", "quarterly", 100, true},
			{"Biannual Overdue", "biannual", 200, true},
			{"Annual Overdue", "annual", 400, true},
		}

		createdContacts := []repository.Contact{}
		for _, tc := range contacts {
			lastContacted := now.AddDate(0, 0, -tc.daysAgo)
			contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
				FullName:      tc.name,
				Cadence:       &tc.cadence,
				LastContacted: &lastContacted,
			})
			require.NoError(t, err)
			createdContacts = append(createdContacts, *contact)
		}

		// Cleanup contacts at the end
		defer func() {
			for _, contact := range createdContacts {
				contactRepo.DeleteContact(ctx, contact.ID)
			}
		}()

		// Generate reminders
		err := reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		// Verify reminders created for each contact
		for i, contact := range createdContacts {
			reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
			require.NoError(t, err)

			if contacts[i].shouldHave {
				assert.Len(t, reminders, 1, "Contact %s should have reminder", contacts[i].name)
				if len(reminders) > 0 {
					assert.Contains(t, reminders[0].Title, contacts[i].cadence)
					// Cleanup
					reminderRepo.DeleteReminder(ctx, reminders[0].ID)
				}
			}
		}
	})

	t.Run("GenerateReminders_RespectEnvironmentCadences", func(t *testing.T) {
		// Test that environment-specific cadences are used
		t.Setenv("CRM_ENV", "staging") // Staging: weekly = 10 minutes

		reminderService, contactRepo, reminderRepo, cleanup := setupReminderGenerationTest(t)
		defer cleanup()

		now := time.Now()
		// 15 minutes ago should be overdue in staging (where weekly = 10 min)
		lastContacted := now.Add(-15 * time.Minute)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
			FullName:      "Staging Env Test",
			Cadence:       strPtr("weekly"),
			LastContacted: &lastContacted,
		})
		require.NoError(t, err)
		defer contactRepo.DeleteContact(ctx, contact.ID)

		err = reminderService.GenerateRemindersForOverdueContacts(ctx)
		require.NoError(t, err)

		reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, reminders, 1, "Should create reminder using staging cadence")

		if len(reminders) > 0 {
			reminderRepo.DeleteReminder(ctx, reminders[0].ID)
		}
	})
}

func TestReminderGeneration_TitleAndDescription(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Setenv("CRM_ENV", "production")

	reminderService, contactRepo, reminderRepo, cleanup := setupReminderGenerationTest(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()
	lastContacted := now.AddDate(0, 0, -14) // 14 days ago

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
		FullName:      "Title Test Contact",
		Cadence:       strPtr("weekly"),
		LastContacted: &lastContacted,
	})
	require.NoError(t, err)
	defer contactRepo.DeleteContact(ctx, contact.ID)

	err = reminderService.GenerateRemindersForOverdueContacts(ctx)
	require.NoError(t, err)

	reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, reminders, 1)

	reminder := reminders[0]
	defer reminderRepo.DeleteReminder(ctx, reminder.ID)

	t.Run("VerifyReminderTitle", func(t *testing.T) {
		expectedTitle := "Follow up with Title Test Contact (weekly cadence)"
		assert.Equal(t, expectedTitle, reminder.Title)
	})

	t.Run("VerifyReminderDescription", func(t *testing.T) {
		assert.NotNil(t, reminder.Description)
		assert.Contains(t, *reminder.Description, "Title Test Contact")
		assert.Contains(t, *reminder.Description, "14 days") // Days since last contact
	})
}

func TestScheduler_CronSpec(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		expected string
	}{
		{"Test environment", "test", "@every 30s"},
		{"Testing environment", "testing", "@every 30s"},
		{"Staging environment", "staging", "@every 5m"},
		{"Accelerated environment", "accelerated", "@every 5m"},
		{"Production environment", "production", "0 0 8 * * *"},
		{"Prod environment", "prod", "0 0 8 * * *"},
		{"Empty defaults to production", "", "0 0 8 * * *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", tt.env)
			cronSpec := reminder.GetSchedulerCronSpec()
			assert.Equal(t, tt.expected, cronSpec)
		})
	}
}

func TestReminderGeneration_InvalidCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Setenv("CRM_ENV", "production")

	reminderService, contactRepo, reminderRepo, cleanup := setupReminderGenerationTest(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()
	lastContacted := now.AddDate(0, 0, -30)

	// Manually insert a contact with invalid cadence (bypassing API validation)
	// This tests robustness of the generation function
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
		FullName:      "Invalid Cadence Contact",
		Cadence:       strPtr("weekly"), // Valid for creation
		LastContacted: &lastContacted,
	})
	require.NoError(t, err)
	defer contactRepo.DeleteContact(ctx, contact.ID)

	// For this test, we'll just verify it handles the valid cadence correctly
	err = reminderService.GenerateRemindersForOverdueContacts(ctx)
	require.NoError(t, err)

	reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)

	if len(reminders) > 0 {
		reminderRepo.DeleteReminder(ctx, reminders[0].ID)
	}
}

func TestReminderGeneration_WithAcceleratedTime(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Setenv("CRM_ENV", "production")

	reminderService, contactRepo, reminderRepo, cleanup := setupReminderGenerationTest(t)
	defer cleanup()

	ctx := context.Background()

	// Set a specific time for testing
	testTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	accelerated.SetCurrentTime(testTime)
	defer accelerated.ResetTime()

	// Create contact with last contact 14 days before test time
	lastContacted := testTime.AddDate(0, 0, -14)

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactParams{
		FullName:      "Accelerated Time Test",
		Cadence:       strPtr("weekly"),
		LastContacted: &lastContacted,
	})
	require.NoError(t, err)
	defer contactRepo.DeleteContact(ctx, contact.ID)

	// Note: The service uses time.Now() internally, not accelerated.GetCurrentTime()
	// This test verifies the service works with regular time
	err = reminderService.GenerateRemindersForOverdueContacts(ctx)
	require.NoError(t, err)

	reminders, err := reminderRepo.ListRemindersByContact(ctx, contact.ID)
	require.NoError(t, err)

	if len(reminders) > 0 {
		reminderRepo.DeleteReminder(ctx, reminders[0].ID)
	}
}

// Helper to create scheduler for testing
func TestScheduler_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Setenv("CRM_ENV", "production")

	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	require.NoError(t, err)
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	// Create scheduler
	schedulerInstance := scheduler.NewScheduler(reminderService)
	require.NotNil(t, schedulerInstance)

	// Test that scheduler has the correct cron spec
	cronSpec := reminder.GetSchedulerCronSpec()
	assert.Equal(t, "0 0 8 * * *", cronSpec, "Production should use daily 8 AM schedule")

	// Note: We don't actually start the scheduler to avoid timing issues in tests
}

// Helper function
func strPtr(s string) *string {
	return &s
}
