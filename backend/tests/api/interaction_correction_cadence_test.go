package api

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe the cadence fields the correction routes recompute
// through GET /api/v1/contacts/:id. A twin is an otherwise identical contact
// on which the corrected observation was never recorded.

func TestInteractionCorrection_RecomputesCadence(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("removing the latest inbound leaves the fields of a contact that never had it", func(t *testing.T) {
		// spec: CAD-047.recomputed-from-effective-participation
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
			e.call(t, c, e.ref("call"), repository.InteractionDirectionOutbound, e.at(1))
		}
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
		require.NotEqual(t, e.fields(t, twin), e.fields(t, a))

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, e.fields(t, twin), e.fields(t, a))
	})

	t.Run("removing the outbound that set the awaiting-reply window leaves it as if never recorded", func(t *testing.T) {
		// spec: CAD-047.recomputed-from-effective-participation
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
			e.call(t, c, e.ref("call"), repository.InteractionDirectionOutbound, e.at(1))
		}
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(30))
		before := e.fields(t, a)
		require.True(t, before.AwaitingReply)

		e.remove(t, latest, removalBody(service.RemovalReasonDuplicate, a))
		require.Equal(t, e.fields(t, twin), e.fields(t, a))

		only, onlyTwin := e.contact(t, "monthly"), e.contact(t, "monthly")
		e.call(t, onlyTwin, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		e.call(t, only, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		outreach := e.call(t, only, e.ref("call"), repository.InteractionDirectionOutbound, e.at(30))
		e.remove(t, outreach, removalBody(service.RemovalReasonDuplicate, only))
		got := e.fields(t, only)
		require.Nil(t, got.AwaitingReplyUntil, "no window when no other outreach remains")
		require.Equal(t, e.fields(t, onlyTwin), got)
	})

	t.Run("removing the only participation returns to the creation state", func(t *testing.T) {
		// spec: CAD-047.recomputed-from-effective-participation
		// spec: CAD-047.due-date-follows-last-contacted
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		only := e.call(t, a, e.ref("call"), repository.InteractionDirectionMutual, e.at(0))

		e.remove(t, only, removalBody(service.RemovalReasonDidNotHappen, a))

		got := e.fields(t, a)
		require.Nil(t, got.LastContacted)
		require.Nil(t, got.LastInteractionAt)
		require.Nil(t, got.LastOutreachAt)
		require.Nil(t, got.LastResponseAt)
		require.Equal(t, e.fields(t, twin), got, "contact_by derives from the contact's creation")
	})

	t.Run("a manual entry backdated before a later automatic participation keeps its last_contacted", func(t *testing.T) {
		// spec: CAD-047.recording-order-kept
		a := e.contact(t, "monthly")
		removed := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-10))
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(5))
		e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionInbound, e.at(2))
		before := e.fields(t, a)
		require.Equal(t, rfc3339(e.at(2)), *before.LastContacted, "the manual entry set last_contacted back")

		e.remove(t, removed, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, before.LastContacted, e.fields(t, a).LastContacted)
	})

	t.Run("an automatic extension recorded after a backdated manual entry keeps last_contacted at the extension's time", func(t *testing.T) {
		// spec: CAD-047.recording-order-kept
		a := e.contact(t, "monthly")
		unrelated := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-20*24))
		automatic := e.record(t, a, repository.InteractionSourcePhoneCalls, e.ref("call"), repository.InteractionDirectionInbound, e.at(-3*24))
		e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionInbound, e.at(-6*24))
		require.NoError(t, e.contacts.ExtendInteraction(e.ctx, automatic.ID, a, repository.InteractionDirectionInbound, e.at(-24), nil))
		before := e.fields(t, a)
		require.Equal(t, rfc3339(e.at(-24)), *before.LastContacted, "the extension moved last_contacted forward")

		e.remove(t, unrelated, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, before, e.fields(t, a))
	})

	t.Run("a cadence-frequency change made after the removed observation sets no floor", func(t *testing.T) {
		// spec: CAD-047.direct-due-date-floor
		a, twin := e.contact(t, "monthly"), e.contact(t, "quarterly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*24))
		}
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		changed := e.setCadence(t, a, "quarterly")

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		got := e.fields(t, a)
		require.Equal(t, e.fields(t, twin), got, "contact_by derives from the recomputed last_contacted under the new frequency")
		require.Less(t, *got.ContactBy, *changed.ContactBy, "the date the cadence change derived is no floor")
	})

	t.Run("a cadence-frequency change whose derived date equals the replayed date sets no floor", func(t *testing.T) {
		// spec: CAD-047.direct-due-date-floor
		// contact_by is a date, so two inbounds on one day derive one date.
		day := e.base.Truncate(24*time.Hour).AddDate(0, 0, -10)
		a, sameDayTwin, olderTwin := e.contact(t, "monthly"), e.contact(t, "quarterly"), e.contact(t, "quarterly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, day.AddDate(0, 0, -40))
		sameDay := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, day.Add(time.Hour))
		e.call(t, sameDayTwin, e.ref("call"), repository.InteractionDirectionInbound, day.AddDate(0, 0, -40))
		e.call(t, sameDayTwin, e.ref("call"), repository.InteractionDirectionInbound, day.Add(time.Hour))
		e.call(t, olderTwin, e.ref("call"), repository.InteractionDirectionInbound, day.AddDate(0, 0, -40))
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, day.Add(20*time.Hour))
		changed := e.setCadence(t, a, "quarterly")

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		got := e.fields(t, a)
		require.NotEqual(t, changed.LastContacted, got.LastContacted, "the removal moved last_contacted")
		require.Equal(t, changed.ContactBy, got.ContactBy, "the date the cadence change derived equals the replayed date")
		require.Equal(t, e.fields(t, sameDayTwin), got)

		e.remove(t, sameDay, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, e.fields(t, olderTwin), e.fields(t, a), "the cadence change left no floor for a later correction")
	})

	t.Run("a skip made after the removed observation sets no floor and is withdrawn when the correction moves last_contacted", func(t *testing.T) {
		// spec: CAD-047.skip-withdrawn-when-clock-moves
		// spec: CAD-047.direct-due-date-floor
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		}
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
		require.True(t, e.skip(t, a).UndoSkipAvailable)

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		got := e.fields(t, a)
		require.False(t, got.UndoSkipAvailable)
		require.Nil(t, got.LastSkippedAt)
		require.Equal(t, e.fields(t, twin).ContactBy, got.ContactBy, "contact_by derives from the recomputed last_contacted")
	})

	t.Run("a skip in effect stays when the correction leaves last_contacted unchanged", func(t *testing.T) {
		// spec: CAD-047.skip-kept-when-clock-unmoved
		a := e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
		outbound := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(0))
		skipped := e.skip(t, a)
		require.True(t, skipped.UndoSkipAvailable)

		e.remove(t, outbound, removalBody(service.RemovalReasonDuplicate, a))

		got := e.fields(t, a)
		require.True(t, got.UndoSkipAvailable)
		require.Equal(t, skipped.LastSkippedAt, got.LastSkippedAt)
		require.Equal(t, skipped.ContactBy, got.ContactBy)
	})

	for _, direction := range []string{repository.InteractionDirectionInbound, repository.InteractionDirectionMutual} {
		t.Run("after a skip ended the awaiting-reply window, removing a later "+direction+" participation does not reopen it", func(t *testing.T) {
			// spec: CAD-047.skip-ended-window-stays-ended
			a := e.contact(t, "monthly")
			e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(-24))
			require.True(t, e.fields(t, a).AwaitingReply)
			require.Nil(t, e.skip(t, a).AwaitingReplyUntil, "the skip ended the window")
			response := e.call(t, a, e.ref("call"), direction, e.at(0))

			e.remove(t, response, removalBody(service.RemovalReasonDidNotHappen, a))

			got := e.fields(t, a)
			require.Nil(t, got.LastResponseAt, "the outbound is unanswered again")
			require.Nil(t, got.AwaitingReplyUntil)
			require.False(t, got.AwaitingReply)
		})
	}

	t.Run("removing one attendee leaves every other attendee's fields unchanged", func(t *testing.T) {
		// spec: CAD-047.only-affected-contacts-recomputed
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(3))
		e.meeting(t, b, ev, e.at(3))
		e.call(t, b, e.ref("call"), repository.InteractionDirectionOutbound, e.at(4))
		before := e.fields(t, b)

		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, a))

		require.Equal(t, before, e.fields(t, b))
	})

	t.Run("merging an inbound with a later outbound leaves the fields of one mutual participation at the later time", func(t *testing.T) {
		// spec: CAD-047.merge-recomputes-observed-contacts
		a, earlyOnly, twin := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
		inRef := e.ref("call")
		inbound := e.call(t, a, inRef, repository.InteractionDirectionInbound, e.at(0))
		e.call(t, earlyOnly, inRef, repository.InteractionDirectionInbound, e.at(0))
		outbound := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(3))
		e.call(t, twin, e.ref("call"), repository.InteractionDirectionMutual, e.at(3))
		earlyBefore := e.fields(t, earlyOnly)

		e.merge(t, a, inbound, outbound)

		require.Equal(t, e.fields(t, twin), e.fields(t, a))
		require.Equal(t, earlyBefore, e.fields(t, earlyOnly))
	})
}

// TestInteractionCorrection_ComposesWithDueDatesSetDirectly makes Todoist
// deadline edits and skips through the real cadence sync, which reconciles
// every contact, so it runs on a database it owns.
func TestInteractionCorrection_ComposesWithDueDatesSetDirectly(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, cfg := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)
	todoistSync := newCadenceSync(e, cfg)
	day := func(contactBy *string) time.Time {
		d, err := time.Parse(todoist.DateFormat, (*contactBy)[:10])
		require.NoError(t, err)
		return d
	}

	// removableLatest gives a contact an older inbound and a latest inbound,
	// and a twin with the older inbound only. It returns the latest.
	removableLatest := func(t *testing.T) (a, twin, latest uuid.UUID) {
		a, twin = e.contact(t, "monthly"), e.contact(t, "monthly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*24))
		}
		return a, twin, e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
	}

	t.Run("a Todoist deadline edit made after the removed observation is a floor when the removal moves last_contacted", func(t *testing.T) {
		// spec: CAD-047.direct-due-date-floor
		// spec: CAD-047.due-date-follows-last-contacted
		later, laterTwin, laterLatest := removableLatest(t)
		earlier, earlierTwin, earlierLatest := removableLatest(t)
		derived := day(e.fields(t, laterTwin).ContactBy)
		laterDeadline, earlierDeadline := derived.AddDate(0, 0, 45), derived.AddDate(0, 0, -5)
		todoistSync.editDeadline(t, later, laterDeadline)
		todoistSync.editDeadline(t, earlier, earlierDeadline)
		require.Equal(t, laterDeadline, day(e.fields(t, later).ContactBy))
		require.Equal(t, earlierDeadline, day(e.fields(t, earlier).ContactBy))

		e.remove(t, laterLatest, removalBody(service.RemovalReasonDidNotHappen, later))
		e.remove(t, earlierLatest, removalBody(service.RemovalReasonDidNotHappen, earlier))

		require.Equal(t, laterDeadline, day(e.fields(t, later).ContactBy), "a deadline later than the derived date holds")
		require.Equal(t, e.fields(t, earlierTwin).ContactBy, e.fields(t, earlier).ContactBy, "the derived date beats an earlier deadline")
		require.Equal(t, e.fields(t, laterTwin).LastContacted, e.fields(t, later).LastContacted)
	})

	t.Run("a Todoist deadline moved earlier after an older duplicate stays when the removal leaves last_contacted unchanged", func(t *testing.T) {
		// spec: CAD-047.due-date-follows-last-contacted
		a := e.contact(t, "monthly")
		duplicate := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-10*24))
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		deadline := day(e.fields(t, a).ContactBy).AddDate(0, 0, -7)
		todoistSync.editDeadline(t, a, deadline)
		before := e.fields(t, a)

		e.remove(t, duplicate, removalBody(service.RemovalReasonDuplicate, a))

		require.Equal(t, before, e.fields(t, a))
		require.Equal(t, deadline, day(before.ContactBy))
	})

	t.Run("a Todoist deadline edit made before the removed observation sets no floor", func(t *testing.T) {
		// spec: CAD-047.direct-due-date-floor
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		for _, c := range []uuid.UUID{a, twin} {
			e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*24))
		}
		todoistSync.editDeadline(t, a, day(e.fields(t, twin).ContactBy).AddDate(0, 0, 90))
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, e.fields(t, twin).ContactBy, e.fields(t, a).ContactBy)
	})

	t.Run("a Todoist skip made after the removed observation sets no floor", func(t *testing.T) {
		// spec: CAD-047.direct-due-date-floor
		a, twin, latest := removableLatest(t)
		skippedFrom := e.fields(t, a).ContactBy
		todoistSync.skipInTodoist(t, a)
		require.Greater(t, *e.fields(t, a).ContactBy, *skippedFrom, "the Todoist skip advanced contact_by")

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, e.fields(t, twin).ContactBy, e.fields(t, a).ContactBy)
	})
}

// setCadence changes the contact's cadence frequency through the profile save
// and returns its fields after it.
func (e *correctionEnv) setCadence(t *testing.T, contactID uuid.UUID, frequency string) contactFields {
	t.Helper()
	c, err := e.contactRepo.GetContact(e.ctx, contactID)
	require.NoError(t, err)
	_, err = e.contacts.UpdateContact(e.ctx, contactID, repository.UpdateContactRequest{FullName: c.FullName, Cadence: &frequency})
	require.NoError(t, err)
	return e.fields(t, contactID)
}

// skip posts a CRM skip for the contact and returns its fields after it.
func (e *correctionEnv) skip(t *testing.T, contactID uuid.UUID) contactFields {
	t.Helper()
	w := e.post(t, "/api/v1/contacts/"+contactID.String()+"/skip", map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return e.fields(t, contactID)
}
