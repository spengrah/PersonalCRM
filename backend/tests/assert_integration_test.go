//go:build integration_testdb

package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertNoopWorker satisfies the River Workers bundle for the test bus (the
// assertion kinds enqueue no consumer jobs, so it is never invoked).
type assertNoopWorker struct {
	river.WorkerDefaults[assertNoopArgs]
}

type assertNoopArgs struct{}

func (assertNoopArgs) Kind() string { return "assert_noop" }
func (*assertNoopWorker) Work(context.Context, *river.Job[assertNoopArgs]) error {
	return nil
}

// assertHarness bundles the service under test + the repos a test reads back.
type assertHarness struct {
	svc           *service.AssertService
	assertionRepo *repository.AssertionRepository
	nodeRepo      *repository.NodeRepository
	entityRepo    *repository.EntityRepository
	eventRepo     *repository.EventRepository
	support       *repository.SyntheticSupportRepository
	database      *db.Database
}

// newAssertHarness builds an AssertService over a never-started River client (the
// assertion kinds route to no consumer, so InsertTx is never called).
func newAssertHarness(t *testing.T, ctx context.Context) (*assertHarness, context.Context) {
	t.Helper()
	database, cfg := newSharedTestDB(t, ctx)

	eventRepo := repository.NewEventRepository(database.Queries)
	workers := river.NewWorkers()
	river.AddWorker(workers, &assertNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)

	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	svc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)

	return &assertHarness{
		svc:           svc,
		assertionRepo: assertionRepo,
		nodeRepo:      nodeRepo,
		entityRepo:    entityRepo,
		eventRepo:     eventRepo,
		support:       repository.NewSyntheticSupportRepository(database.Queries),
		database:      database,
	}, ctx
}

// seedPerson creates a person node and registers cleanup (assertions before
// nodes, since the assertion→node FK is restrict). The event rows an assert
// produces are cleaned per-assertion by the test via cleanupAssertionEvents.
func (h *assertHarness) seedPerson(t *testing.T, ctx context.Context, prefix, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := h.nodeRepo.CreateNode(ctx, id, repository.NodeTypePerson, prefix+label)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = h.support.DeleteAssertionsForNode(ctx, id) })
	t.Cleanup(func() { _, _ = h.support.DeleteNodesByLabelPrefix(ctx, prefix) })
	return id
}

// seedPlace creates a place entity node (subtype='place') for lives_in/within
// edge tests, registering the same cleanup ordering.
func (h *assertHarness) seedPlace(t *testing.T, ctx context.Context, prefix, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := h.nodeRepo.CreateNode(ctx, id, repository.NodeTypeEntity, prefix+label)
	require.NoError(t, err)
	_, err = h.entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
		NodeID:         id,
		Subtype:        repository.EntitySubtypePlace,
		NormalizedName: prefix + label,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = h.support.DeleteAssertionsForNode(ctx, id) })
	t.Cleanup(func() { _, _ = h.support.DeleteNodesByLabelPrefix(ctx, prefix) })
	return id
}

// userLocator builds a user provenance locator with a stable per-test source_id
// (no content-row existence check). srcSuffix disambiguates distinct locators.
func userLocator(prefix, srcSuffix string) service.ProvenanceLocator {
	return service.ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     prefix + "edit:" + srcSuffix,
		ProducerKind: repository.ProducerKindUser,
	}
}

// textFactReq builds a single-text-fact AssertRequest with one user locator.
func textFactReq(subject uuid.UUID, predicate, value, prefix, srcSuffix string) service.AssertRequest {
	v := value
	return service.AssertRequest{
		SubjectNodeID: subject,
		PredicateKey:  predicate,
		ValueText:     &v,
		Confidence:    80,
		Locators:      []service.ProvenanceLocator{userLocator(prefix, srcSuffix)},
	}
}

// edgeReq builds an edge AssertRequest with one user locator.
func edgeReq(subject, object uuid.UUID, predicate, prefix, srcSuffix string) service.AssertRequest {
	return service.AssertRequest{
		SubjectNodeID: subject,
		PredicateKey:  predicate,
		ObjectNodeID:  &object,
		Confidence:    80,
		Locators:      []service.ProvenanceLocator{userLocator(prefix, srcSuffix)},
	}
}

// cleanupAssertionEvents registers cleanup of the event rows an assertion
// produced (source='assertion', source_id prefixed by the assertion id).
func (h *assertHarness) cleanupAssertionEvents(t *testing.T, ctx context.Context, assertionID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_ = h.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, "assertion", assertionID.String())
	})
}

// eventExists reports whether an event row exists for (source='assertion',
// source_id).
func (h *assertHarness) eventExists(t *testing.T, ctx context.Context, sourceID string) bool {
	t.Helper()
	_, err := h.eventRepo.FindEventBySource(ctx, "assertion", sourceID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, db.ErrNotFound)
	return false
}

// acceptedSourceID / supersededSourceID / provenanceSourceID build the event
// source_id keys an assertion's transitions produce.
func acceptedSourceID(id uuid.UUID) string   { return id.String() + ":accepted" }
func proposedSourceID(id uuid.UUID) string   { return id.String() + ":proposed" }
func supersededSourceID(id uuid.UUID) string { return id.String() + ":superseded" }
func rejectedSourceID(id uuid.UUID) string   { return id.String() + ":rejected" }

func TestAssert_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	h, ctx := newAssertHarness(t, ctx0())

	// Case 1: a new fact lands accepted; assertion.accepted emitted.
	t.Run("1 new fact lands accepted with event", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		a, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "123 Main St", gen.Prefix(), "1"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
		assert.Nil(t, a.KnowledgeTo)
		assert.True(t, h.eventExists(t, ctx, acceptedSourceID(a.ID)), "assertion.accepted emitted")

		provs, err := h.assertionRepo.ListProvenance(ctx, a.ID)
		require.NoError(t, err)
		require.Len(t, provs, 1)
	})

	// Case 2: corroboration from a 2nd locator → one assertion, two provenance,
	// confidence=max, provenance_added emitted; a 3rd SAME locator → no-op.
	t.Run("2 corroboration dedup + locator idempotency", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		req1 := textFactReq(subject, "home_address", "456 Oak Ave", gen.Prefix(), "loc1")
		req1.Confidence = 60
		a, err := h.svc.Assert(ctx, req1)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)

		// Second source, SAME proposition (same value+bucket), different locator.
		req2 := textFactReq(subject, "home_address", "456 Oak Ave", gen.Prefix(), "loc2")
		req2.Confidence = 90
		a2, err := h.svc.Assert(ctx, req2)
		require.NoError(t, err)
		assert.Equal(t, a.ID, a2.ID, "same proposition → one assertion row")
		assert.EqualValues(t, 90, a2.Confidence, "confidence = max(existing, incoming)")

		provs, err := h.assertionRepo.ListProvenance(ctx, a.ID)
		require.NoError(t, err)
		assert.Len(t, provs, 2, "two distinct locators")

		// One assertion row total for the subject.
		n, err := h.support.CountAssertionsForSubject(ctx, subject)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)

		// Third corroboration with the SAME locator as req2 → no new provenance.
		a3, err := h.svc.Assert(ctx, req2)
		require.NoError(t, err)
		assert.Equal(t, a.ID, a3.ID)
		provs, err = h.assertionRepo.ListProvenance(ctx, a.ID)
		require.NoError(t, err)
		assert.Len(t, provs, 2, "same-locator re-emit is a no-op")
	})

	// Case 3: single-predicate successor (different value) supersedes the prior.
	t.Run("3 present successor supersedes prior", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		prior, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "NYC addr", gen.Prefix(), "p"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, prior.ID)

		successor, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "LA addr", gen.Prefix(), "s"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, successor.ID)
		assert.NotEqual(t, prior.ID, successor.ID)

		closed, err := h.assertionRepo.GetAssertion(ctx, prior.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
		require.NotNil(t, closed.SupersededBy)
		assert.Equal(t, successor.ID, *closed.SupersededBy)
		require.NotNil(t, closed.KnowledgeTo)
		assert.True(t, h.eventExists(t, ctx, supersededSourceID(prior.ID)))
		assert.True(t, h.eventExists(t, ctx, acceptedSourceID(successor.ID)))

		now := accelerated.GetCurrentTime().UTC()
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, successor.ID, cur.ID, "current is the successor")
	})

	// Case 4: closure-only → prior closed 'ended', current becomes a gap.
	t.Run("4 closure-only leaves a gap", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		prior, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "old addr", gen.Prefix(), "p"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, prior.ID)

		require.NoError(t, h.svc.AssertClosure(ctx, service.ClosureRequest{SubjectNodeID: subject, PredicateKey: "home_address"}))

		closed, err := h.assertionRepo.GetAssertion(ctx, prior.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
		require.NotNil(t, closed.ClosureReason)
		assert.Equal(t, repository.ClosureReasonEnded, *closed.ClosureReason)
		assert.Nil(t, closed.SupersededBy, "closure-only has no successor")

		now := accelerated.GetCurrentTime().UTC()
		_, err = h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.ErrorIs(t, err, db.ErrNotFound, "current is now a gap")
	})

	// Case 5: multi-predicate accumulation — two health conditions, both live.
	t.Run("5 multi accumulates without supersession", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		// health_condition is always-confirm → proposed; accept both so they are live.
		a, err := h.svc.Assert(ctx, textFactReq(subject, "health_condition", "condition A", gen.Prefix(), "a"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)
		assert.Equal(t, repository.AssertionStatusProposed, a.Status)

		b, err := h.svc.Assert(ctx, textFactReq(subject, "health_condition", "condition B", gen.Prefix(), "b"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, b.ID)

		// Both still live (proposed), no supersession on a multi predicate.
		ga, err := h.assertionRepo.GetAssertion(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusProposed, ga.Status)
		gb, err := h.assertionRepo.GetAssertion(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusProposed, gb.Status)
	})

	// Case 6: always-confirm single (partner_of) → proposed; Accept supersedes the
	// prior accepted partnership at accept time.
	t.Run("6 always-confirm proposed then Accept supersedes", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")
		c := h.seedPerson(t, ctx, gen.Prefix(), "C")

		// partner_of is always-confirm → proposed. Accept A-B so it is the current
		// accepted partnership.
		propAB, err := h.svc.Assert(ctx, edgeReq(a, b, "partner_of", gen.Prefix(), "ab"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propAB.ID)
		assert.Equal(t, repository.AssertionStatusProposed, propAB.Status)
		acceptedAB, err := h.svc.Accept(ctx, propAB.ID, service.AcceptRequest{})
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, acceptedAB.Status)

		// Propose A-C, then Accept it → A-B is superseded (A has one partner).
		propAC, err := h.svc.Assert(ctx, edgeReq(a, c, "partner_of", gen.Prefix(), "ac"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propAC.ID)
		acceptedAC, err := h.svc.Accept(ctx, propAC.ID, service.AcceptRequest{})
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, acceptedAC.Status)

		closedAB, err := h.assertionRepo.GetAssertion(ctx, propAB.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closedAB.Status, "A-B superseded on Accept(A-C)")
		require.NotNil(t, closedAB.SupersededBy)
		assert.Equal(t, propAC.ID, *closedAB.SupersededBy)
	})

	// Case 7: Reject a proposed → rejected (never current); Retract an accepted →
	// retracted (emits superseded).
	t.Run("7 reject and retract", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		// Reject a proposed health_condition.
		prop, err := h.svc.Assert(ctx, textFactReq(subject, "health_condition", "to reject", gen.Prefix(), "r"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, prop.ID)
		rejected, err := h.svc.Reject(ctx, prop.ID, service.RejectRequest{})
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusRejected, rejected.Status)
		require.NotNil(t, rejected.KnowledgeTo)
		assert.True(t, h.eventExists(t, ctx, rejectedSourceID(prop.ID)))

		// Retract an accepted fact → retracted, emits the superseded kind.
		acc, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "to retract", gen.Prefix(), "rt"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, acc.ID)
		retracted, err := h.svc.Retract(ctx, acc.ID, service.RetractRequest{})
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusRetracted, retracted.Status)
		require.NotNil(t, retracted.ClosureReason)
		assert.Equal(t, repository.ClosureReasonRetracted, *retracted.ClosureReason)
		assert.True(t, h.eventExists(t, ctx, supersededSourceID(acc.ID)), "retract emits the superseded kind")
	})

	// Case 8: symmetric edge (knows) A→B then B→A → one row, found for both.
	t.Run("8 symmetric edge dedups both directions", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")

		ab, err := h.svc.Assert(ctx, edgeReq(a, b, "knows", gen.Prefix(), "ab"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, ab.ID)

		ba, err := h.svc.Assert(ctx, edgeReq(b, a, "knows", gen.Prefix(), "ba"))
		require.NoError(t, err)
		assert.Equal(t, ab.ID, ba.ID, "A knows B and B knows A collapse to one row")

		forA, err := h.assertionRepo.ListLiveEdgesForNode(ctx, a, "knows")
		require.NoError(t, err)
		require.Len(t, forA, 1)
		forB, err := h.assertionRepo.ListLiveEdgesForNode(ctx, b, "knows")
		require.NoError(t, err)
		require.Len(t, forB, 1)
		assert.Equal(t, forA[0].ID, forB[0].ID)
	})

	// Case 9: inverse edge parent_of(A,B) then child_of(B,A) → one canonical row.
	t.Run("9 inverse edge dedups to one canonical row", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")

		// parent_of(A,B): A is parent of B.
		parentAB, err := h.svc.Assert(ctx, edgeReq(a, b, "parent_of", gen.Prefix(), "p"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, parentAB.ID)

		// child_of(B,A): B is child of A — the SAME relationship, canonical token.
		childBA, err := h.svc.Assert(ctx, edgeReq(b, a, "child_of", gen.Prefix(), "c"))
		require.NoError(t, err)
		assert.Equal(t, parentAB.ID, childBA.ID, "inverse pair collapses to one row")

		// The stored row uses the canonical predicate (child_of < parent_of).
		row, err := h.assertionRepo.GetAssertion(ctx, parentAB.ID)
		require.NoError(t, err)
		assert.Equal(t, "child_of", row.PredicateKey)
	})

	// Case 10: edge to a latent person via EnsureLatentPerson.
	t.Run("10 edge to a latent person", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")

		var latentID uuid.UUID
		tx, err := h.database.Pool.Begin(ctx)
		require.NoError(t, err)
		latentID, err = h.svc.EnsureLatentPerson(ctx, tx, gen.Prefix()+"latent-person")
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		t.Cleanup(func() { _, _ = h.support.DeleteAssertionsForNode(ctx, latentID) })

		edge, err := h.svc.Assert(ctx, edgeReq(a, latentID, "knows", gen.Prefix(), "k"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)
		require.NotNil(t, edge.ObjectNodeID)

		forLatent, err := h.assertionRepo.ListLiveEdgesForNode(ctx, latentID, "knows")
		require.NoError(t, err)
		require.Len(t, forLatent, 1)
	})

	// Case 11: idempotent re-emit — replaying the same accepted write twice → one
	// assertion, one :accepted event.
	t.Run("11 idempotent re-emit", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		req := textFactReq(subject, "home_address", "idem addr", gen.Prefix(), "i")
		a1, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a1.ID)
		a2, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, a1.ID, a2.ID, "replay → one assertion")

		n, err := h.support.CountAssertionsForSubject(ctx, subject)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.True(t, h.eventExists(t, ctx, acceptedSourceID(a1.ID)))
	})
}

// ctx0 returns a fresh background context (small indirection so the harness
// constructor reads cleanly).
func ctx0() context.Context { return context.Background() }

// TestAssert_Concurrency covers the race-protection cases (savepoint-recover +
// advisory lock). Each runs concurrent top-level Assert calls (separate txs).
func TestAssert_Concurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	h, ctx := newAssertHarness(t, ctx0())

	// Case 12: concurrent SAME-proposition writes → one live assertion, the loser
	// corroborates (savepoint-recover path).
	t.Run("12 concurrent same-proposition", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		req := textFactReq(subject, "home_address", "same addr", gen.Prefix(), "same")
		const n = 4
		results := make([]*repository.Assertion, n)
		errs := make([]error, n)
		runConcurrent(n, func(i int) { results[i], errs[i] = h.svc.Assert(ctx, req) })

		var firstID uuid.UUID
		for i := 0; i < n; i++ {
			require.NoErrorf(t, errs[i], "goroutine %d", i)
			require.NotNil(t, results[i])
			if firstID == uuid.Nil {
				firstID = results[i].ID
			}
			assert.Equal(t, firstID, results[i].ID, "all converge on one assertion")
		}
		h.cleanupAssertionEvents(t, ctx, firstID)
		cnt, err := h.support.CountAssertionsForSubject(ctx, subject)
		require.NoError(t, err)
		assert.EqualValues(t, 1, cnt, "exactly one assertion row")
	})

	// Case 13: concurrent DIFFERENT-value single-card writes into an empty slot →
	// exactly one accepted-current row (the advisory lock serializes the empty slot).
	t.Run("13 concurrent different-value empty slot", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		const n = 4
		results := make([]*repository.Assertion, n)
		errs := make([]error, n)
		runConcurrent(n, func(i int) {
			req := textFactReq(subject, "home_address", "addr-"+uuid.NewString(), gen.Prefix(), "v"+uuid.NewString())
			results[i], errs[i] = h.svc.Assert(ctx, req)
		})
		for i := 0; i < n; i++ {
			require.NoErrorf(t, errs[i], "goroutine %d", i)
			h.cleanupAssertionEvents(t, ctx, results[i].ID)
		}

		// Exactly one is current-accepted; the rest are superseded.
		now := accelerated.GetCurrentTime().UTC()
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err, "exactly one current-accepted row survives the race")
		assert.NotNil(t, cur)

		live := 0
		for i := 0; i < n; i++ {
			row, err := h.assertionRepo.GetAssertion(ctx, results[i].ID)
			require.NoError(t, err)
			if row.Status == repository.AssertionStatusAccepted && row.KnowledgeTo == nil {
				live++
			}
		}
		assert.Equal(t, 1, live, "exactly one accepted-live row after the race")
	})

	// Case 14: concurrent Accept of two proposed single-card assertions into an
	// empty slot → exactly one ends accepted-current (advisory lock on the Accept
	// path).
	t.Run("14 concurrent Accept of two proposed", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")
		c := h.seedPerson(t, ctx, gen.Prefix(), "C")

		// Two proposed partnerships for A (partner_of is always-confirm → proposed).
		propAB, err := h.svc.Assert(ctx, edgeReq(a, b, "partner_of", gen.Prefix(), "ab"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propAB.ID)
		propAC, err := h.svc.Assert(ctx, edgeReq(a, c, "partner_of", gen.Prefix(), "ac"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propAC.ID)

		ids := []uuid.UUID{propAB.ID, propAC.ID}
		errs := make([]error, 2)
		runConcurrent(2, func(i int) { _, errs[i] = h.svc.Accept(ctx, ids[i], service.AcceptRequest{}) })
		// Both Accept calls may succeed (the second supersedes the first); what
		// matters is exactly one of A's partnerships ends accepted-live. partner_of is
		// symmetric (stored UUID-ordered), so A may be subject OR object — count via
		// ListLiveEdgesForNode (both orientations), not the subject-only current read.
		for i := 0; i < 2; i++ {
			require.NoErrorf(t, errs[i], "accept %d", i)
		}
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, a, "partner_of")
		require.NoError(t, err)
		liveAccepted := 0
		for _, e := range edges {
			if e.Status == repository.AssertionStatusAccepted {
				liveAccepted++
			}
		}
		assert.Equal(t, 1, liveAccepted, "exactly one accepted partnership for A after the race")
	})
}

// TestAssert_ValidTime covers the bi-temporal valid-time branches (future
// successor + rollover, past-bounded backfill, present-edit cancellation).
func TestAssert_ValidTime(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	h, ctx := newAssertHarness(t, ctx0())

	// Case 15 + 15b: future-dated successor bounds the prior (still current); the
	// rollover (after the bound passes) terminalizes it and flips GetCurrentAccepted
	// — but does NOT terminalize a successor-less bounded-past fact.
	t.Run("15 future successor + rollover", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC()

		// NYC current (open).
		nyc, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)

		// LA with a FUTURE valid_from → NYC bounded (valid_to=future, superseded_by=LA)
		// but stays accepted/current; LA accepted-but-not-yet-current.
		future := now.Add(72 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &future
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)

		boundedNYC, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, boundedNYC.Status, "NYC stays accepted until the bound")
		require.NotNil(t, boundedNYC.ValidTo)
		require.NotNil(t, boundedNYC.SupersededBy)
		assert.Equal(t, la.ID, *boundedNYC.SupersededBy)

		// NYC is current now (window contains now); LA is not yet.
		curNow, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, nyc.ID, curNow.ID)

		// Simulate the bound passing: re-bound NYC's valid_to into the PAST (keeping
		// superseded_by) so the rollover's valid_to <= now predicate fires. (This
		// avoids manipulating global accelerated-time env vars under t.Parallel().)
		past := now.Add(-time.Hour)
		rebindTx, err := h.database.Pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, h.assertionRepo.BoundPendingSuccessorTx(ctx, rebindTx, nyc.ID, past, la.ID))
		require.NoError(t, rebindTx.Commit(ctx))

		// Run the rollover.
		n, err := h.svc.RunRollover(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 1)

		rolled, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, rolled.Status, "rollover terminalizes NYC")
		require.NotNil(t, rolled.ClosureReason)
		assert.Equal(t, repository.ClosureReasonSuperseded, *rolled.ClosureReason)
		require.NotNil(t, rolled.KnowledgeTo)
		require.NotNil(t, rolled.SupersededBy)
		assert.Equal(t, la.ID, *rolled.SupersededBy)
		assert.True(t, h.eventExists(t, ctx, supersededSourceID(nyc.ID)), "rollover emits superseded for NYC")

		// LA is now the current accepted (its valid_from is in the future, but
		// GetCurrentAccepted at a time past its valid_from returns it).
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", future.Add(time.Hour))
		require.NoError(t, err)
		assert.Equal(t, la.ID, cur.ID, "LA is current once its valid_from passes")
	})

	// Case 15b: a successor-less bounded-past fact is NOT terminalized by rollover.
	t.Run("15b rollover skips successor-less bounded-past fact", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC()

		// A bounded-past fact (valid window entirely in the past), no successor.
		from := now.Add(-200 * 24 * time.Hour)
		to := now.Add(-100 * 24 * time.Hour)
		boston := textFactReq(subject, "home_address", "Boston", gen.Prefix(), "bos")
		boston.ValidFrom = &from
		boston.ValidTo = &to
		a, err := h.svc.Assert(ctx, boston)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)
		require.Nil(t, a.SupersededBy, "no successor")

		_, err = h.svc.RunRollover(ctx)
		require.NoError(t, err)

		still, err := h.assertionRepo.GetAssertion(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, still.Status, "successor-less bounded-past fact stays accepted")
	})

	// Case 16 + 16b: past-bounded backfill coexists (vs both a present current AND
	// an open-start current row) and never supersedes.
	t.Run("16 past-bounded backfill coexists", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC()

		// Current NYC, open start (a user edit → valid_from NULL).
		nyc, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "NYC current", gen.Prefix(), "nyc"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)
		require.Nil(t, nyc.ValidFrom, "open-start current row")

		// Backfill a past-bounded Boston [now-200d, now-100d).
		from := now.Add(-200 * 24 * time.Hour)
		to := now.Add(-100 * 24 * time.Hour)
		boston := textFactReq(subject, "home_address", "Boston past", gen.Prefix(), "bos")
		boston.ValidFrom = &from
		boston.ValidTo = &to
		bos, err := h.svc.Assert(ctx, boston)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, bos.ID)

		// Boston coexists (accepted), NYC stays current.
		gotBos, err := h.assertionRepo.GetAssertion(ctx, bos.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, gotBos.Status, "backfill coexists")
		gotNYC, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, gotNYC.Status, "open-start current is untouched")
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, nyc.ID, cur.ID, "NYC is still current")
	})

	// Case 16 degenerate-range guard: a now/unknown-start with a past valid_to is
	// rejected (not corrupted).
	t.Run("16 degenerate range rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC()

		// valid_from NULL (→ effective_from = now) but an explicit PAST valid_to.
		past := now.Add(-time.Hour)
		bad := textFactReq(subject, "home_address", "degenerate", gen.Prefix(), "deg")
		bad.ValidTo = &past
		_, err := h.svc.Assert(ctx, bad)
		require.Error(t, err)
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	// Case 16c: a present edit cancels a pending future successor.
	t.Run("16c present edit cancels pending future successor", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC()

		// NYC current, then a FUTURE LA → NYC bounded by LA.
		nyc, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)
		future := now.Add(72 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &future
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)

		// A present Chicago edit → BOTH NYC and the pending LA are superseded by Chicago.
		chi, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "Chicago", gen.Prefix(), "chi"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, chi.ID)

		gotNYC, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, gotNYC.Status, "NYC superseded by present edit")
		require.NotNil(t, gotNYC.SupersededBy)
		assert.Equal(t, chi.ID, *gotNYC.SupersededBy)

		gotLA, err := h.assertionRepo.GetAssertion(ctx, la.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, gotLA.Status, "pending future LA cancelled")
		require.NotNil(t, gotLA.SupersededBy)
		assert.Equal(t, chi.ID, *gotLA.SupersededBy)

		// Chicago is current; LA never becomes current after its date.
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, chi.ID, cur.ID)
		_, err = h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", future.Add(time.Hour))
		// Either Chicago (open) remains current or LA is gone — Chicago's open window
		// covers the future, and LA is superseded, so Chicago is current.
		require.NoError(t, err)
	})

	// Case 17: symmetric-single conflict — partner_of(A,B) then partner_of(B,C)
	// supersedes the A-B partnership (B can have one partner).
	t.Run("17 symmetric-single per-participant conflict", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")
		c := h.seedPerson(t, ctx, gen.Prefix(), "C")

		// partner_of is always-confirm → propose+accept A-B.
		propAB, err := h.svc.Assert(ctx, edgeReq(a, b, "partner_of", gen.Prefix(), "ab"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propAB.ID)
		_, err = h.svc.Accept(ctx, propAB.ID, service.AcceptRequest{})
		require.NoError(t, err)

		// partner_of(B,C): B as subject. Propose+accept → A-B superseded via B.
		propBC, err := h.svc.Assert(ctx, edgeReq(b, c, "partner_of", gen.Prefix(), "bc"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, propBC.ID)
		_, err = h.svc.Accept(ctx, propBC.ID, service.AcceptRequest{})
		require.NoError(t, err)

		closedAB, err := h.assertionRepo.GetAssertion(ctx, propAB.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closedAB.Status, "A-B superseded (B has one partner)")
	})
}

// TestAssert_Validation covers the DB-dependent validation rejections (the pure
// ones are in the service-package unit tests).
func TestAssert_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	h, ctx := newAssertHarness(t, ctx0())

	t.Run("unknown predicate rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		_, err := h.svc.Assert(ctx, textFactReq(subject, "no_such_predicate", "x", gen.Prefix(), "u"))
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("missing subject node rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		_, err := h.svc.Assert(ctx, textFactReq(uuid.New(), "home_address", "x", gen.Prefix(), "m"))
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("missing provenance rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		req := textFactReq(subject, "home_address", "x", gen.Prefix(), "p")
		req.Locators = nil
		_, err := h.svc.Assert(ctx, req)
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("out-of-range confidence rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		req := textFactReq(subject, "home_address", "x", gen.Prefix(), "c")
		req.Confidence = 101
		_, err := h.svc.Assert(ctx, req)
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("fact value type mismatch rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		// birthday expects a date; supply text.
		_, err := h.svc.Assert(ctx, textFactReq(subject, "birthday", "not a date", gen.Prefix(), "b"))
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("edge to wrong object type rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		place := h.seedPlace(t, ctx, gen.Prefix(), "place")
		// partner_of expects a person object; supply a place node.
		_, err := h.svc.Assert(ctx, edgeReq(a, place, "partner_of", gen.Prefix(), "x"))
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("phone_call/calendar_event may not back a fact", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		req := textFactReq(subject, "home_address", "x", gen.Prefix(), "pc")
		req.Locators = []service.ProvenanceLocator{{
			SourceKind:   repository.SourceKindPhoneCall,
			SourceID:     uuid.NewString(),
			ProducerKind: repository.ProducerKindExtractor,
		}}
		_, err := h.svc.Assert(ctx, req)
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("nonexistent content source row rejected", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		req := textFactReq(subject, "home_address", "x", gen.Prefix(), "src")
		req.Locators = []service.ProvenanceLocator{{
			SourceKind:   repository.SourceKindCommsMessage,
			SourceID:     uuid.NewString(), // no such comms_message row
			ProducerKind: repository.ProducerKindExtractor,
		}}
		_, err := h.svc.Assert(ctx, req)
		require.ErrorIs(t, err, service.ErrAssertValidation)
	})

	t.Run("entity-subtype subject accepted (within place->place)", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		child := h.seedPlace(t, ctx, gen.Prefix(), "brooklyn")
		parent := h.seedPlace(t, ctx, gen.Prefix(), "nyc")
		// within: place subject → place object. The validator must accept an
		// entity-subtype subject, not only person.
		a, err := h.svc.Assert(ctx, edgeReq(child, parent, "within", gen.Prefix(), "w"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
	})
}

// runConcurrent runs fn(0..n-1) in n goroutines and waits for all.
func runConcurrent(n int, fn func(i int)) {
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			fn(idx)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
