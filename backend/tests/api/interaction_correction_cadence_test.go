package api

import (
	"net/http"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
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
		// spec: CAD-047.manual-recording-order-kept
		a := e.contact(t, "monthly")
		removed := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-10))
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(5))
		e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionInbound, e.at(2))
		before := e.fields(t, a)
		require.Equal(t, rfc3339(e.at(2)), *before.LastContacted, "the manual entry set last_contacted back")

		e.remove(t, removed, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, before.LastContacted, e.fields(t, a).LastContacted)
	})

	t.Run("a CRM skip made after the removed observation keeps its contact_by", func(t *testing.T) {
		// spec: CAD-047.hand-set-value-kept
		a := e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
		skipped := e.skip(t, a)

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, skipped.ContactBy, e.fields(t, a).ContactBy)
	})

	t.Run("a skip in effect is withdrawn when the correction moves last_contacted", func(t *testing.T) {
		// spec: CAD-047.skip-withdrawn-when-clock-moves
		a := e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		latest := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
		require.True(t, e.skip(t, a).UndoSkipAvailable)

		e.remove(t, latest, removalBody(service.RemovalReasonDidNotHappen, a))

		got := e.fields(t, a)
		require.False(t, got.UndoSkipAvailable)
		require.Nil(t, got.LastSkippedAt)
	})

	t.Run("a skip in effect stays when the correction moves neither last_contacted nor contact_by", func(t *testing.T) {
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

// skip posts a CRM skip for the contact and returns its fields after it.
func (e *correctionEnv) skip(t *testing.T, contactID uuid.UUID) contactFields {
	t.Helper()
	w := e.post(t, "/api/v1/contacts/"+contactID.String()+"/skip", map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return e.fields(t, contactID)
}
