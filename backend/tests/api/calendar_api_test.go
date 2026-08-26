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

// calendarRawEnvelope decodes the calendar list payload as raw JSON maps so
// assertions bind to the literal wire keys ("id", ...) instead of the
// production CalendarEventResponse struct — a renamed JSON tag on the DTO
// must turn these tests red rather than silently round-tripping.
type calendarRawEnvelope struct {
	Success bool             `json:"success"`
	Data    []map[string]any `json:"data"`
}

// rawEventIDs projects the raw event maps onto their literal "id" wire key.
func rawEventIDs(t *testing.T, data []map[string]any) []string {
	t.Helper()
	ids := make([]string, 0, len(data))
	for _, item := range data {
		id, ok := item["id"].(string)
		require.True(t, ok, "each event must expose a string id wire key")
		ids = append(ids, id)
	}
	return ids
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

// seedEventStatus is seedEvent but with an explicit status (e.g.
// "cancelled") instead of the hardcoded "confirmed". Used by tests that
// prove cancelled events are filtered out of contact-facing reads.
func seedEventStatus(t *testing.T, ctx context.Context, repo *repository.CalendarEventRepository, ns, suffix, status string, start, end time.Time, contactIDs []uuid.UUID) *repository.CalendarEvent {
	t.Helper()
	title := "Calendar API Test Event"
	ev, err := repo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       ns + "-" + suffix,
		GcalCalendarID:    ns + "-cal",
		GoogleAccountID:   ns + "-acct",
		Title:             &title,
		StartTime:         start,
		EndTime:           end,
		Status:            status,
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

	t.Run("UpcomingForContact_IncludedUntilEnded", func(t *testing.T) {
		// spec: CAL-031.included-until-ended
		// spec: CAL-031.ended-excluded
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-upcoming-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Upcoming Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEvent(t, ctx, calendarRepo, ns, "past", now.Add(-48*time.Hour), now.Add(-47*time.Hour), []uuid.UUID{contactID}, nil)
		inProgress := seedEvent(t, ctx, calendarRepo, ns, "inprogress", now.Add(-30*time.Minute), now.Add(time.Hour), []uuid.UUID{contactID}, nil)
		future := seedEvent(t, ctx, calendarRepo, ns, "future", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		// The contract keys on end time: an in-progress event stays upcoming
		// until it ends, while the ended event is excluded.
		require.Len(t, envelope.Data, 2, "in-progress and future events should be returned; ended events must be excluded")
		assert.Equal(t, []string{inProgress.ID.String(), future.ID.String()}, []string{envelope.Data[0].ID, envelope.Data[1].ID})
	})

	t.Run("UpcomingForContact_LimitCap", func(t *testing.T) {
		// spec: CAL-031.limit-cap-250
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-upcoming-limit-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Upcoming Limit Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		for i := 0; i < 251; i++ {
			start := now.Add(time.Duration(i+1) * time.Hour)
			seedEvent(t, ctx, calendarRepo, ns, fmt.Sprintf("future-%d", i), start, start.Add(time.Hour), []uuid.UUID{contactID}, nil)
		}

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)
		var defaulted calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &defaulted))
		assert.Len(t, defaulted.Data, 10, "an omitted limit must default to 10")

		w = doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming?limit=7")
		require.Equal(t, http.StatusOK, w.Code)
		var limited calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &limited))
		assert.Len(t, limited.Data, 7, "an explicit limit under the cap must be honored")

		w = doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming?limit=250")
		require.Equal(t, http.StatusOK, w.Code)
		var capped calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &capped))
		assert.Len(t, capped.Data, 250, "the endpoint must honor its 250-event maximum")

		w = doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming?limit=1000")
		require.Equal(t, http.StatusOK, w.Code)
		var clamped calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &clamped))
		assert.Len(t, clamped.Data, 250, "a limit above the endpoint maximum must clamp to 250")
	})

	t.Run("GlobalUpcoming_ReturnsFutureEvents", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-global-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Global Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEvent(t, ctx, calendarRepo, ns, "past", now.Add(-24*time.Hour), now.Add(-23*time.Hour), []uuid.UUID{contactID}, nil)
		seedEvent(t, ctx, calendarRepo, ns, "inprogress", now.Add(-30*time.Minute), now.Add(time.Hour), []uuid.UUID{contactID}, nil)
		future := seedEvent(t, ctx, calendarRepo, ns, "future", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		// The clone is empty apart from this subtest's own seeding, so the
		// global feed is exactly the seeded future event — the past and
		// in-progress (started-but-not-ended) events must be excluded.
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

// spec: CAL-031.included-until-ended
//
// TestUpcomingForContact_EndTimeBoundary proves that an event ending exactly
// at the current time remains in the contact-scoped upcoming feed, while one
// ending one second earlier is excluded.
func TestUpcomingForContact_EndTimeBoundary(t *testing.T) {
	ctx := context.Background()
	frozen := accelerated.GetCurrentTime().Truncate(time.Second)
	restore := accelerated.SetNowForTest(func() time.Time { return frozen })
	t.Cleanup(restore)

	router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)
	ns := "cal-upcoming-boundary-" + uuid.NewString()[:8]
	contactID := seedContact("Calendar Upcoming Boundary Test Contact " + ns)
	boundary := seedEvent(t, ctx, calendarRepo, ns, "boundary", frozen.Add(-time.Hour), frozen, []uuid.UUID{contactID}, nil)
	seedEvent(t, ctx, calendarRepo, ns, "ended", frozen.Add(-2*time.Hour), frozen.Add(-time.Second), []uuid.UUID{contactID}, nil)

	w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
	require.Equal(t, http.StatusOK, w.Code)
	var envelope calendarRawEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	assert.Equal(t, []string{boundary.ID.String()}, rawEventIDs(t, envelope.Data))
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

		// Prove the attendees were actually stored before asserting their
		// absence from the body — otherwise a seed path that silently drops
		// attendees would make the redaction assertions pass vacuously.
		var envelope calendarEventsEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.Len(t, envelope.Data, 1)
		require.Equal(t, 2, envelope.Data[0].AttendeeCount)

		body := w.Body.String()
		assert.NotContains(t, body, organizerEmail, "response must not leak the organizer's raw email")
		assert.NotContains(t, body, organizerName, "response must not leak the organizer's display name")
		assert.NotContains(t, body, attendeeEmail, "response must not leak the attendee's raw email")
		assert.NotContains(t, body, attendeeName, "response must not leak the attendee's display name")
	})
}

// spec: CAL-020
//
// TestCalendarAPI_CancelledEventsExcluded proves clause (b) of CAL-020 —
// "every contact-facing read filters out cancelled events" — across every
// contact-facing read endpoint the calendar HTTP surface exposes
// (ListEventsForContact, ListUpcomingEventsForContact,
// ListUpcomingEventsWithContacts): a cancelled event is excluded from that
// endpoint's response while a sibling non-cancelled event (same namespace,
// distinct identity) is returned. Clause (a) of CAL-020 — the hard delete —
// is proven jointly by TestIntegration_CalendarEvent_HardDeleteReadBack in
// calendar_decline_removal_integration_test.go.
func TestCalendarAPI_CancelledEventsExcluded(t *testing.T) {
	t.Parallel()

	t.Run("ContactEvents_ExcludesCancelled", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-cancel-history-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Cancelled History Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEventStatus(t, ctx, calendarRepo, ns, "cancelled", "cancelled", now.Add(-2*time.Hour), now.Add(-time.Hour), []uuid.UUID{contactID})
		kept := seedEventStatus(t, ctx, calendarRepo, ns, "confirmed", "confirmed", now.Add(-4*time.Hour), now.Add(-3*time.Hour), []uuid.UUID{contactID})

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1, "the cancelled event must be excluded from meeting history")
		assert.Equal(t, []string{kept.ID.String()}, rawEventIDs(t, envelope.Data))
	})

	t.Run("UpcomingForContact_ExcludesCancelled", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-cancel-upcoming-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Cancelled Upcoming Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEventStatus(t, ctx, calendarRepo, ns, "cancelled", "cancelled", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID})
		kept := seedEventStatus(t, ctx, calendarRepo, ns, "confirmed", "confirmed", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID})

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1, "the cancelled event must be excluded from the contact's upcoming events")
		assert.Equal(t, []string{kept.ID.String()}, rawEventIDs(t, envelope.Data))
	})

	t.Run("GlobalUpcoming_ExcludesCancelled", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-cancel-global-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Cancelled Global Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		seedEventStatus(t, ctx, calendarRepo, ns, "cancelled", "cancelled", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID})
		kept := seedEventStatus(t, ctx, calendarRepo, ns, "confirmed", "confirmed", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID})

		w := doCalendarGet(router, "/api/v1/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 1, "the cancelled event must be excluded from the global upcoming feed")
		assert.Equal(t, []string{kept.ID.String()}, rawEventIDs(t, envelope.Data))
	})
}

// spec: CAL-021
//
// TestCalendarAPI_EventOrdering proves every contact-facing read endpoint
// imposes an explicit, deterministic order: meeting history is returned
// most-recent-first and both upcoming feeds are returned soonest-first.
// Three namespaced events with pairwise-distinct times are seeded per
// endpoint over HTTP — no existing subtest asserts multi-event ordering
// because every prior seed left exactly one surviving event.
//
// Seeding order is deliberately SCRAMBLED (middle, then the extreme the sort
// puts first, then the extreme it puts last) so it matches neither the
// expected output order nor its reverse: a query whose ORDER BY is dropped
// (heap/insertion order) or inverted cannot coincidentally pass.
func TestCalendarAPI_EventOrdering(t *testing.T) {
	t.Parallel()

	t.Run("ContactEvents_MostRecentFirst", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-order-history-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Order History Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		middle := seedEvent(t, ctx, calendarRepo, ns, "middle", now.Add(-48*time.Hour), now.Add(-47*time.Hour), []uuid.UUID{contactID}, nil)
		newest := seedEvent(t, ctx, calendarRepo, ns, "newest", now.Add(-24*time.Hour), now.Add(-23*time.Hour), []uuid.UUID{contactID}, nil)
		oldest := seedEvent(t, ctx, calendarRepo, ns, "oldest", now.Add(-72*time.Hour), now.Add(-71*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 3)
		assert.Equal(t,
			[]string{newest.ID.String(), middle.ID.String(), oldest.ID.String()},
			rawEventIDs(t, envelope.Data),
			"meeting history must be ordered most-recent-first")
	})

	t.Run("UpcomingForContact_SoonestFirst", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-order-upcoming-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Order Upcoming Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		middle := seedEvent(t, ctx, calendarRepo, ns, "middle", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID}, nil)
		latest := seedEvent(t, ctx, calendarRepo, ns, "latest", now.Add(72*time.Hour), now.Add(73*time.Hour), []uuid.UUID{contactID}, nil)
		soonest := seedEvent(t, ctx, calendarRepo, ns, "soonest", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/contacts/"+contactID.String()+"/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 3)
		assert.Equal(t,
			[]string{soonest.ID.String(), middle.ID.String(), latest.ID.String()},
			rawEventIDs(t, envelope.Data),
			"a contact's upcoming events must be ordered soonest-first")
	})

	t.Run("GlobalUpcoming_SoonestFirst", func(t *testing.T) {
		ctx := context.Background()
		router, calendarRepo, seedContact := newCalendarAPITest(t, ctx)

		ns := "cal-order-global-" + uuid.NewString()[:8]
		contactID := seedContact("Calendar Order Global Test Contact " + ns)
		now := accelerated.GetCurrentTime()
		middle := seedEvent(t, ctx, calendarRepo, ns, "middle", now.Add(48*time.Hour), now.Add(49*time.Hour), []uuid.UUID{contactID}, nil)
		latest := seedEvent(t, ctx, calendarRepo, ns, "latest", now.Add(72*time.Hour), now.Add(73*time.Hour), []uuid.UUID{contactID}, nil)
		soonest := seedEvent(t, ctx, calendarRepo, ns, "soonest", now.Add(24*time.Hour), now.Add(25*time.Hour), []uuid.UUID{contactID}, nil)

		w := doCalendarGet(router, "/api/v1/events/upcoming")
		require.Equal(t, http.StatusOK, w.Code)

		var envelope calendarRawEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Len(t, envelope.Data, 3)
		assert.Equal(t,
			[]string{soonest.ID.String(), middle.ID.String(), latest.ID.String()},
			rawEventIDs(t, envelope.Data),
			"the global upcoming feed must be ordered soonest-first")
	})
}
