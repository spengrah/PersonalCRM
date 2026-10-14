//go:build integration_testdb

package repository_test

import (
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The effective-participation read for a contact (arc seam S4; IXN-016,
// IXN-017, IXN-018; effective-participation.c1): observations recorded through
// the recording path, corrections written through AssertService and the
// interaction-node merge write.
func TestEffectiveParticipationForContact(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	in, out, mutual := repository.InteractionDirectionInbound, repository.InteractionDirectionOutbound, repository.InteractionDirectionMutual
	ref := func() string { return "call-" + uuid.NewString() }

	t.Run("two members merged under one root give one entry combining direction at the later time", func(t *testing.T) {
		cases := []struct {
			name          string
			first, second string
			wantDirection string
		}{
			{"inbound then outbound", in, out, mutual},
			{"outbound then inbound", out, in, mutual},
			{"inbound then inbound", in, in, in},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := e.contact(t)
				x := e.call(t, c, ref(), tc.first, e.at(1))
				y := e.call(t, c, ref(), tc.second, e.at(2))
				e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)

				eps := e.participation(t, c)
				require.Len(t, eps, 1)
				assert.Equal(t, *y.InteractionNodeID, *eps[0].InteractionNodeID)
				assert.Equal(t, tc.wantDirection, eps[0].Direction)
				assert.True(t, e.at(2).Equal(eps[0].OccurredAt))
				assert.ElementsMatch(t, []uuid.UUID{x.ID, y.ID}, memberIDs(eps[0]))
			})
		}
	})

	t.Run("a mutual and a later inbound give one mutual entry at the later time", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, ref(), mutual, e.at(1))
		y := e.call(t, c, ref(), in, e.at(2))
		e.merge(t, *y.InteractionNodeID, *x.InteractionNodeID)

		eps := e.participation(t, c)
		require.Len(t, eps, 1)
		assert.Equal(t, *x.InteractionNodeID, *eps[0].InteractionNodeID)
		assert.Equal(t, mutual, eps[0].Direction)
		assert.True(t, e.at(2).Equal(eps[0].OccurredAt))
	})

	t.Run("a second contact observed only on the earlier member keeps its direction and time", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		xRef := ref()
		e.call(t, a, xRef, in, e.at(1))
		x := e.call(t, b, xRef, in, e.at(1))
		y := e.call(t, a, ref(), out, e.at(3))
		e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)

		eps := e.participation(t, b)
		require.Len(t, eps, 1)
		assert.Equal(t, *y.InteractionNodeID, *eps[0].InteractionNodeID)
		assert.Equal(t, in, eps[0].Direction)
		assert.True(t, e.at(1).Equal(eps[0].OccurredAt))
		assert.Equal(t, []uuid.UUID{x.ID}, memberIDs(eps[0]))
	})

	t.Run("a contact with no observation on any merged node has no entry for the root", func(t *testing.T) {
		a, other := e.contact(t), e.contact(t)
		x := e.call(t, a, ref(), in, e.at(1))
		y := e.call(t, a, ref(), out, e.at(2))
		e.call(t, other, ref(), in, e.at(1))
		e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)

		for _, ep := range e.participation(t, other) {
			assert.NotEqual(t, *y.InteractionNodeID, *ep.InteractionNodeID)
		}
	})

	t.Run("a chain gives one entry under the last root holding the observations of all three", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, ref(), in, e.at(1))
		w := e.call(t, c, ref(), in, e.at(2))
		v := e.call(t, c, ref(), in, e.at(3))
		e.merge(t, *x.InteractionNodeID, *w.InteractionNodeID)
		e.merge(t, *w.InteractionNodeID, *v.InteractionNodeID)

		eps := e.participation(t, c)
		require.Len(t, eps, 1)
		assert.Equal(t, *v.InteractionNodeID, *eps[0].InteractionNodeID)
		require.Len(t, eps[0].Members, 3)
		byID := map[uuid.UUID]repository.EffectiveParticipationMember{}
		for _, m := range eps[0].Members {
			byID[m.ObservationID] = m
		}
		for _, obs := range []*repository.Interaction{x, w, v} {
			m, ok := byID[obs.ID]
			require.True(t, ok, "member %s", obs.ID)
			assert.Equal(t, obs.Source, m.Source)
			assert.Equal(t, obs.SourceRef, m.SourceRef)
			assert.Equal(t, obs.InteractionNodeID, m.InteractionNodeID, "a member keeps its own node")
		}
	})

	t.Run("two live observations of the contact on the same node give one entry", func(t *testing.T) {
		c := e.contact(t)
		// Two email observations of one thread-day share a node: the identity
		// key drops the source_ref's first component.
		thread := "thread-" + uuid.NewString()
		first := e.observe(t, c, repository.InteractionSourceEmail, "a:"+thread+":2026-10-01", in, e.at(1))
		second := e.observe(t, c, repository.InteractionSourceEmail, "b:"+thread+":2026-10-01", out, e.at(2))
		require.Equal(t, *first.InteractionNodeID, *second.InteractionNodeID)

		eps := e.participation(t, c)
		require.Len(t, eps, 1)
		assert.Equal(t, mutual, eps[0].Direction)
		assert.True(t, e.at(2).Equal(eps[0].OccurredAt))
		assert.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, memberIDs(eps[0]))
	})

	t.Run("not_participant_in binds to the judged node through later merges", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, ref(), in, e.at(1))
		y := e.call(t, c, ref(), in, e.at(2))

		e.removeContact(t, c, *x.InteractionNodeID)
		for _, ep := range e.participation(t, c) {
			assert.NotContains(t, memberIDs(ep), x.ID, "the removed observation is excluded")
		}

		e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)
		eps := e.participation(t, c)
		require.Len(t, eps, 1)
		assert.Equal(t, []uuid.UUID{y.ID}, memberIDs(entryFor(t, eps, *y.InteractionNodeID)))

		e.removeContact(t, c, *y.InteractionNodeID)
		assert.Empty(t, e.participation(t, c))
	})

	t.Run("not_participant_in from one contact leaves another contact's observation effective", func(t *testing.T) {
		a, b := e.contact(t), e.contact(t)
		xRef := ref()
		x := e.call(t, a, xRef, in, e.at(1))
		bx := e.call(t, b, xRef, in, e.at(1))
		e.removeContact(t, a, *x.InteractionNodeID)

		assert.Empty(t, e.participation(t, a))
		assert.Equal(t, []uuid.UUID{bx.ID}, memberIDs(entryFor(t, e.participation(t, b), *x.InteractionNodeID)))
	})

	t.Run("a proposed not_participant_in and a retracted one leave the observation effective", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, ref(), in, e.at(1))
		node := *x.InteractionNodeID

		proposed, err := e.asserts.Assert(e.ctx, service.AssertRequest{
			SubjectNodeID: c,
			PredicateKey:  "not_participant_in",
			ObjectNodeID:  &node,
			Confidence:    100,
			ForceConfirm:  true,
			Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("did_not_attend"))},
		})
		require.NoError(t, err)
		require.Equal(t, repository.AssertionStatusProposed, proposed.Status)
		assert.Len(t, e.participation(t, c), 1, "a proposed removal excludes nothing")

		_, err = e.asserts.Reject(e.ctx, proposed.ID, service.RejectRequest{})
		require.NoError(t, err)
		accepted := e.removeContact(t, c, node)
		assert.Empty(t, e.participation(t, c))
		_, err = e.asserts.Retract(e.ctx, accepted.ID, service.RetractRequest{})
		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{x.ID}, memberIDs(entryFor(t, e.participation(t, c), node)), "a retracted removal excludes nothing")
	})

	t.Run("removed_for_everyone excludes every observation of the node, later ones included, and no node merged in afterwards", func(t *testing.T) {
		a, b, late := e.contact(t), e.contact(t), e.contact(t)
		xRef := ref()
		x := e.call(t, a, xRef, in, e.at(1))
		e.call(t, b, xRef, out, e.at(1))
		e.removeEveryone(t, *x.InteractionNodeID)
		lateObs := e.call(t, late, xRef, in, e.at(1))
		require.Equal(t, *x.InteractionNodeID, *lateObs.InteractionNodeID)

		for _, c := range []uuid.UUID{a, b, late} {
			assert.Empty(t, e.participation(t, c))
		}

		y := e.call(t, a, ref(), out, e.at(2))
		e.merge(t, *y.InteractionNodeID, *x.InteractionNodeID)
		eps := e.participation(t, a)
		require.Len(t, eps, 1)
		assert.Equal(t, []uuid.UUID{y.ID}, memberIDs(entryFor(t, eps, *x.InteractionNodeID)))
		assert.Equal(t, out, eps[0].Direction)
	})

	t.Run("each message-burst observation is an entry of its own with no interaction node", func(t *testing.T) {
		c := e.contact(t)
		chat := "tg:" + uuid.NewString()
		first := e.observe(t, c, repository.InteractionSourceTelegram, chat+":1", in, e.at(1))
		second := e.observe(t, c, repository.InteractionSourceTelegram, chat+":2", out, e.at(2))
		call := e.call(t, c, ref(), in, e.at(3))

		eps := e.participation(t, c)
		require.Len(t, eps, 3)
		bursts := map[uuid.UUID]repository.EffectiveParticipation{}
		for _, ep := range eps {
			if ep.InteractionNodeID == nil {
				require.Len(t, ep.Members, 1)
				assert.Nil(t, ep.Members[0].InteractionNodeID)
				bursts[ep.Members[0].ObservationID] = ep
			}
		}
		require.Len(t, bursts, 2)
		assert.Equal(t, in, bursts[first.ID].Direction)
		assert.Equal(t, out, bursts[second.ID].Direction)
		assert.Equal(t, []uuid.UUID{call.ID}, memberIDs(entryFor(t, eps, *call.InteractionNodeID)))
	})

	t.Run("a soft-deleted observation contributes no entry and no member", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, ref(), in, e.at(1))
		y := e.call(t, c, ref(), out, e.at(2))
		gone := e.call(t, c, ref(), in, e.at(3))
		e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)
		require.NoError(t, e.interactions.SoftDeleteInteraction(e.ctx, x.ID))
		require.NoError(t, e.interactions.SoftDeleteInteraction(e.ctx, gone.ID))

		eps := e.participation(t, c)
		require.Len(t, eps, 1)
		assert.Equal(t, []uuid.UUID{y.ID}, memberIDs(eps[0]))
		assert.Equal(t, out, eps[0].Direction)
	})
}

// The effective-participation read for an interaction (arc seam S4;
// effective-participation.c2): read by the root's id or any merged node's id,
// it lists exactly the contacts holding an entry for the root in their own
// effective participation, each with that entry.
func TestEffectiveParticipationForInteraction(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	in, out := repository.InteractionDirectionInbound, repository.InteractionDirectionOutbound
	ref := func() string { return "call-" + uuid.NewString() }

	// participants reads the interaction by node and checks each listed entry
	// against the contact's own effective participation.
	participants := func(t *testing.T, node, root uuid.UUID) map[uuid.UUID]repository.EffectiveParticipation {
		t.Helper()
		eps, err := e.interactions.ListInteractionParticipants(e.ctx, node)
		require.NoError(t, err)
		byContact := map[uuid.UUID]repository.EffectiveParticipation{}
		for _, ep := range eps {
			require.NotNil(t, ep.InteractionNodeID)
			assert.Equal(t, root, *ep.InteractionNodeID)
			assert.Equal(t, entryFor(t, e.participation(t, ep.ContactID), root), ep)
			byContact[ep.ContactID] = ep
		}
		return byContact
	}

	// mergedPair records a, b and c on call X and a on call Y, then merges X
	// into Y.
	mergedPair := func(t *testing.T) (a, b, c uuid.UUID, x, y uuid.UUID, bx *repository.Interaction) {
		t.Helper()
		a, b, c = e.contact(t), e.contact(t), e.contact(t)
		xRef := ref()
		ax := e.call(t, a, xRef, in, e.at(1))
		bx = e.call(t, b, xRef, in, e.at(1))
		e.call(t, c, xRef, in, e.at(1))
		ay := e.call(t, a, ref(), out, e.at(2))
		e.merge(t, *ax.InteractionNodeID, *ay.InteractionNodeID)
		return a, b, c, *ax.InteractionNodeID, *ay.InteractionNodeID, bx
	}

	t.Run("a contact excluded on every member it was observed on is absent", func(t *testing.T) {
		a, b, c, x, y, _ := mergedPair(t)
		e.removeContact(t, c, x)

		got := participants(t, y, y)
		assert.Len(t, got, 2)
		assert.Contains(t, got, a)
		assert.Contains(t, got, b)
		assert.NotContains(t, got, c)
	})

	t.Run("reading by a merged-away node's id returns the root's participants", func(t *testing.T) {
		a, b, c, x, y, _ := mergedPair(t)
		byRoot := participants(t, y, y)
		byMember := participants(t, x, y)
		assert.Equal(t, byRoot, byMember)
		assert.Len(t, byMember, 3)
		for _, contact := range []uuid.UUID{a, b, c} {
			assert.Contains(t, byMember, contact)
		}
	})

	t.Run("a contact observed on only one member is listed with that member", func(t *testing.T) {
		_, b, _, _, y, bx := mergedPair(t)
		got := participants(t, y, y)
		require.Contains(t, got, b)
		assert.Equal(t, []uuid.UUID{bx.ID}, memberIDs(got[b]))
		assert.Equal(t, in, got[b].Direction)
		assert.True(t, e.at(1).Equal(got[b].OccurredAt))
	})

	t.Run("removed_for_everyone on every member leaves no participant", func(t *testing.T) {
		_, _, _, x, y, _ := mergedPair(t)
		e.removeEveryone(t, x)
		assert.Len(t, participants(t, y, y), 1, "the member not yet removed keeps its participant")
		e.removeEveryone(t, y)
		assert.Empty(t, participants(t, y, y))
	})
}
