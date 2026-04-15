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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarEventUpsertResetsLastContactedUpdated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Migrations are applied once by TestMain.
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
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Calendar Event Match Reset",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	contactTwo, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Calendar Event Match Reset Two",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contactTwo.ID) }()

	accountID := "calendar-reset@example.com"
	defer func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) }()

	now := accelerated.GetCurrentTime()
	startTime := now.Add(-2 * time.Hour)
	endTime := now.Add(-time.Hour)
	title := "Test Meeting"
	userResponse := "accepted"

	makeRequest := func(eventID string, matched []uuid.UUID, updated bool) repository.UpsertCalendarEventRequest {
		return repository.UpsertCalendarEventRequest{
			GcalEventID:          eventID,
			GcalCalendarID:       "primary",
			GoogleAccountID:      accountID,
			Title:                &title,
			StartTime:            startTime,
			EndTime:              endTime,
			AllDay:               false,
			Status:               "confirmed",
			UserResponse:         &userResponse,
			Attendees:            []repository.Attendee{},
			MatchedContactIDs:    matched,
			SyncedAt:             now,
			LastContactedUpdated: updated,
		}
	}

	t.Run("ResetsOnNewMatch", func(t *testing.T) {
		eventID := "event-" + uuid.NewString()
		req := makeRequest(eventID, []uuid.UUID{}, true)

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		req.MatchedContactIDs = []uuid.UUID{contact.ID}
		req.LastContactedUpdated = false

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		updatedEvent, err := calendarRepo.GetByGcalID(ctx, req.GcalEventID, req.GcalCalendarID, req.GoogleAccountID)
		require.NoError(t, err)
		assert.False(t, updatedEvent.LastContactedUpdated)
	})

	t.Run("ResetsOnAddedMatch", func(t *testing.T) {
		eventID := "event-" + uuid.NewString()
		req := makeRequest(eventID, []uuid.UUID{contact.ID}, true)

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		req.MatchedContactIDs = []uuid.UUID{contact.ID, contactTwo.ID}
		req.LastContactedUpdated = false

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		updatedEvent, err := calendarRepo.GetByGcalID(ctx, req.GcalEventID, req.GcalCalendarID, req.GoogleAccountID)
		require.NoError(t, err)
		assert.False(t, updatedEvent.LastContactedUpdated)
	})

	t.Run("ResetsOnRemovedMatch", func(t *testing.T) {
		eventID := "event-" + uuid.NewString()
		req := makeRequest(eventID, []uuid.UUID{contact.ID, contactTwo.ID}, true)

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		req.MatchedContactIDs = []uuid.UUID{contact.ID}
		req.LastContactedUpdated = false

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		updatedEvent, err := calendarRepo.GetByGcalID(ctx, req.GcalEventID, req.GcalCalendarID, req.GoogleAccountID)
		require.NoError(t, err)
		assert.False(t, updatedEvent.LastContactedUpdated)
	})

	t.Run("PreservesOnReorder", func(t *testing.T) {
		eventID := "event-" + uuid.NewString()
		req := makeRequest(eventID, []uuid.UUID{contact.ID, contactTwo.ID}, true)

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		req.MatchedContactIDs = []uuid.UUID{contactTwo.ID, contact.ID}
		req.LastContactedUpdated = false

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		updatedEvent, err := calendarRepo.GetByGcalID(ctx, req.GcalEventID, req.GcalCalendarID, req.GoogleAccountID)
		require.NoError(t, err)
		assert.True(t, updatedEvent.LastContactedUpdated)
	})

	t.Run("PreservesOnSameMatches", func(t *testing.T) {
		eventID := "event-" + uuid.NewString()
		req := makeRequest(eventID, []uuid.UUID{contact.ID}, true)

		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		req.LastContactedUpdated = false
		_, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)

		stableEvent, err := calendarRepo.GetByGcalID(ctx, req.GcalEventID, req.GcalCalendarID, req.GoogleAccountID)
		require.NoError(t, err)
		assert.True(t, stableEvent.LastContactedUpdated)
	})
}

func TestUpdateContactLastContactedIfLater_OnlyUpdatesWhenLater(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Migrations are applied once by TestMain.
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
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Last Contacted Clamp",
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	initial := accelerated.GetCurrentTime().Add(-2 * time.Hour)
	later := initial.Add(2 * time.Hour)
	earlier := initial.Add(-2 * time.Hour)

	err = contactRepo.UpdateContactLastContacted(ctx, contact.ID, initial, nil)
	require.NoError(t, err)

	err = contactRepo.UpdateContactLastContactedIfLater(ctx, contact.ID, earlier)
	require.NoError(t, err)

	contactAfterEarlier, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, contactAfterEarlier.LastContacted)
	assert.WithinDuration(t, initial, *contactAfterEarlier.LastContacted, time.Second)

	err = contactRepo.UpdateContactLastContactedIfLater(ctx, contact.ID, later)
	require.NoError(t, err)

	contactAfterLater, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, contactAfterLater.LastContacted)
	assert.WithinDuration(t, later, *contactAfterLater.LastContacted, time.Second)
}
