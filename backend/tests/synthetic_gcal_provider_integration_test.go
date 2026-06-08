package tests

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	calendarapi "google.golang.org/api/calendar/v3"
)

// These GCal PROVIDER-level integration tests drive the REAL
// CalendarSyncProvider.Sync through the Element-1 harness (real bus + real DB +
// River settle) across the multi-call sync flows the single-event ReplayGCal
// smoke and the off-mode fetcher-seam unit tests do not reach: incremental via
// stored sync token, 410/fullSyncRequired fallback, unmatched-attendee import
// candidate, decline cleanup through the real CalendarDeclineHandler consumer,
// and initial-window idempotency. Slow-gated (TestSynthetic prefix +
// RequireLongTests); each sub-test gets a unique namespace.

// cancelledStubFor returns a minimal cancelled delta for an existing event id —
// the shape Google sends on an incremental sync when the user declines/removes
// an event (no Start/End DateTime required by the remove branch).
func cancelledStubFor(gcalEventID string) *calendarapi.Event {
	return &calendarapi.Event{Id: gcalEventID, Status: "cancelled"}
}

func TestSyntheticGCalProvider_InitialSync_MatchedAttendeeSettled(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	ev := gen.GCalEvent(spec, factory.MatchSeeded)
	sess := h.NewGCalSyncSession(ev.AccountID)
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev.Event, "synth-cursor-1")))
	require.NoError(t, sess.SettleMatched(ctx, ev.GcalEventID, contact.ID))

	// Exactly one gcal interaction for the contact (deeper than the E1 smoke).
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"))
	// The provider recorded the returned sync token as the next cursor.
	require.Equal(t, "synth-cursor-1", sess.Cursor())
}

func TestSyntheticGCalProvider_IncrementalSync_SecondEventSettles(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	ev1 := gen.GCalEvent(spec, factory.MatchSeeded)
	ev2 := gen.GCalEvent(spec, factory.MatchSeeded)
	sess := h.NewGCalSyncSession(ev1.AccountID)

	// Sync 1: initial window delivers ev1.
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev1.Event, "synth-cursor-a")))
	require.NoError(t, sess.SettleMatched(ctx, ev1.GcalEventID, contact.ID))

	// Sync 2: incremental (the provider passes the stored sync token) delivers a
	// NEW past event for the same contact.
	require.NoError(t, sess.Sync(ctx, oneEventIncrementalOnly(ev2.Event, "synth-cursor-b")))
	require.NoError(t, sess.SettleMatched(ctx, ev2.GcalEventID, contact.ID))

	// Two distinct gcal interactions, no duplicate of the first; cursor advanced.
	require.Equal(t, 2, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"))
	require.Equal(t, "synth-cursor-b", sess.Cursor())
}

func TestSyntheticGCalProvider_Incremental410_FallsBackAndSettles(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	ev1 := gen.GCalEvent(spec, factory.MatchSeeded)
	ev2 := gen.GCalEvent(spec, factory.MatchSeeded)
	sess := h.NewGCalSyncSession(ev1.AccountID)

	// Sync 1: initial window delivers ev1, records a cursor.
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev1.Event, "synth-cursor-x")))
	require.NoError(t, sess.SettleMatched(ctx, ev1.GcalEventID, contact.ID))

	// Sync 2: the sync-token (incremental) call returns 410/fullSyncRequired; the
	// provider falls back to the initial window, which serves ev2.
	require.NoError(t, sess.Sync(ctx, func() google.FakeCalendarFetcherFuncs {
		return google.FakeCalendarFetcherFuncs{
			ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
				if opts.SyncToken != "" {
					return nil, "", "", errors.New("googleapi: Error 410: Sync token is no longer valid, fullSyncRequired")
				}
				return []*calendarapi.Event{ev2.Event}, "", "synth-cursor-y", nil
			},
		}
	}()))
	require.NoError(t, sess.SettleMatched(ctx, ev2.GcalEventID, contact.ID))

	require.Equal(t, 2, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"))
	require.Equal(t, "synth-cursor-y", sess.Cursor(), "fallback must record a fresh cursor")
}

func TestSyntheticGCalProvider_UnmatchedAttendee_ImportCandidate(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	// Unknown attendee → no matched contact; the provider stores an import
	// candidate keyed by the (synthetic, ns-prefixed) attendee email.
	ev := gen.GCalEvent(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
	sess := h.NewGCalSyncSession(ev.AccountID)
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev.Event, "synth-cursor-u")))
	require.NoError(t, sess.SettleUnmatched(ctx, ev.GcalEventID))

	n, err := sess.CountImportCandidateByEmailPrefix(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, int64(1), "unmatched attendee must produce an external_contact import candidate")
}

func TestSyntheticGCalProvider_DeclineCleanup_ThroughRealConsumer(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	ev := gen.GCalEvent(spec, factory.MatchSeeded)
	sess := h.NewGCalSyncSession(ev.AccountID)

	// Sync 1: store + settle the matched event (interaction recorded).
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev.Event, "synth-cursor-d1")))
	require.NoError(t, sess.SettleMatched(ctx, ev.GcalEventID, contact.ID))
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"))

	// Sync 2: the same event arrives as a cancelled stub on the incremental call.
	// The cutover decline branch deletes the calendar_event + publishes
	// calendar.declined; the real CalendarDeclineHandler consumer soft-deletes
	// the derived interaction.
	require.NoError(t, sess.Sync(ctx, func() google.FakeCalendarFetcherFuncs {
		return google.FakeCalendarFetcherFuncs{
			ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
				return []*calendarapi.Event{cancelledStubFor(ev.GcalEventID)}, "", "synth-cursor-d2", nil
			},
		}
	}()))
	require.NoError(t, sess.SettleDeclined(ctx, ev.GcalEventID, contact.ID))

	// Terminal state: no calendar_event row, and no live gcal interaction.
	require.Equal(t, 0, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"),
		"the decline consumer must soft-delete the derived gcal interaction")
}

func TestSyntheticGCalProvider_IdempotentInitialReplay(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	ev := gen.GCalEvent(spec, factory.MatchSeeded)
	sess := h.NewGCalSyncSession(ev.AccountID)

	// Replay the SAME initial window twice (empty cursor both times — a fresh
	// session each Sync would also start empty, but reusing one session and
	// re-issuing the initial window is the no-op-on-replay assertion).
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev.Event, "")))
	require.NoError(t, sess.SettleMatched(ctx, ev.GcalEventID, contact.ID))
	require.NoError(t, sess.Sync(ctx, oneEventInitialOnly(ev.Event, "")))
	require.NoError(t, sess.SettleMatched(ctx, ev.GcalEventID, contact.ID))

	// Stable gcal_event_id dedup at the event-upsert + interaction-dedup level.
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "gcal"),
		"replaying the same initial window must not duplicate the event's interaction")
}

// --- fetcher behavior helpers ----------------------------------------------

// oneEventInitialOnly serves the event only on the INITIAL-window call (no
// SyncToken) and returns syncToken as the new cursor. An incremental (SyncToken)
// call serves nothing — used where only an initial sync should land the event.
func oneEventInitialOnly(event *calendarapi.Event, syncToken string) google.FakeCalendarFetcherFuncs {
	return google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			if opts.SyncToken != "" {
				return nil, "", syncToken, nil
			}
			return []*calendarapi.Event{event}, "", syncToken, nil
		},
	}
}

// oneEventIncrementalOnly serves the event only on the INCREMENTAL call (a
// non-empty SyncToken) and returns syncToken as the new cursor.
func oneEventIncrementalOnly(event *calendarapi.Event, syncToken string) google.FakeCalendarFetcherFuncs {
	return google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			if opts.SyncToken == "" {
				return nil, "", syncToken, nil
			}
			return []*calendarapi.Event{event}, "", syncToken, nil
		},
	}
}

// countInteractionsBySource counts the contact's LIVE interactions with the
// given source (ListContactInteractions filters soft-deleted rows).
func countInteractionsBySource(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, source string) int {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 200, 0)
	require.NoError(t, err)
	n := 0
	for _, r := range rows {
		if r.Source == source {
			n++
		}
	}
	return n
}
