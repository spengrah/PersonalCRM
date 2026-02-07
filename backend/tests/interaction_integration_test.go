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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupInteractionTestDeps(t *testing.T) (*service.ContactService, *repository.InteractionRepository, *repository.ContactRepository, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	ctx := context.Background()
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo)

	cleanup := func() {
		database.Close()
	}

	return contactService, interactionRepo, contactRepo, cleanup
}

func createTestContactForInteraction(t *testing.T, contactRepo *repository.ContactRepository, name string) uuid.UUID {
	t.Helper()
	contact, err := contactRepo.CreateContact(context.Background(), repository.CreateContactRequest{
		FullName: name,
	})
	require.NoError(t, err)
	return contact.ID
}

func TestRecordInteraction_SourceRefDedup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	contactService, interactionRepo, contactRepo, cleanup := setupInteractionTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	contactID := createTestContactForInteraction(t, contactRepo, "SourceRef Dedup Test")
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contactID) }()

	sourceRef := "gcal-event-123"

	t.Run("FirstInteractionCreated", func(t *testing.T) {
		interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
			ContactID:  contactID,
			Source:     repository.InteractionSourceGCal,
			SourceRef:  &sourceRef,
			OccurredAt: time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, interaction.ID)
		assert.Equal(t, repository.InteractionSourceGCal, interaction.Source)
	})

	t.Run("DuplicateSourceRefReturnsSame", func(t *testing.T) {
		// Same source_ref should return existing
		interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
			ContactID:  contactID,
			Source:     repository.InteractionSourceGCal,
			SourceRef:  &sourceRef,
			OccurredAt: time.Date(2025, 6, 15, 15, 0, 0, 0, time.UTC), // different time, same ref
		})
		require.NoError(t, err)

		// Should only have 1 interaction total
		count, err := interactionRepo.CountContactInteractions(ctx, contactID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
		assert.Equal(t, repository.InteractionSourceGCal, interaction.Source)
	})

	t.Run("DifferentSourceRefCreatesNew", func(t *testing.T) {
		differentRef := "gcal-event-456"
		_, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
			ContactID:  contactID,
			Source:     repository.InteractionSourceGCal,
			SourceRef:  &differentRef,
			OccurredAt: time.Date(2025, 6, 16, 14, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)

		count, err := interactionRepo.CountContactInteractions(ctx, contactID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})
}

func TestRecordInteraction_ForwardOnlyLastContacted(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	contactService, _, contactRepo, cleanup := setupInteractionTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	contactID := createTestContactForInteraction(t, contactRepo, "Forward Only Test")
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contactID) }()

	recentTime := time.Date(2025, 12, 1, 14, 0, 0, 0, time.UTC)
	oldTime := time.Date(2025, 6, 1, 14, 0, 0, 0, time.UTC)
	recentRef := "recent-event"
	oldRef := "old-event"

	// Set last_contacted to recent time via a gcal interaction
	_, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceGCal,
		SourceRef:  &recentRef,
		OccurredAt: recentTime,
	})
	require.NoError(t, err)

	// Verify last_contacted is set
	contact, err := contactRepo.GetContact(ctx, contactID)
	require.NoError(t, err)
	require.NotNil(t, contact.LastContacted)
	assert.Equal(t, recentTime.UTC(), contact.LastContacted.UTC())

	// Now record an older gcal interaction
	_, err = contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceGCal,
		SourceRef:  &oldRef,
		OccurredAt: oldTime,
	})
	require.NoError(t, err)

	// last_contacted should NOT have moved backward
	contact, err = contactRepo.GetContact(ctx, contactID)
	require.NoError(t, err)
	require.NotNil(t, contact.LastContacted)
	assert.Equal(t, recentTime.UTC(), contact.LastContacted.UTC(), "automated source should not move last_contacted backward")
}

func TestRecordInteraction_ManualAlwaysUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	contactService, _, contactRepo, cleanup := setupInteractionTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	contactID := createTestContactForInteraction(t, contactRepo, "Manual Update Test")
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contactID) }()

	// Record an interaction with a recent time first (via contact creation auto-sets last_contacted)
	oldDate := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)

	// Manual source should always update, even to a past date
	_, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceManual,
		OccurredAt: oldDate,
	})
	require.NoError(t, err)

	contact, err := contactRepo.GetContact(ctx, contactID)
	require.NoError(t, err)
	require.NotNil(t, contact.LastContacted)
	assert.Equal(t, oldDate.UTC(), contact.LastContacted.UTC(), "manual source should always update last_contacted")
}

func TestRecordInteraction_NonExistentContact(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	contactService, _, _, cleanup := setupInteractionTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	fakeID := uuid.New()

	_, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  fakeID,
		Source:     repository.InteractionSourceManual,
		OccurredAt: accelerated.GetCurrentTime(),
	})
	assert.Error(t, err)
}
