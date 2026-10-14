package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
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

	t.Run("a removal binds to a judged member merged away into one the contact still attends", func(t *testing.T) {
		// spec: IXN-018.binds-to-judged-members
		alice, bob := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev, callRef := e.event(t), e.ref("call")
		meetingX := e.meeting(t, alice, ev, e.at(0))
		e.meeting(t, bob, ev, e.at(0))
		callY := e.call(t, alice, callRef, repository.InteractionDirectionMutual, e.at(1))

		e.remove(t, meetingX, removalBody(service.RemovalReasonDidNotAttend, alice))
		e.merge(t, bob, callY, meetingX)

		eps := e.correctable(t, alice)
		require.Len(t, eps, 1, "alice keeps her participation through Y")
		require.Equal(t, callY, *eps[0].InteractionNodeID)
		require.Len(t, eps[0].Members, 1)
		require.Equal(t, callY, *eps[0].Members[0].InteractionNodeID)
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

// A removal naming a contact and a concurrent assertion write naming that
// contact and the interaction serialize on the correction locks without a
// deadlock (corrections.c11). The removal's not_participant_in names the
// contact's node, so the removal takes that node's correction lock with the
// interaction's in one ordered acquisition. With the contact sorting before
// the interaction, a removal that took the interaction's lock first and the
// contact's later would hold the interaction while the writer holds the
// contact and waits for the interaction.
func TestInteractionRemoval_SerializesWithAssertionWrites(t *testing.T) {
	// spec: IXN-018.scope-removes-exactly-selected
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)
	interaction := "interaction"
	predicate, err := repository.NewPredicateRepository(e.database.Queries).CreateProvisional(e.ctx, repository.CreatePredicateRequest{
		Key:                 "test_attended_" + uuid.NewString()[:8],
		Kind:                repository.PredicateKindEdge,
		SubjectType:         "person",
		ObjectType:          &interaction,
		Cardinality:         repository.PredicateCardinalityMulti,
		TemporalProfile:     repository.PredicateTemporalMutable,
		DefaultSalience:     50,
		DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
		PropositionBucket:   repository.PredicateBucketNone,
		Description:         "test person-to-interaction edge",
	})
	require.NoError(t, err)

	// The correction lock is taken in node-id order; both ids are random, so
	// draw until the contact sorts before the interaction.
	var contact, node uuid.UUID
	for {
		contact = e.contact(t, "monthly")
		node = e.call(t, contact, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		if bytes.Compare(contact[:], node[:]) < 0 {
			break
		}
	}

	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.nodes.AcquireCorrectionLocksTx(e.ctx, holder, node))

	removal := make(chan int, 1)
	go func() {
		w := e.post(t, "/api/v1/interactions/"+node.String()+"/removals", removalBody(service.RemovalReasonDidNotAttend, contact))
		removal <- w.Code
	}()
	time.Sleep(500 * time.Millisecond)
	write := make(chan error, 1)
	go func() {
		_, err := e.asserts.Assert(e.ctx, service.AssertRequest{
			SubjectNodeID: contact,
			PredicateKey:  predicate.Key,
			ObjectNodeID:  &node,
			Confidence:    100,
			Locators: []service.ProvenanceLocator{{
				SourceKind:   repository.SourceKindUser,
				SourceID:     uuid.NewString(),
				ProducerKind: repository.ProducerKindUser,
			}},
		})
		write <- err
	}()
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, holder.Rollback(e.ctx))

	select {
	case code := <-removal:
		require.Equal(t, http.StatusNoContent, code)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the removal did not finish")
	}
	select {
	case err := <-write:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the assertion write did not finish")
	}
	require.Empty(t, e.correctable(t, contact))
}

// A removal and a concurrent contact merge whose source's assertions name the
// removal's interaction serialize on the correction locks without a deadlock
// (corrections.c11). Contact A was removed from member X; contact B
// participates through member Y; X and Y are merged. Removing B takes the
// correction locks on X, Y and B and then B's row. Merging A into B writes
// B's row and re-points A's not_participant_in on X, so it takes its
// correction locks, on A, B and X, before its row writes, as a correction
// does; taking them only after would hold B's row while waiting for the locks
// the removal holds while it waits for B's row.
func TestInteractionRemoval_SerializesWithContactMerge(t *testing.T) {
	// spec: IXN-018.survives-contact-merge
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)
	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	nodeX := e.meeting(t, a, e.event(t), e.at(0))
	nodeY := e.meeting(t, b, e.event(t), e.at(1))
	e.remove(t, nodeX, removalBody(service.RemovalReasonDidNotAttend, a))
	e.merge(t, b, nodeX, nodeY)

	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.nodes.AcquireCorrectionLocksTx(e.ctx, holder, nodeX))
	holderPID, err := db.New(holder).TestBackendPID(e.ctx)
	require.NoError(t, err)
	waiting := func(n int64) func() bool {
		return func() bool {
			got, err := e.database.Queries.TestCountBackendsWaitingOn(e.ctx, holderPID)
			return err == nil && got == n
		}
	}

	removal := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removal <- e.post(t, "/api/v1/interactions/"+nodeX.String()+"/removals", removalBody(service.RemovalReasonDidNotAttend, b))
	}()
	require.Eventually(t, waiting(1), 10*time.Second, 10*time.Millisecond, "the removal never queued on X's correction lock")
	merge := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		merge <- e.post(t, "/api/v1/contacts/"+b.String()+"/merge", map[string]any{"source_contact_id": a.String()})
	}()
	require.Eventually(t, waiting(2), 10*time.Second, 10*time.Millisecond, "the contact merge never queued on a correction lock")
	require.NoError(t, holder.Rollback(e.ctx))

	w := responseWithin(t, removal, "removal")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	w = responseWithin(t, merge, "contact merge")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, e.correctable(t, b))
}

// responseWithin returns the response done delivers, failing the test when
// none arrives within ten seconds.
func responseWithin(t *testing.T, done <-chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(10 * time.Second):
		require.FailNowf(t, "request did not finish", "the %s did not finish", what)
		return nil
	}
}

// A contact merge and a concurrent profile update of its target serialize
// without a deadlock (corrections.c15). The source's how_met re-points into
// the target's how_met slot. The profile update locks the target's contact
// row, then its person-node row, then that slot. The merge takes its
// correction locks before any row write and its slot locks only after its row
// writes, as the profile writer does; taking the slot locks with the
// correction locks would hold the slot while waiting for the contact row the
// profile update holds while it waits for the slot. A held person-node row
// keeps the profile update between its row locks and its slot lock until the
// merge has queued behind it.
func TestContactMerge_SerializesWithProfileUpdate(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)
	source, target := e.contact(t, "monthly"), e.contact(t, "monthly")
	sourceHowMet, targetHowMet := "met at a synthetic meetup", "met at a synthetic workshop"
	e.updateProfile(t, source, &sourceHowMet)

	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.nodes.UpdateNodeCanonicalLabelTx(e.ctx, holder, target, "held"))
	holderPID, err := db.New(holder).TestBackendPID(e.ctx)
	require.NoError(t, err)
	waiting := func(n int64) func() bool {
		return func() bool {
			got, err := e.database.Queries.TestCountBackendsWaitingOn(e.ctx, holderPID)
			return err == nil && got == n
		}
	}

	update := make(chan error, 1)
	go func() {
		_, err := e.contacts.UpdateContact(e.ctx, target, e.profileRequest(t, target, &targetHowMet))
		update <- err
	}()
	require.Eventually(t, waiting(1), 10*time.Second, 10*time.Millisecond, "the profile update never queued on the target's node row")
	merge := make(chan error, 1)
	go func() {
		_, err := e.contacts.MergeContacts(e.ctx, service.MergeContactsRequest{SourceContactID: source, TargetContactID: target})
		merge <- err
	}()
	require.Eventually(t, waiting(2), 10*time.Second, 10*time.Millisecond, "the contact merge never queued behind the profile update")
	require.NoError(t, holder.Rollback(e.ctx))

	require.NoError(t, errWithin(t, update, "profile update"))
	require.NoError(t, errWithin(t, merge, "contact merge"))
}

// updateProfile sets a contact's how_met through UpdateContact, keeping its
// other profile fields.
func (e *correctionEnv) updateProfile(t *testing.T, contactID uuid.UUID, howMet *string) {
	t.Helper()
	_, err := e.contacts.UpdateContact(e.ctx, contactID, e.profileRequest(t, contactID, howMet))
	require.NoError(t, err)
}

// profileRequest is the contact's current profile with how_met replaced.
func (e *correctionEnv) profileRequest(t *testing.T, contactID uuid.UUID, howMet *string) repository.UpdateContactRequest {
	t.Helper()
	c, err := e.contacts.GetContact(e.ctx, contactID)
	require.NoError(t, err)
	return repository.UpdateContactRequest{
		FullName: c.FullName, Location: c.Location, Birthday: c.Birthday,
		HowMet: howMet, Cadence: c.Cadence, ProfilePhoto: c.ProfilePhoto,
	}
}

// errWithin returns the error done delivers, failing the test when none
// arrives within ten seconds.
func errWithin(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		require.FailNowf(t, "call did not finish", "the %s did not finish", what)
		return nil
	}
}
