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

	t.Parallel()
	ctx := context.Background()

	// Migrations are applied once by TestMain.
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8, // mirrors the lowered TestConfig() ceiling for parallel tests
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)
	defer database.Close()

	gen, _ := migrationGenerator(t)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)

	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	defer contactCleanup()

	contactTwo, contactTwoCleanup := seedMigrationContact(ctx, t, database, gen)
	defer contactTwoCleanup()

	accountID := gen.Prefix() + "calendar-reset@example.com"
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

// spec: CAL-020
//
// TestCalendarEventCountForContact_ExcludesCancelled binds the count read
// named by CAL-020: CountEventsForContact applies the same cancelled-event
// filter as the contact-facing list reads, so a cancelled event matched to
// the contact must not be counted. The count is scoped to a namespaced
// contact's matched_contact_ids, so the exact == 1 assertion is independent
// of sibling tests on the shared DB.
func TestCalendarEventCountForContact_ExcludesCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	ctx := context.Background()

	// Migrations are applied once by TestMain.
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8, // mirrors the lowered TestConfig() ceiling for parallel tests
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)
	defer database.Close()

	gen, _ := migrationGenerator(t)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)

	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	defer contactCleanup()

	accountID := gen.Prefix() + "calendar-count@example.com"
	defer func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) }()

	now := accelerated.GetCurrentTime()
	title := "Count Test Meeting"
	seed := func(suffix, status string, start time.Time) {
		_, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID:       "count-" + suffix + "-" + uuid.NewString(),
			GcalCalendarID:    "primary",
			GoogleAccountID:   accountID,
			Title:             &title,
			StartTime:         start,
			EndTime:           start.Add(time.Hour),
			Status:            status,
			MatchedContactIDs: []uuid.UUID{contact.ID},
			SyncedAt:          now,
		})
		require.NoError(t, err)
	}
	seed("cancelled", "cancelled", now.Add(-3*time.Hour))
	seed("confirmed", "confirmed", now.Add(-2*time.Hour))

	count, err := calendarRepo.CountEventsForContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the cancelled event must be excluded from the contact's event count")
}
