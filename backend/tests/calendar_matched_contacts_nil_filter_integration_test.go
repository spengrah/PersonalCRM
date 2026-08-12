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
	"github.com/stretchr/testify/require"
)

// TestCalendarMatchedContactsNilFilter exercises all three exported writers
// of calendar_event.matched_contact_ids — Upsert, UpdateMatchedContacts, and
// AppendMatchedContact — through their public repository methods, proving
// each filters a uuid.Nil element out on input. repo-shrink plan §1.5/§5.6:
// after the sqlc uuid[] override, a NULL array element decodes to uuid.Nil
// rather than being rejected, so the write path (not just the read path)
// must guard against it or a caller-supplied uuid.Nil would silently persist.
//
// The discriminating predicate is the existing CountEventsForContact(ctx,
// uuid.Nil): its query is server-side (`WHERE $1::uuid = ANY(matched_contact_ids)`)
// and returns an int64 straight from Postgres, so it never routes through
// convertDbCalendarEvent's read-side uuid.Nil filter and can't be fooled by
// it — unlike reading MatchedContactIDs back through GetByID/GetByGcalID,
// which filters uuid.Nil on the way out regardless of what the write path
// actually stored.
func TestCalendarMatchedContactsNilFilter(t *testing.T) {
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

	contactA, cleanupA := seedMigrationContact(ctx, t, database, gen)
	defer cleanupA()
	contactB, cleanupB := seedMigrationContact(ctx, t, database, gen)
	defer cleanupB()

	now := accelerated.GetCurrentTime()
	startTime := now.Add(-time.Hour)
	endTime := now

	t.Run("Upsert", func(t *testing.T) {
		accountID := gen.Prefix() + "nil-filter-upsert@example.com"
		defer func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) }()

		req := repository.UpsertCalendarEventRequest{
			GcalEventID:     "event-" + uuid.NewString(),
			GcalCalendarID:  "primary",
			GoogleAccountID: accountID,
			StartTime:       startTime,
			EndTime:         endTime,
			Status:          "confirmed",
			SyncedAt:        now,
			// uuid.Nil interleaved with real contacts: survivors must come
			// back as [B, A], in input order, with every nil dropped.
			MatchedContactIDs: []uuid.UUID{uuid.Nil, contactB.ID, uuid.Nil, contactA.ID},
		}

		event, err := calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{contactB.ID, contactA.ID}, event.MatchedContactIDs)

		nilCount, err := calendarRepo.CountEventsForContact(ctx, uuid.Nil)
		require.NoError(t, err)
		require.Zero(t, nilCount, "uuid.Nil must never be stored in matched_contact_ids")

		// An all-nil input reduces to a non-nil empty slice, not an error —
		// the write must not choke on (nor silently store) an all-filtered
		// input, and the repository's own read path must report it back
		// as an empty, non-nil slice.
		req.MatchedContactIDs = []uuid.UUID{uuid.Nil}
		event, err = calendarRepo.Upsert(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, event.MatchedContactIDs)
		require.Empty(t, event.MatchedContactIDs)
	})

	t.Run("UpdateMatchedContacts", func(t *testing.T) {
		accountID := gen.Prefix() + "nil-filter-update@example.com"
		defer func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) }()

		created, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID:     "event-" + uuid.NewString(),
			GcalCalendarID:  "primary",
			GoogleAccountID: accountID,
			StartTime:       startTime,
			EndTime:         endTime,
			Status:          "confirmed",
			SyncedAt:        now,
		})
		require.NoError(t, err)

		updated, err := calendarRepo.UpdateMatchedContacts(ctx, created.ID,
			[]uuid.UUID{uuid.Nil, contactA.ID, uuid.Nil, contactB.ID})
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{contactA.ID, contactB.ID}, updated.MatchedContactIDs)

		nilCount, err := calendarRepo.CountEventsForContact(ctx, uuid.Nil)
		require.NoError(t, err)
		require.Zero(t, nilCount, "uuid.Nil must never be stored in matched_contact_ids")

		cleared, err := calendarRepo.UpdateMatchedContacts(ctx, created.ID, []uuid.UUID{uuid.Nil})
		require.NoError(t, err)
		require.NotNil(t, cleared.MatchedContactIDs)
		require.Empty(t, cleared.MatchedContactIDs)
	})

	t.Run("AppendMatchedContact", func(t *testing.T) {
		accountID := gen.Prefix() + "nil-filter-append@example.com"
		defer func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) }()

		created, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID:       "event-" + uuid.NewString(),
			GcalCalendarID:    "primary",
			GoogleAccountID:   accountID,
			StartTime:         startTime,
			EndTime:           endTime,
			Status:            "confirmed",
			SyncedAt:          now,
			MatchedContactIDs: []uuid.UUID{contactA.ID},
		})
		require.NoError(t, err)

		// Appending uuid.Nil must be a no-op: it must never reach storage.
		err = calendarRepo.AppendMatchedContact(ctx, created.ID, uuid.Nil)
		require.NoError(t, err)

		err = calendarRepo.AppendMatchedContact(ctx, created.ID, contactB.ID)
		require.NoError(t, err)

		after, err := calendarRepo.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{contactA.ID, contactB.ID}, after.MatchedContactIDs)

		nilCount, err := calendarRepo.CountEventsForContact(ctx, uuid.Nil)
		require.NoError(t, err)
		require.Zero(t, nilCount, "uuid.Nil must never be stored in matched_contact_ids")
	})
}
