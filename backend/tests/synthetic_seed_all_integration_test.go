package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestSyntheticSeedAll exercises the mode-(b) SeedAll: it builds, settles, and
// the harness cleanup empties its namespaced rows. Slow-gated (TestSynthetic
// name prefix routes it into the nightly slow suite via BACKEND_SLOW_TESTS_REGEX).
func TestSyntheticSeedAll(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params := synthetic.DefaultParams()
	params.Namespace = syntheticNS(t) // unique per run for shared-DB isolation

	// Construct the harness for the SAME namespace so identifiers + cleanup align.
	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)

	res, err := synthetic.SeedAll(ctx, h, params)
	require.NoError(t, err)
	require.NotEmpty(t, res.GmailContactIDs)
	require.NotEmpty(t, res.TelegramContactIDs)

	// The seeded contacts exist before cleanup.
	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "SeedAll should have created contacts")

	// Idempotency: re-replaying SeedAll's captured Gmail source payload (stable
	// source-ids) must NOT create a duplicate comms_message row. SeedAll's
	// contact creation is not upsert-idempotent, so the idempotency contract is
	// at the source-message level — this re-replays the exact same payload.
	require.NotNil(t, res.GmailIdempotencyProbe)
	probe := res.GmailIdempotencyProbe
	rowsBefore, err := h.CommsRepo().ListByContact(ctx, probe.ContactID)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, probe.ContactID, probe.Spec)
	require.NoError(t, err)
	rowsAfter, err := h.CommsRepo().ListByContact(ctx, probe.ContactID)
	require.NoError(t, err)
	require.Equal(t, len(rowsBefore), len(rowsAfter), "re-replaying the same Gmail payload must not add a duplicate row")
}

// TestSyntheticSeedAll_CleanupEmptiesNamespace proves the harness's cleanup
// closure removes the namespace's contacts while leaving a sentinel contact
// seeded by a DIFFERENT namespace intact (non-destructive scoping). It drives
// the teardown explicitly via NewHarnessWithDB so it can assert post-cleanup.
func TestSyntheticSeedAll_CleanupEmptiesNamespace(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Sentinel namespace: seed a contact + settled replay, and DO NOT tear it
	// down until the end, so we can prove the target's cleanup leaves it alone.
	sentinel := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 12345)
	sgen := sentinel.Generator()
	sspec := sgen.Contact(factory.WithEmail())
	sentinelContact, err := sentinel.SeedContact(ctx, sspec)
	require.NoError(t, err)

	// Target namespace via the explicit-teardown constructor so we can run the
	// closure and then assert.
	target, teardown, err := synthetic.NewHarnessWithDB(ctx, database)
	require.NoError(t, err)
	tgen := target.Generator()
	tspec := tgen.Contact(factory.WithEmail())
	targetContact, err := target.SeedContact(ctx, tspec)
	require.NoError(t, err)
	_, err = target.ReplayGmail(ctx, targetContact.ID, tgen.GmailMessage(tspec, factory.MatchSeeded))
	require.NoError(t, err)

	// The target's Gmail replay drives the real recorder, which mints a venue node
	// (interaction.venue_id). Scoped to THIS run's tracked venue node ids so the
	// check is parallel-test-safe. Non-vacuous precondition: at least one exists.
	venueBefore, err := target.VenueNodesRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, venueBefore, int64(0), "the Gmail replay must mint a venue node")

	// Run the target's teardown (quiesce + Gate-B-gated cleanup).
	require.NoError(t, teardown(context.Background()))

	// The target's contacts are gone.
	gone, err := target.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "target namespace cleanup should remove its contacts")

	// The target's venue nodes are gone too — no leak (the exact failure class the
	// teardown's venue_node step exists to prevent).
	venueAfter, err := target.VenueNodesRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), venueAfter, "teardown must remove the venue nodes the replay created (no leak)")

	// The sentinel's contact survives (non-destructive scoping).
	surviving, err := sentinel.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, surviving, int64(0), "a different namespace's contact must survive the target's cleanup")
	_ = sentinelContact
}

// TestSyntheticTodoistReplay_DoesNotTouchOtherNamespace proves the Todoist replay
// is namespace-scoped: a replay in namespace A does NOT create or mutate a
// contact_task on a cadence-bearing sentinel contact seeded in namespace B, even
// though the provider's reconcile lists ALL cadence-bearing contacts DB-wide.
// Mirrors the cross-namespace cleanup guard.
func TestSyntheticTodoistReplay_DoesNotTouchOtherNamespace(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// Namespace B: a cadence-bearing sentinel contact (eligible for the Todoist
	// reconcile's global ListContactsWithContactBy enumeration).
	sentinel := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 24680)
	sgen := sentinel.Generator()
	sentinelContact, err := sentinel.SeedContact(ctx, sgen.Contact(factory.WithEmail(), factory.WithCadence("weekly")))
	require.NoError(t, err)

	// No contact_task on the sentinel before the other namespace's replay.
	pre, err := contactTaskRepo.ListContactTasksByContact(ctx, sentinelContact.ID)
	require.NoError(t, err)
	require.Empty(t, pre, "sentinel contact should have no contact_task before the replay")

	// Namespace A: seed its own cadence contact + run a Todoist replay.
	a := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 13579)
	agen := a.Generator()
	aContact, err := a.SeedContact(ctx, agen.Contact(factory.WithEmail(), factory.WithCadence("weekly")))
	require.NoError(t, err)
	_, err = a.ReplayTodoist(ctx, []uuid.UUID{aContact.ID})
	require.NoError(t, err)

	// The sentinel (namespace B) contact must be untouched — no contact_task
	// created or mutated by namespace A's replay.
	post, err := contactTaskRepo.ListContactTasksByContact(ctx, sentinelContact.ID)
	require.NoError(t, err)
	require.Empty(t, post, "namespace A's Todoist replay must not create a contact_task on namespace B's contact")
}

// TestSyntheticGCalReplay_DoesNotTouchOtherNamespace proves the GCal replay is
// namespace-scoped: a replay in namespace A does NOT read, mark, or publish for a
// past calendar_event seeded in namespace B, even though the provider's
// updateLastContactedForPastEvents lists ALL past confirmed events DB-wide via
// ListPastEventsNeedingUpdate. Mirrors the Todoist cross-namespace guard.
func TestSyntheticGCalReplay_DoesNotTouchOtherNamespace(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// Namespace B: a contact + a past, confirmed calendar_event with the contact
	// matched and last_contacted_updated=FALSE — i.e. eligible for the global
	// ListPastEventsNeedingUpdate enumeration but NOT yet processed. Seeded
	// directly (not via ReplayGCal, which would settle/process it). The
	// ns-prefixed gcal_event_id keeps the row inside namespace B's cleanup scope.
	sentinel := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 35791)
	sgen := sentinel.Generator()
	sentinelContact, err := sentinel.SeedContact(ctx, sgen.Contact(factory.WithEmail()))
	require.NoError(t, err)

	now := accelerated.GetCurrentTime()
	sentinelGcalID := sgen.Prefix() + "gcal-sentinel"
	_, err = calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:          sentinelGcalID,
		GcalCalendarID:       "primary",
		GoogleAccountID:      sgen.Prefix() + "account",
		StartTime:            now.Add(-2 * time.Hour),
		EndTime:              now.Add(-1 * time.Hour),
		Status:               "confirmed",
		Attendees:            []repository.Attendee{},
		MatchedContactIDs:    []uuid.UUID{sentinelContact.ID},
		SyncedAt:             now,
		LastContactedUpdated: false,
	})
	require.NoError(t, err)

	// The sentinel is unprocessed before namespace A's replay.
	pre, err := support.CountProcessedCalendarEventByGcalID(ctx, sentinelGcalID, sentinelContact.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), pre, "sentinel event should be unprocessed before the replay")

	// Namespace A: seed its own email contact + run a GCal replay.
	a := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 97531)
	agen := a.Generator()
	aSpec := agen.Contact(factory.WithEmail())
	aContact, err := a.SeedContact(ctx, aSpec)
	require.NoError(t, err)
	res, err := a.ReplayGCal(ctx, aContact.ID, agen.GCalEvent(aSpec, factory.MatchSeeded))
	require.NoError(t, err)
	require.True(t, res.Matched)

	// The sentinel (namespace B) event must be untouched — namespace A's replay
	// must NOT mark it processed (no calendar.attended published for it).
	post, err := support.CountProcessedCalendarEventByGcalID(ctx, sentinelGcalID, sentinelContact.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), post, "namespace A's GCal replay must not process namespace B's past event")
}
