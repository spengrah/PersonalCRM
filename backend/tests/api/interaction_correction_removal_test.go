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

// These tests observe POST /api/v1/interactions/:id/removals through effective
// participation (arc seam S4); the later contact merge goes through
// POST /api/v1/contacts/:id/merge.

func TestInteractionRemoval_RemovesSelectedParticipations(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("a scope naming only the current contact removes only that contact", func(t *testing.T) {
		// spec: IXN-018.scope-removes-exactly-selected
		a, b, c := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))
		e.meeting(t, c, ev, e.at(0))
		otherB, otherC := e.participation(t, b), e.participation(t, c)

		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, a))

		require.Empty(t, e.correctable(t, a))
		require.Equal(t, otherB, e.participation(t, b))
		require.Equal(t, otherC, e.participation(t, c))
	})

	t.Run("scope everyone removes every attendee", func(t *testing.T) {
		// spec: IXN-018.everyone-removes-every-attendee
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))

		e.remove(t, node, everyoneBody(service.RemovalReasonDidNotHappen))

		require.Empty(t, e.correctable(t, a))
		require.Empty(t, e.correctable(t, b))
	})

	t.Run("a removal binds to the members it judged, never to one merged in later", func(t *testing.T) {
		// spec: IXN-018.binds-to-judged-members
		alice, bob := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev, callRef := e.event(t), e.ref("call")
		meetingX := e.meeting(t, alice, ev, e.at(0))
		e.meeting(t, bob, ev, e.at(0))
		callY := e.call(t, alice, callRef, repository.InteractionDirectionMutual, e.at(1))

		e.remove(t, meetingX, removalBody(service.RemovalReasonDidNotAttend, alice))
		e.merge(t, bob, meetingX, callY)

		eps := e.correctable(t, alice)
		require.Len(t, eps, 1, "alice is on the merged interaction through Y")
		require.Equal(t, meetingX, *eps[0].InteractionNodeID)
		require.Len(t, eps[0].Members, 1)
		require.Equal(t, callY, *eps[0].Members[0].InteractionNodeID)

		e.remove(t, meetingX, removalBody(service.RemovalReasonDidNotAttend, alice))

		require.Empty(t, e.correctable(t, alice), "removed from Y as well, alice has no participation")
		require.Len(t, e.correctable(t, bob), 1)
	})

	t.Run("the reason never changes what a scope removes", func(t *testing.T) {
		// spec: IXN-018.reason-independent-of-scope
		var results [][]repository.EffectiveParticipation
		for _, reason := range []string{service.RemovalReasonDuplicate, service.RemovalReasonDidNotAttend, service.RemovalReasonDidNotHappen} {
			a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
			ev := e.event(t)
			node := e.meeting(t, a, ev, e.at(0))
			e.meeting(t, b, ev, e.at(0))
			e.meeting(t, a, e.event(t), e.at(2))

			e.remove(t, node, removalBody(reason, a))

			remainingA := e.correctable(t, a)
			require.Len(t, remainingA, 1)
			require.True(t, remainingA[0].OccurredAt.Equal(e.at(2)))
			require.Len(t, e.correctable(t, b), 1, "under %s the attendee the scope does not name keeps its participation", reason)
			results = append(results, remainingA)
		}
		for _, r := range results[1:] {
			require.Equal(t, len(results[0]), len(r))
			require.Equal(t, results[0][0].Direction, r[0].Direction)
		}
	})

	t.Run("a removal on a merged interaction covers every member's source object", func(t *testing.T) {
		// spec: IXN-018.covers-every-member
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		evX, evY := e.event(t), e.event(t)
		nodeX := e.meeting(t, a, evX, e.at(0))
		nodeY := e.meeting(t, b, evY, e.at(1))
		e.meeting(t, b, evX, e.at(0))
		e.merge(t, b, nodeX, nodeY)

		e.remove(t, nodeX, removalBody(service.RemovalReasonDidNotAttend, a))
		require.Empty(t, e.correctable(t, a))

		// A is observed later on Y, a member it had no observation of when judged.
		later := e.record(t, a, repository.InteractionSourceGCal, evY, repository.InteractionDirectionMutual, e.at(1))
		require.Equal(t, nodeY, *later.InteractionNodeID)
		require.Empty(t, e.correctable(t, a))
	})

	t.Run("a removal addressed by a merged-away node's id acts on its merged interaction", func(t *testing.T) {
		// spec: IXN-018.merged-away-id-acts-on-merged
		a := e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		e.merge(t, a, nodeX, nodeY)

		e.remove(t, nodeY, removalBody(service.RemovalReasonDuplicate, a))

		require.Empty(t, e.correctable(t, a))
	})

	t.Run("a removal survives a later contact merge of the removed contact", func(t *testing.T) {
		// spec: IXN-018.survives-contact-merge
		removed, survivor, other := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, removed, ev, e.at(0))
		e.meeting(t, other, ev, e.at(0))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))

		w := e.post(t, "/api/v1/contacts/"+survivor.String()+"/merge", map[string]any{"source_contact_id": removed.String()})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		require.Empty(t, e.correctable(t, survivor))
		require.Len(t, e.correctable(t, other), 1)
	})
}

// participantIDs returns the contact ids of an interaction's effective
// participants.
func participantIDs(t *testing.T, e *correctionEnv, node uuid.UUID) []uuid.UUID {
	t.Helper()
	eps, err := e.interactions.ListInteractionParticipants(e.ctx, node)
	require.NoError(t, err)
	ids := make([]uuid.UUID, len(eps))
	for i, ep := range eps {
		ids[i] = ep.ContactID
	}
	return ids
}
