package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduler_ExternalSyncSchedule_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Setenv("CRM_ENV", "test")

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
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	syncRepo := repository.NewSyncRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	syncService := service.NewSyncService(syncRepo, contactRepo, registry)

	schedulerInstance := scheduler.NewScheduler(reminderService, syncService, true)
	require.NotNil(t, schedulerInstance)

	require.NoError(t, schedulerInstance.Start())
	defer schedulerInstance.Stop()

	entries := schedulerInstance.GetScheduledJobs()
	require.Len(t, entries, 2)

	fixedTime := time.Date(2026, 1, 22, 10, 2, 0, 0, time.UTC)
	expectedSyncNext := time.Date(2026, 1, 22, 10, 5, 0, 0, time.UTC)
	expectedReminderNext := fixedTime.Add(30 * time.Second)

	foundSync := false
	foundReminder := false
	for _, entry := range entries {
		next := entry.Schedule.Next(fixedTime)
		if next.Equal(expectedSyncNext) {
			foundSync = true
		}
		if next.Equal(expectedReminderNext) {
			foundReminder = true
		}
	}

	assert.True(t, foundSync, "expected external sync schedule every 5 minutes")
	assert.True(t, foundReminder, "expected reminder schedule every 30 seconds in test env")
}
