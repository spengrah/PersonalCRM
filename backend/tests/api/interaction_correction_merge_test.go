package api

import (
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe POST /api/v1/interactions/merge and
// POST /api/v1/interactions/merge/preview through effective participation
// (arc seam S4) and GET /api/v1/contacts/:id.

// memberObservations returns an entry's member observation ids.
func memberObservations(ep repository.EffectiveParticipation) []uuid.UUID {
	ids := make([]uuid.UUID, len(ep.Members))
	for i, m := range ep.Members {
		ids[i] = m.ObservationID
	}
	return ids
}

func TestInteractionMerge_DeclaresOneInteraction(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("duplicate calendar interactions give each attendee one participation under the winner", func(t *testing.T) {
		// spec: IXN-016.one-participation-per-attendee
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		x, y := e.event(t), e.event(t)
		nodeX := e.meeting(t, a, x, e.at(0))
		e.meeting(t, b, x, e.at(0))
		nodeY := e.meeting(t, a, y, e.at(0))
		e.meeting(t, b, y, e.at(0))

		answer := e.merge(t, a, nodeX, nodeY)

		require.Equal(t, nodeX, answer.InteractionID)
		for _, contactID := range []uuid.UUID{a, b} {
			eps := e.correctable(t, contactID)
			require.Len(t, eps, 1)
			require.Equal(t, nodeX, *eps[0].InteractionNodeID)
			require.Len(t, eps[0].Members, 2)
		}
	})

	t.Run("an attendee observed in only one member keeps that member's participation", func(t *testing.T) {
		// spec: IXN-016.own-members-only
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		x, y := e.event(t), e.event(t)
		nodeX := e.meeting(t, a, x, e.at(0))
		nodeY := e.meeting(t, a, y, e.at(2))
		onlyY := e.record(t, b, repository.InteractionSourceGCal, y, repository.InteractionDirectionMutual, e.at(2))

		e.merge(t, a, nodeX, nodeY)

		eps := e.correctable(t, b)
		require.Len(t, eps, 1)
		require.Equal(t, nodeX, *eps[0].InteractionNodeID)
		require.Equal(t, []uuid.UUID{onlyY.ID}, memberObservations(eps[0]))
		require.Equal(t, repository.InteractionDirectionMutual, eps[0].Direction)
		require.True(t, eps[0].OccurredAt.Equal(e.at(2)))
	})

	t.Run("a contact with no observation in any member gains no participation and no cadence change", func(t *testing.T) {
		// spec: IXN-016.no-observation-no-participation
		a, outsider := e.contact(t, "monthly"), e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		e.meeting(t, outsider, e.event(t), e.at(-5))
		before, fieldsBefore := e.participation(t, outsider), e.fields(t, outsider)

		e.merge(t, a, nodeX, nodeY)

		require.Equal(t, before, e.participation(t, outsider))
		require.Equal(t, fieldsBefore, e.fields(t, outsider))
	})

	t.Run("a merged interaction merged again with a third gives one participation across all three", func(t *testing.T) {
		// spec: IXN-016.remerge-counts-once
		a := e.contact(t, "monthly")
		x, y, z := e.event(t), e.event(t), e.event(t)
		nodeX := e.meeting(t, a, x, e.at(0))
		nodeY := e.meeting(t, a, y, e.at(1))
		nodeZ := e.meeting(t, a, z, e.at(2))

		e.merge(t, a, nodeX, nodeY)
		answer := e.merge(t, a, nodeZ, nodeX)

		require.Equal(t, nodeZ, answer.InteractionID)
		eps := e.correctable(t, a)
		require.Len(t, eps, 1)
		require.Equal(t, nodeZ, *eps[0].InteractionNodeID)
		require.Len(t, eps[0].Members, 3)
	})

	t.Run("an id merged away acts on its merged interaction", func(t *testing.T) {
		// spec: IXN-016.merged-away-id-acts-on-merged
		a := e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		nodeZ := e.meeting(t, a, e.event(t), e.at(2))
		e.merge(t, a, nodeX, nodeY)

		answer := e.merge(t, a, nodeY, nodeZ)

		require.Equal(t, nodeX, answer.InteractionID, "merged-away Y names the interaction rooted at X")
		eps := e.correctable(t, a)
		require.Len(t, eps, 1)
		require.Equal(t, nodeX, *eps[0].InteractionNodeID)
		require.Len(t, eps[0].Members, 3)
	})

	t.Run("the answer's interaction_id is the root of the first listed id", func(t *testing.T) {
		// spec: IXN-016.answer-is-first-root
		a := e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		nodeZ := e.meeting(t, a, e.event(t), e.at(2))
		e.merge(t, a, nodeY, nodeZ)

		answer := e.merge(t, a, nodeZ, nodeX)

		require.Equal(t, nodeY, answer.InteractionID)
	})
}

func TestInteractionMerge_IsAtomicPerContact(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// mergedAs merges two calls of contact a, the first observed in first at
	// hour 0 and the second in second at hour 3, and returns the answer and
	// a's resulting participation.
	mergedAs := func(t *testing.T, first, second string) (mergeAnswer, repository.EffectiveParticipation) {
		t.Helper()
		a := e.contact(t, "monthly")
		one := e.call(t, a, e.ref("call"), first, e.at(0))
		two := e.call(t, a, e.ref("call"), second, e.at(3))
		answer := e.merge(t, a, one, two)
		eps := e.correctable(t, a)
		require.Len(t, eps, 1)
		return answer, eps[0]
	}

	t.Run("an inbound and a later outbound become one mutual participation at the later time", func(t *testing.T) {
		// spec: IXN-017.mixed-directions-mutual-at-latest
		answer, ep := mergedAs(t, repository.InteractionDirectionInbound, repository.InteractionDirectionOutbound)
		require.Equal(t, repository.InteractionDirectionMutual, ep.Direction)
		require.True(t, ep.OccurredAt.Equal(e.at(3)))
		require.Equal(t, repository.InteractionDirectionMutual, answer.Participation.Direction)
		require.True(t, answer.Participation.OccurredAt.Equal(e.at(3)))
	})

	t.Run("an outbound and a later inbound become one mutual participation at the later time", func(t *testing.T) {
		// spec: IXN-017.mixed-directions-mutual-at-latest
		answer, ep := mergedAs(t, repository.InteractionDirectionOutbound, repository.InteractionDirectionInbound)
		require.Equal(t, repository.InteractionDirectionMutual, ep.Direction)
		require.True(t, ep.OccurredAt.Equal(e.at(3)))
		require.Equal(t, repository.InteractionDirectionMutual, answer.Participation.Direction)
	})

	t.Run("two inbound members stay one inbound participation at the later time", func(t *testing.T) {
		// spec: IXN-017.shared-direction-kept
		answer, ep := mergedAs(t, repository.InteractionDirectionInbound, repository.InteractionDirectionInbound)
		require.Equal(t, repository.InteractionDirectionInbound, ep.Direction)
		require.True(t, ep.OccurredAt.Equal(e.at(3)))
		require.Equal(t, repository.InteractionDirectionInbound, answer.Participation.Direction)
		require.True(t, answer.Participation.OccurredAt.Equal(e.at(3)))
	})

	t.Run("a mutual member and a later inbound member become one mutual participation at the later time", func(t *testing.T) {
		// spec: IXN-017.mutual-member-makes-mutual
		answer, ep := mergedAs(t, repository.InteractionDirectionMutual, repository.InteractionDirectionInbound)
		require.Equal(t, repository.InteractionDirectionMutual, ep.Direction)
		require.True(t, ep.OccurredAt.Equal(e.at(3)))
		require.Equal(t, repository.InteractionDirectionMutual, answer.Participation.Direction)
	})

	t.Run("a contact present only in the earlier member keeps that member's direction and time", func(t *testing.T) {
		// spec: IXN-017.no-inherited-member
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		x, y := e.ref("call"), e.ref("call")
		nodeX := e.call(t, a, x, repository.InteractionDirectionInbound, e.at(0))
		e.call(t, b, x, repository.InteractionDirectionInbound, e.at(0))
		nodeY := e.call(t, a, y, repository.InteractionDirectionOutbound, e.at(3))

		answer := e.merge(t, b, nodeX, nodeY)

		eps := e.correctable(t, b)
		require.Len(t, eps, 1)
		require.Equal(t, repository.InteractionDirectionInbound, eps[0].Direction)
		require.True(t, eps[0].OccurredAt.Equal(e.at(0)))
		require.Equal(t, repository.InteractionDirectionInbound, answer.Participation.Direction)
		require.True(t, answer.Participation.OccurredAt.Equal(e.at(0)))
	})

	t.Run("the preview's answer equals the merge's answer for the same body", func(t *testing.T) {
		// spec: IXN-017.preview-equals-merge
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		x, y := e.ref("call"), e.ref("call")
		nodeX := e.call(t, a, x, repository.InteractionDirectionOutbound, e.at(0))
		e.call(t, b, x, repository.InteractionDirectionInbound, e.at(0))
		nodeY := e.call(t, a, y, repository.InteractionDirectionInbound, e.at(4))

		for _, contactID := range []uuid.UUID{a, b} {
			preview := e.preview(t, contactID, nodeY, nodeX)
			require.Equal(t, preview, e.preview(t, contactID, nodeY, nodeX), "a preview changes nothing a second preview reads")
		}
		previewA, previewB := e.preview(t, a, nodeY, nodeX), e.preview(t, b, nodeY, nodeX)

		require.Equal(t, previewA, e.merge(t, a, nodeY, nodeX))
		require.Equal(t, nodeY, previewB.InteractionID)
		eps := e.correctable(t, b)
		require.Len(t, eps, 1)
		require.Equal(t, previewB.Participation.Direction, eps[0].Direction)
		require.True(t, previewB.Participation.OccurredAt.Equal(eps[0].OccurredAt))
	})
}
