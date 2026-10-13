//go:build integration_testdb

package repository_test

import (
	"bytes"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The correction predicates' catalog rows and their write path (KNW-037;
// effective-participation.c3): migration 089 seeds three predicates marked
// correction, and AssertService accepts interaction nodes where they name
// 'interaction', landing a user-located write accepted.
func TestCorrectionPredicateCatalog(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("each row reads back with its kind, types, cardinality, symmetry and review policy", func(t *testing.T) {
		cases := []struct {
			key, kind, subject string
			object, value      *string
			cardinality        string
			symmetric          bool
		}{
			{"not_participant_in", repository.PredicateKindEdge, "person", strPtr("interaction"), nil, repository.PredicateCardinalityMulti, false},
			{"removed_for_everyone", repository.PredicateKindFact, "interaction", nil, strPtr(repository.PredicateValueTypeBool), repository.PredicateCardinalitySingle, false},
			{"same_interaction_as", repository.PredicateKindEdge, "interaction", strPtr("interaction"), nil, repository.PredicateCardinalityMulti, false},
		}
		for _, c := range cases {
			p, err := e.predicates.GetPredicate(e.ctx, c.key)
			require.NoError(t, err, c.key)
			assert.Equal(t, c.kind, p.Kind, c.key)
			assert.Equal(t, c.subject, p.SubjectType, c.key)
			assert.Equal(t, c.object, p.ObjectType, c.key)
			assert.Equal(t, c.value, p.ValueType, c.key)
			assert.Equal(t, c.cardinality, p.Cardinality, c.key)
			assert.Equal(t, c.symmetric, p.Symmetric, c.key)
			assert.Equal(t, repository.PredicateReviewAutoIfConfident, p.DefaultReviewPolicy, c.key)
		}
	})

	t.Run("the predicates marked correction are exactly the three", func(t *testing.T) {
		corrections, err := e.predicates.ListCorrection(e.ctx)
		require.NoError(t, err)
		keys := make([]string, len(corrections))
		for i, p := range corrections {
			keys[i] = p.Key
		}
		assert.Equal(t, []string{"not_participant_in", "removed_for_everyone", "same_interaction_as"}, keys)
	})

	t.Run("not_participant_in from a person node to an interaction node lands accepted", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1))
		a := e.removeContact(t, c, *x.InteractionNodeID)
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
	})

	t.Run("removed_for_everyone on an interaction node merged into another lands accepted", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1))
		y := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(2))
		e.merge(t, *x.InteractionNodeID, *y.InteractionNodeID)

		var a *repository.Assertion
		e.inTx(t, func(tx pgx.Tx) error {
			yes := true
			var err error
			a, err = e.asserts.AssertTx(e.ctx, tx, service.AssertRequest{
				SubjectNodeID: *x.InteractionNodeID,
				PredicateKey:  "removed_for_everyone",
				ValueBool:     &yes,
				Confidence:    100,
				Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("duplicate"))},
			})
			return err
		})
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
		assert.Equal(t, *x.InteractionNodeID, a.SubjectNodeID)
	})

	t.Run("same_interaction_as between two interaction nodes lands accepted", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1))
		y := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(2))

		var a *repository.Assertion
		e.inTx(t, func(tx pgx.Tx) error {
			var err error
			a, err = e.asserts.AssertTx(e.ctx, tx, service.AssertRequest{
				SubjectNodeID: *x.InteractionNodeID,
				PredicateKey:  "same_interaction_as",
				ObjectNodeID:  y.InteractionNodeID,
				Confidence:    100,
				Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), nil)},
			})
			return err
		})
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
	})

	t.Run("a node of the wrong type is rejected and writes nothing", func(t *testing.T) {
		c := e.contact(t)
		x := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1))
		node := *x.InteractionNodeID
		yes := true
		cases := map[string]service.AssertRequest{
			"interaction node as not_participant_in subject": {SubjectNodeID: node, PredicateKey: "not_participant_in", ObjectNodeID: &node},
			"person node as not_participant_in object":       {SubjectNodeID: c, PredicateKey: "not_participant_in", ObjectNodeID: &c},
			"person node as removed_for_everyone subject":    {SubjectNodeID: c, PredicateKey: "removed_for_everyone", ValueBool: &yes},
			"person node as same_interaction_as subject":     {SubjectNodeID: c, PredicateKey: "same_interaction_as", ObjectNodeID: &node},
			"person node as same_interaction_as object":      {SubjectNodeID: node, PredicateKey: "same_interaction_as", ObjectNodeID: &c},
		}
		for name, req := range cases {
			req.Confidence = 100
			req.Locators = []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("duplicate"))}
			_, err := e.asserts.Assert(e.ctx, req)
			assert.ErrorIs(t, err, service.ErrAssertValidation, name)
		}
		for _, n := range []uuid.UUID{c, node} {
			touching, err := e.assertions.ListAssertionsBySubject(e.ctx, n)
			require.NoError(t, err)
			for _, a := range touching {
				assert.NotContains(t, []string{"not_participant_in", "removed_for_everyone", "same_interaction_as"}, a.PredicateKey,
					"rejected write left %s on %s", a.PredicateKey, n)
			}
		}
	})
}

// A correction assertion carries the correction representation's provenance
// (effective-participation.c4): AssertService rejects a correction write whose
// locator is not one user locator of the act, a removal without its reason, and
// a second act's corroboration of a recorded correction, and writes nothing.
func TestCorrectionProvenanceIsRequired(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	c := e.contact(t)
	x := e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1))
	node := *x.InteractionNodeID

	agentLocator := userLocator(uuid.New(), strPtr("duplicate"))
	agentLocator.SourceKind = repository.SourceKindAgentSession
	agentLocator.ProducerKind = repository.ProducerKindAgent
	otherReason := userLocator(uuid.New(), strPtr("duplicate"))
	otherReason.Quote = strPtr("not_a_reason")

	cases := map[string][]service.ProvenanceLocator{
		"agent provenance":  {agentLocator},
		"no reason":         {userLocator(uuid.New(), nil)},
		"an unknown reason": {otherReason},
		"two locators":      {userLocator(uuid.New(), strPtr("duplicate")), userLocator(uuid.New(), strPtr("duplicate"))},
	}
	for name, locators := range cases {
		_, err := e.asserts.Assert(e.ctx, service.AssertRequest{
			SubjectNodeID: c,
			PredicateKey:  "not_participant_in",
			ObjectNodeID:  &node,
			Confidence:    100,
			Locators:      locators,
		})
		assert.ErrorIs(t, err, service.ErrAssertValidation, name)
	}
	touching, err := e.assertions.ListAssertionsBySubject(e.ctx, c)
	require.NoError(t, err)
	for _, a := range touching {
		assert.NotEqual(t, "not_participant_in", a.PredicateKey, "a rejected write left a removal")
	}

	t.Run("a second act cannot corroborate a recorded removal", func(t *testing.T) {
		a := e.removeContact(t, c, node)
		_, err := e.asserts.Assert(e.ctx, service.AssertRequest{
			SubjectNodeID: c,
			PredicateKey:  "not_participant_in",
			ObjectNodeID:  &node,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("duplicate"))},
		})
		assert.ErrorIs(t, err, service.ErrAssertValidation)
		prov, err := e.assertions.ListProvenance(e.ctx, a.ID)
		require.NoError(t, err)
		assert.Len(t, prov, 1)
	})
}

// An AssertService write that closes an earlier assertion naming an interaction
// node waits for that node's correction lock (effective-participation.c7): a
// single-cardinality person-to-interaction write that supersedes P -> X blocks
// while another transaction holds X's correction lock, and closes P -> X once
// the lock is released.
func TestAssertWaitsForCorrectionLockOfClosedAssertion(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	predicate := singleAttendedPredicate(t, e)

	c := e.contact(t)
	x := *e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1)).InteractionNodeID
	y := *e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(2)).InteractionNodeID
	write := func(object uuid.UUID) (*repository.Assertion, error) {
		return e.asserts.Assert(e.ctx, attendedRequest(predicate, c, object))
	}
	prior, err := write(x)
	require.NoError(t, err)

	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.nodes.AcquireCorrectionLocksTx(e.ctx, holder, x))

	done := make(chan error, 1)
	go func() {
		_, err := write(y)
		done <- err
	}()
	select {
	case err := <-done:
		require.Failf(t, "write did not wait", "the write closing P -> X finished while X's correction lock was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	require.NoError(t, holder.Rollback(e.ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the write did not finish after the correction lock was released")
	}
	closed, err := e.assertions.GetAssertion(e.ctx, prior.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
}

// singleAttendedPredicate mints a provisional single-cardinality
// person-to-interaction edge predicate.
func singleAttendedPredicate(t *testing.T, e *correctionEnv) *repository.Predicate {
	t.Helper()
	interaction := "interaction"
	predicate, err := e.predicates.CreateProvisional(e.ctx, repository.CreatePredicateRequest{
		Key:                 "test_single_attended_" + uuid.NewString()[:8],
		Kind:                repository.PredicateKindEdge,
		SubjectType:         "person",
		ObjectType:          &interaction,
		Cardinality:         repository.PredicateCardinalitySingle,
		TemporalProfile:     repository.PredicateTemporalMutable,
		DefaultSalience:     50,
		DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
		PropositionBucket:   repository.PredicateBucketNone,
		Description:         "test single-cardinality person-to-interaction edge",
	})
	require.NoError(t, err)
	return predicate
}

// attendedRequest is a user-located write of subject -> object under predicate.
func attendedRequest(predicate *repository.Predicate, subject, object uuid.UUID) service.AssertRequest {
	return service.AssertRequest{
		SubjectNodeID: subject,
		PredicateKey:  predicate.Key,
		ObjectNodeID:  &object,
		Confidence:    100,
		Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), nil)},
	}
}

// A closure and a replacement of one single-cardinality person-to-interaction
// slot serialize on the correction locks without a deadlock
// (effective-participation.c7, ruling R41): with P -> X accepted and X sorting
// before P in the correction-lock order, a closure of P's slot waits while
// another transaction holds X's correction lock, that transaction replaces
// P -> X with P -> Y, and both complete; the closure then closes P -> Y.
func TestClosureAndReplacementSerializeOnCorrectionLocks(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	predicate := singleAttendedPredicate(t, e)

	c := e.contact(t)
	// The correction lock is taken in node-id order, so X must sort before P.
	// An observation's node id is a hash and P's is random, so X is an
	// interaction node whose id leads with eight zero bytes: it sorts before
	// every version-4 id, P's included, since a version-4 id has a nonzero
	// byte 6.
	x := uuid.New()
	copy(x[:8], make([]byte, 8))
	e.inTx(t, func(tx pgx.Tx) error {
		_, err := e.nodes.CreateNodeTx(e.ctx, tx, x, repository.NodeTypeInteraction, "")
		return err
	})
	require.Negative(t, bytes.Compare(x[:], c[:]), "X sorts before P")
	y := *e.call(t, c, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(2)).InteractionNodeID
	prior, err := e.asserts.Assert(e.ctx, attendedRequest(predicate, c, x))
	require.NoError(t, err)

	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.nodes.AcquireCorrectionLocksTx(e.ctx, holder, x))

	done := make(chan error, 1)
	go func() {
		done <- e.asserts.AssertClosure(e.ctx, service.ClosureRequest{SubjectNodeID: c, PredicateKey: predicate.Key})
	}()
	select {
	case err := <-done:
		require.Failf(t, "closure did not wait", "the closure of P -> X finished while X's correction lock was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	replacement, err := e.asserts.AssertTx(e.ctx, holder, attendedRequest(predicate, c, y))
	require.NoError(t, err)
	require.NoError(t, holder.Commit(e.ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the closure did not finish after the replacement committed")
	}

	closedPrior, err := e.assertions.GetAssertion(e.ctx, prior.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.AssertionStatusSuperseded, closedPrior.Status)
	closedReplacement, err := e.assertions.GetAssertion(e.ctx, replacement.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.AssertionStatusSuperseded, closedReplacement.Status)
	require.NotNil(t, closedReplacement.ClosureReason)
	assert.Equal(t, repository.ClosureReasonEnded, *closedReplacement.ClosureReason)
}

// A correction assertion keeps one user locator through a contact merge
// (effective-participation.c4): when two contacts' removals from one
// interaction node collide, the surviving removal keeps only its own act's
// locator, and the merged-away removal stays readable, closed, with its act and
// reason.
func TestContactMergeKeepsOneLocatorPerCorrection(t *testing.T) {
	t.Parallel()
	e := newCorrectionEnv(t)
	loser := e.contact(t)
	winner := e.contact(t)
	node := *e.call(t, winner, "call-"+uuid.NewString(), repository.InteractionDirectionInbound, e.at(1)).InteractionNodeID
	merged := e.removeContact(t, loser, node)
	kept := e.removeContact(t, winner, node)
	keptProv, err := e.assertions.ListProvenance(e.ctx, kept.ID)
	require.NoError(t, err)
	mergedProv, err := e.assertions.ListProvenance(e.ctx, merged.ID)
	require.NoError(t, err)

	e.inTx(t, func(tx pgx.Tx) error { return e.asserts.MergeAssertionsTx(e.ctx, tx, loser, winner) })

	prov, err := e.assertions.ListProvenance(e.ctx, kept.ID)
	require.NoError(t, err)
	require.Len(t, prov, 1)
	assert.Equal(t, keptProv[0].SourceID, prov[0].SourceID)

	closed, err := e.assertions.GetAssertion(e.ctx, merged.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
	assert.Equal(t, &kept.ID, closed.SupersededBy)
	prov, err = e.assertions.ListProvenance(e.ctx, merged.ID)
	require.NoError(t, err)
	require.Len(t, prov, 1)
	assert.Equal(t, mergedProv[0].SourceID, prov[0].SourceID)
	assert.Equal(t, strPtr("did_not_attend"), prov[0].Quote)
}
