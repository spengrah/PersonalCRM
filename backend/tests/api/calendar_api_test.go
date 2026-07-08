//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// calendarEventsEnvelope mirrors api.APIResponse for the calendar list
// endpoints, unwrapping straight into the exported handlers response type.
type calendarEventsEnvelope struct {
	Success bool                             `json:"success"`
	Data    []handlers.CalendarEventResponse `json:"data"`
}

// newCalendarAPITest builds a fresh, empty per-subtest DB clone
// (newIsolatedRiverTestDB) and a router carrying only the production
// calendar route surface (handlers.RegisterCalendarRoutes). Because the
// clone starts empty, global-feed and pagination assertions in the caller
// are exact and independent of sibling subtests/tests. Returns the
// router, the calendar repository (for seeding events), and a seedContact
// helper (for seeding a matched contact).
func newCalendarAPITest(t *testing.T, ctx context.Context) (*gin.Engine, *repository.CalendarEventRepository, func(name string) uuid.UUID) {
	t.Helper()

	database, _ := newIsolatedRiverTestDB(t, ctx)

	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	handler := handlers.NewCalendarHandler(calendarRepo)

	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterCalendarRoutes(v1, handler)

	seedContact := func(name string) uuid.UUID {
		t.Helper()
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
		require.NoError(t, err)
		return contact.ID
	}

	return router, calendarRepo, seedContact
}

// seedEvent upserts a calendar event via the real repository (never raw
// SQL). suffix must be unique within the calling subtest's namespace so the
// (gcal_event_id, gcal_calendar_id, google_account_id) unique constraint
// never collides.
func seedEvent(t *testing.T, ctx context.Context, repo *repository.CalendarEventRepository, ns, suffix string, start, end time.Time, contactIDs []uuid.UUID, attendees []repository.Attendee) *repository.CalendarEvent {
	t.Helper()
	title := "Calendar API Test Event"
	ev, err := repo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       ns + "-" + suffix,
		GcalCalendarID:    ns + "-cal",
		GoogleAccountID:   ns + "-acct",
		Title:             &title,
		StartTime:         start,
		EndTime:           end,
		Status:            "confirmed",
		Attendees:         attendees,
		MatchedContactIDs: contactIDs,
		SyncedAt:          accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	return ev
}

// doCalendarGet serves a GET request against the router and returns the
// recorder.
func doCalendarGet(router *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	router.ServeHTTP(w, req)
	return w
}

// spec: CAL-022
//
// TestCalendarAPI_ReadEndpoints proves the calendar HTTP surface is
// readable, paginated with a bounded/default limit, rejects malformed
// contact ids, and exposes no write endpoints.
func TestCalendarAPI_ReadEndpoints(t *testing.T) {
	t.Parallel()

	t.Run("ContactEvents_ReturnsSeededEvent", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-read-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Read Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		ev := seedEvent(t, ctx, calendarRepo, ns, "past", now.Add(-48*time.Hour), now.Add(-47*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1)
		assert.Equal(t, ev.ID.String(), envelope.Data[0].ID)
	})

	t.Run("UpcomingForContact_FiltersToFuture", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-upcoming-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Upcoming Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEvent(t, ctx, calendarRepo, ns, "past", now.Add(-48*time.Hour), now.Add(-47*time.Hour), []uuid.UUID{contactID}, nil)
		future := seedEvent(t, ctx, calendarRepo, ns, "future", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1, "only the future event should be returned")
		assert.Equal(t, future.ID.String(), envelope.Data[0].ID)
	})

	t.Run("GlobalUpcoming_ReturnsFutureEvents", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-global-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Global Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		future := seedEvent(t, ctx, calendarRepo, ns, "future", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		// The clone is empty apart from this subtest's own seeding, so
		// the global feed is exactly the seeded future event.
		require.Len(t, envelope.Data, 1)
		assert.Equal(t, future.ID.String(), envelope.Data[0].ID)
	})

	t.Run("Pagination_ClampsAndDefaults", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-page-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Pagination Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		for i := 0; i < 101; i++ {
			start := now.Add(time.Duration(i+1) * time.Hour)
			seedEvent(t, ctx, calendarRepo, ns, fmt.Sprintf("page-%d", i), start, start.Add(30*time.Minute), []uuid.UUID{contactID}, nil)
		}

		// limit=1000 clamps to the hard max (100) — exercises the
		// limit > maxLimit branch, the load-bearing "bounded maximum" facet.
		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events?limit=1000")
		require.Equal(t, http.StatusOK, w.Code)
		var clamped calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &clamped))
		assert.Len(t, clamped.Data, 100, "limit above the hard max must clamp to 100")

		// No limit -> the sensible default of 20.
		w = doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)
		var defaulted calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &defaulted))
		assert.Len(t, defaulted.Data, 20, "an omitted limit must default to 20")

		// limit=5 -> honored exactly.
		w = doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events?limit=5")
		require.Equal(t, http.StatusOK, w.Code)
		var limited calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &limited))
		assert.Len(t, limited.Data, 5, "an explicit limit under the max must be honored")
	})

	t.Run("MalformedContactID_Returns400", func(t *testing.T) {
		ctx := context.Background()
		router, _, _ := newCalendarAPITest(t, ctx)

		w := doCalendarGet(router, "/api/v1/contacts/not-a-uuid/events")
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("RoutesAreReadOnly", func(t *testing.T) {
		ctx := context.Background()
		router, _, _ := newCalendarAPITest(t, ctx)

		routes := router.Routes()
		require.NotEmpty(t, routes, "the calendar router must register at least one route")
		for _, route := range routes {
			assert.Equal(t, http.MethodGet, route.Method, "calendar route %s %s must be GET-only — no endpoint creates, updates, or deletes an event", route.Method, route.Path)
		}
	})
}

// spec: CAL-023
//
// TestCalendarAPI_AttendeeRedaction proves event responses report an
// attendee count but never leak raw attendee emails or display names.
func TestCalendarAPI_AttendeeRedaction(t *testing.T) {
	t.Parallel()

	t.Run("Response_ReportsAttendeeCount", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-redact-count-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Redaction Count Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		attendees := []repository.Attendee{
			{Email: "organizer-" + ns + "@example.test", DisplayName: "Organizer " + ns, Organizer: true, Self: true},
			{Email: "attendee-" + ns + "@example.test", DisplayName: "Attendee Two " + ns},
		}
		seedEvent(t, ctx, calendarRepo, ns, "redact", now.Add(-time.Hour), now, []uuid.UUID{contactID}, attendees)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1)
		assert.Equal(t, 2, envelope.Data[0].AttendeeCount)
	})

	t.Run("Response_OmitsAttendeeAddresses", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-redact-omit-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Redaction Omit Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		organizerEmail := "organizer-" + ns + "@example.test"
		organizerName := "Organizer " + ns
		attendeeEmail := "attendee-" + ns + "@example.test"
		attendeeName := "Attendee Two " + ns
		attendees := []repository.Attendee{
			{Email: organizerEmail, DisplayName: organizerName, Organizer: true, Self: true},
			{Email: attendeeEmail, DisplayName: attendeeName},
		}
		seedEvent(t, ctx, calendarRepo, ns, "redact", now.Add(-time.Hour), now, []uuid.UUID{contactID}, attendees)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)

		body := w.Body.String()
		assert.NotContains(t, body, organizerEmail, "response must not leak the organizer's raw email")
		assert.NotContains(t, body, organizerName, "response must not leak the organizer's display name")
		assert.NotContains(t, body, attendeeEmail, "response must not leak the attendee's raw email")
		assert.NotContains(t, body, attendeeName, "response must not leak the attendee's display name")
	})
}
