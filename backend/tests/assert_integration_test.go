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

// acceptedSourceID / supersededSourceID / rejectedSourceID build the event
// source_id keys an assertion's transitions produce.
func acceptedSourceID(id uuid.UUID) string   { return id.String() + ":accepted" }
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

		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
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

		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
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

	// Case 11b: a date-typed fact (birthday) round-trips through the DATE column
	// and GetCurrentAccepted returns the stored date.
	t.Run("11b date fact round-trips", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		bday := time.Date(1990, 4, 21, 0, 0, 0, 0, time.UTC)
		a, err := h.svc.Assert(ctx, service.AssertRequest{
			SubjectNodeID: subject, PredicateKey: "birthday", ValueDate: &bday, Confidence: 90,
			Locators: []service.ProvenanceLocator{userLocator(gen.Prefix(), "bday")},
		})
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, a.ID)
		assert.Equal(t, repository.AssertionStatusAccepted, a.Status)
		require.NotNil(t, a.ValueDate)
		assert.Equal(t, "1990-04-21", a.ValueDate.UTC().Format("2006-01-02"))

		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "birthday", accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond))
		require.NoError(t, err)
		require.NotNil(t, cur.ValueDate)
		assert.Equal(t, "1990-04-21", cur.ValueDate.UTC().Format("2006-01-02"))
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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

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
		_ = n // RunRollover is a GLOBAL sweep; under t.Parallel() a concurrent test can roll this row first, so n is unreliable — the per-row state asserted below is the real check

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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

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
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

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

	// Case 18: a chained future successor bounds the prior pending successor (no
	// gap). NYC current → future LA (July) bounds NYC → future SF (Aug) must BOUND
	// LA to Aug (LA current Jul-Aug), not terminalize it leaving a July gap.
	t.Run("18 chained future successors leave no gap", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		nyc, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)

		julFrom := now.Add(30 * 24 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &julFrom
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)

		augFrom := now.Add(60 * 24 * time.Hour)
		sfReq := textFactReq(subject, "home_address", "SF", gen.Prefix(), "sf")
		sfReq.ValidFrom = &augFrom
		sf, err := h.svc.Assert(ctx, sfReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, sf.ID)

		// LA must be BOUNDED to Aug (still accepted, current Jul-Aug), not superseded.
		gotLA, err := h.assertionRepo.GetAssertion(ctx, la.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, gotLA.Status, "LA stays accepted (current Jul-Aug)")
		require.NotNil(t, gotLA.ValidTo)
		assert.True(t, gotLA.ValidTo.Equal(augFrom), "LA bounded to Aug")
		require.NotNil(t, gotLA.SupersededBy)
		assert.Equal(t, sf.ID, *gotLA.SupersededBy)

		// Mid-July there IS a current value (LA), no gap.
		midJul := now.Add(45 * 24 * time.Hour)
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", midJul)
		require.NoError(t, err, "no gap in July")
		assert.Equal(t, la.ID, cur.ID)
	})

	// Case 19: re-affirming a value bounded by a pending future successor must NOT
	// clear the bound (the P0 — widen must not resurrect the prior past the
	// successor). NYC current → future LA bounds NYC at +30d → re-affirm NYC from a
	// later year bucket → NYC's valid_to bound to LA is preserved.
	t.Run("19 widen does not resurrect prior across pending successor", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		// home_address is year-bucketed. NYC at year Y (valid_from this year).
		thisYear := time.Date(now.Year(), 1, 15, 0, 0, 0, 0, time.UTC)
		nycReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc")
		nycReq.ValidFrom = &thisYear
		nyc, err := h.svc.Assert(ctx, nycReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)

		future := now.Add(30 * 24 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &future
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)

		boundedNYC, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		require.NotNil(t, boundedNYC.ValidTo, "NYC bounded by LA")
		require.NotNil(t, boundedNYC.SupersededBy)

		// Re-affirm NYC from an EARLIER (last) year bucket — a same-value
		// reaffirmation whose window OVERLAPS the bounded NYC (so it actually hits
		// widenReaffirmation). It must widen NYC's lower bound backward but NOT clear
		// NYC's pending upper bound (LA).
		lastYear := time.Date(now.Year()-1, 6, 1, 0, 0, 0, 0, time.UTC)
		reaffirm := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc2")
		reaffirm.ValidFrom = &lastYear
		_, err = h.svc.Assert(ctx, reaffirm)
		require.NoError(t, err)

		afterReaffirm, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		require.NotNil(t, afterReaffirm.ValidTo, "NYC's pending-successor bound is preserved")
		assert.True(t, afterReaffirm.ValidTo.Equal(*boundedNYC.ValidTo), "valid_to bound unchanged by the reaffirmation")
		require.NotNil(t, afterReaffirm.ValidFrom)
		assert.True(t, afterReaffirm.ValidFrom.Equal(lastYear), "lower bound widened backward to the new evidence")
		// LA stays the pending future successor; exactly one current now (NYC).
		curNow, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, nyc.ID, curNow.ID, "exactly NYC is current now")
		_ = la
	})

	// Case 24: a reaffirmation that BRIDGES two non-contiguous same-value stints
	// merges ALL of them into one survivor (not just the first), so the
	// single-cardinality slot keeps exactly one live row. home_address year-bucketed.
	t.Run("24 bridging reaffirmation merges all same-value stints", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		// Two non-contiguous past NYC stints (both bounded-past → coexist).
		yA0 := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
		yA1 := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
		a := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "a")
		a.ValidFrom, a.ValidTo = &yA0, &yA1
		rowA, err := h.svc.Assert(ctx, a)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, rowA.ID)

		yB0 := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		yB1 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		b := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "b")
		b.ValidFrom, b.ValidTo = &yB0, &yB1
		rowB, err := h.svc.Assert(ctx, b)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, rowB.ID)
		require.NotEqual(t, rowA.ID, rowB.ID, "distinct year buckets → two coexisting stints")

		// A bridging NYC [2012,2019) overlaps BOTH stints → all collapse to one row.
		yBridge0 := time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC)
		yBridge1 := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
		bridge := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "br")
		bridge.ValidFrom, bridge.ValidTo = &yBridge0, &yBridge1
		survivor, err := h.svc.Assert(ctx, bridge)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, survivor.ID)

		// Exactly ONE live NYC row remains; the other stint is superseded into it.
		live, err := h.assertionRepo.ListLiveEdgesForNode(ctx, subject, "home_address")
		require.NoError(t, err)
		liveCount := 0
		for _, e := range live {
			if e.Status == repository.AssertionStatusAccepted {
				liveCount++
			}
		}
		assert.Equal(t, 1, liveCount, "bridged same-value stints collapse to one live row")
		assert.Equal(t, rowA.ID, survivor.ID, "survivor is the first matched stint, widened")
	})

	// Case 25: the rollover sweep does NOT abort on a bounded row whose
	// knowledge_from was set in the future (KnowledgeFromOverride); knowledge_to is
	// clamped via GREATEST so the assertion_knowledge_range CHECK holds.
	t.Run("25 rollover clamps knowledge_to for future-knowledge row", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		// NYC current with a FUTURE knowledge_from override.
		tomorrow := now.Add(24 * time.Hour)
		nycReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc")
		nycReq.KnowledgeFromOverride = &tomorrow
		nyc, err := h.svc.Assert(ctx, nycReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)
		require.True(t, nyc.KnowledgeFrom.After(now), "knowledge_from is in the future")

		// Future LA bounds NYC (pending successor).
		future := now.Add(72 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &future
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)

		// Re-bind NYC's valid_to into the past (simulate the bound passing).
		past := now.Add(-time.Hour)
		rebind, err := h.database.Pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, h.assertionRepo.BoundPendingSuccessorTx(ctx, rebind, nyc.ID, past, la.ID))
		require.NoError(t, rebind.Commit(ctx))

		// Rollover must succeed (not abort on the CHECK) and clamp knowledge_to.
		n, err := h.svc.RunRollover(ctx)
		require.NoError(t, err, "rollover must not abort on a future-knowledge_from row")
		_ = n // RunRollover is a GLOBAL sweep; under t.Parallel() a concurrent test can roll this row first, so n is unreliable — the per-row state asserted below is the real check
		rolled, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, rolled.Status)
		require.NotNil(t, rolled.KnowledgeTo)
		assert.False(t, rolled.KnowledgeTo.Before(rolled.KnowledgeFrom), "knowledge_to >= knowledge_from (clamped)")
	})
}

// TestAssert_Lifecycle covers the lifecycle-transition concurrency + same-value
// accept-time behaviors the review surfaced.
func TestAssert_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	h, ctx := newAssertHarness(t, ctx0())

	// Case 20: concurrent Accept + Reject of the SAME proposed row — exactly one
	// transition wins (the row-lock makes the from-status check atomic).
	t.Run("20 concurrent Accept and Reject race", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")

		prop, err := h.svc.Assert(ctx, textFactReq(subject, "health_condition", "race", gen.Prefix(), "r"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, prop.ID)
		require.Equal(t, repository.AssertionStatusProposed, prop.Status)

		errs := make([]error, 2)
		runConcurrent(2, func(i int) {
			if i == 0 {
				_, errs[i] = h.svc.Accept(ctx, prop.ID, service.AcceptRequest{})
			} else {
				_, errs[i] = h.svc.Reject(ctx, prop.ID, service.RejectRequest{})
			}
		})
		// Exactly one succeeds; the other fails the from-status precondition.
		successes := 0
		for _, e := range errs {
			if e == nil {
				successes++
			} else {
				require.ErrorIs(t, e, service.ErrAssertValidation, "the loser fails the status precondition")
			}
		}
		assert.Equal(t, 1, successes, "exactly one transition wins")

		// The row ends in a terminal state consistent with the winner (not both).
		final, err := h.assertionRepo.GetAssertion(ctx, prop.ID)
		require.NoError(t, err)
		assert.Contains(t, []string{repository.AssertionStatusAccepted, repository.AssertionStatusRejected}, final.Status)
	})

	// Case 21: Accept of a same-value proposal in a different bucket WIDENS the
	// existing accepted row (does not supersede it). home_address is year-bucketed +
	// auto-apply, so use a force-confirm proposal to land it 'proposed' first.
	t.Run("21 Accept same-value different-bucket widens", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		// Accepted NYC this year (open-ended, auto-apply), confidence 50.
		thisYear := time.Date(now.Year(), 2, 1, 0, 0, 0, 0, time.UTC)
		nycReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc")
		nycReq.ValidFrom = &thisYear
		nycReq.Confidence = 50
		accepted, err := h.svc.Assert(ctx, nycReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, accepted.ID)
		require.Equal(t, repository.AssertionStatusAccepted, accepted.Status)
		require.EqualValues(t, 50, accepted.Confidence)

		// A force-confirm same-value NYC proposal in NEXT year's bucket → proposed,
		// confidence 95 (higher, so the merge must raise the survivor's confidence).
		nextYear := time.Date(now.Year()+1, 3, 1, 0, 0, 0, 0, time.UTC)
		propReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc2")
		propReq.ValidFrom = &nextYear
		propReq.ForceConfirm = true
		propReq.Confidence = 95
		proposed, err := h.svc.Assert(ctx, propReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, proposed.ID)
		require.Equal(t, repository.AssertionStatusProposed, proposed.Status)
		require.NotEqual(t, accepted.ID, proposed.ID, "different bucket → distinct proposed row")

		// Accept the proposal → it must WIDEN the existing accepted row (absorb the
		// proposal), NOT supersede it. The accepted NYC stays accepted-live.
		survivor, err := h.svc.Accept(ctx, proposed.ID, service.AcceptRequest{})
		require.NoError(t, err)
		assert.Equal(t, accepted.ID, survivor.ID, "Accept returns the widened survivor, not a superseded row")

		gotAccepted, err := h.assertionRepo.GetAssertion(ctx, accepted.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, gotAccepted.Status, "existing NYC stays accepted (widened)")
		assert.EqualValues(t, 95, gotAccepted.Confidence, "merge folds the higher loser confidence into the survivor")
		gotProposed, err := h.assertionRepo.GetAssertion(ctx, proposed.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, gotProposed.Status, "the proposal is absorbed (superseded)")
		// Exactly one live home_address proposition for the subject.
		live, err := h.assertionRepo.ListLiveEdgesForNode(ctx, subject, "home_address")
		require.NoError(t, err)
		require.Len(t, live, 1)
		assert.Equal(t, accepted.ID, live[0].ID)
	})

	// Case 22: KnowledgeFromOverride in the future + a terminal transition clamps
	// knowledge_to >= knowledge_from (no assertion_knowledge_range violation).
	t.Run("22 terminal transition clamps knowledge_to for future override", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		tomorrow := now.Add(24 * time.Hour)
		req := textFactReq(subject, "health_condition", "future-knowledge", gen.Prefix(), "fk")
		req.KnowledgeFromOverride = &tomorrow
		prop, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, prop.ID)
		require.True(t, prop.KnowledgeFrom.After(now), "knowledge_from is in the future")

		// Reject today → knowledge_to must be clamped to >= knowledge_from (no CHECK
		// violation).
		rejected, err := h.svc.Reject(ctx, prop.ID, service.RejectRequest{})
		require.NoError(t, err)
		require.NotNil(t, rejected.KnowledgeTo)
		assert.False(t, rejected.KnowledgeTo.Before(rejected.KnowledgeFrom), "knowledge_to >= knowledge_from")
	})

	// Case 23: accept-time same-value widen must NOT clear a pending-successor bound
	// (the accept-path analogue of the P0). NYC accepted (bounded by a future LA),
	// then a same-value force-confirm NYC proposal whose window overlaps NYC is
	// accepted → it widens NYC backward but keeps NYC's bound to LA; exactly one
	// current now, and LA still becomes current after its date.
	t.Run("23 accept-time widen preserves pending-successor bound", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		thisYear := time.Date(now.Year(), 2, 1, 0, 0, 0, 0, time.UTC)
		nycReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc")
		nycReq.ValidFrom = &thisYear
		nyc, err := h.svc.Assert(ctx, nycReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, nyc.ID)

		future := now.Add(30 * 24 * time.Hour)
		laReq := textFactReq(subject, "home_address", "LA", gen.Prefix(), "la")
		laReq.ValidFrom = &future
		la, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, la.ID)
		boundedNYC, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		require.NotNil(t, boundedNYC.ValidTo)
		require.NotNil(t, boundedNYC.SupersededBy)

		// A force-confirm same-value NYC proposal in last year's bucket (overlaps NYC).
		lastYear := time.Date(now.Year()-1, 6, 1, 0, 0, 0, 0, time.UTC)
		propReq := textFactReq(subject, "home_address", "NYC", gen.Prefix(), "nyc2")
		propReq.ValidFrom = &lastYear
		propReq.ForceConfirm = true
		proposed, err := h.svc.Assert(ctx, propReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, proposed.ID)
		require.Equal(t, repository.AssertionStatusProposed, proposed.Status)

		survivor, err := h.svc.Accept(ctx, proposed.ID, service.AcceptRequest{})
		require.NoError(t, err)
		assert.Equal(t, nyc.ID, survivor.ID, "Accept widens into NYC")

		afterAccept, err := h.assertionRepo.GetAssertion(ctx, nyc.ID)
		require.NoError(t, err)
		require.NotNil(t, afterAccept.ValidTo, "NYC's pending-successor bound is preserved by accept-time widen")
		assert.True(t, afterAccept.ValidTo.Equal(*boundedNYC.ValidTo), "valid_to bound unchanged")

		// Exactly one current now (NYC); LA still becomes current after its date.
		curNow, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", now)
		require.NoError(t, err)
		assert.Equal(t, nyc.ID, curNow.ID)
		curFuture, err := h.assertionRepo.GetCurrentAccepted(ctx, subject, "home_address", future.Add(time.Hour))
		require.NoError(t, err)
		assert.Equal(t, la.ID, curFuture.ID, "LA becomes current after its date (bound intact)")
	})

	// Case 26: AssertClosure on a current row whose knowledge_from is in the future
	// (KnowledgeFromOverride) clamps knowledge_to so the closure does not violate
	// assertion_knowledge_range.
	t.Run("26 closure clamps knowledge_to for future-knowledge row", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		tomorrow := now.Add(24 * time.Hour)
		req := textFactReq(subject, "home_address", "future-k", gen.Prefix(), "fk")
		req.KnowledgeFromOverride = &tomorrow
		current, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, current.ID)
		require.True(t, current.KnowledgeFrom.After(now))

		// Closure today must succeed (clamped knowledge_to), not violate the CHECK.
		require.NoError(t, h.svc.AssertClosure(ctx, service.ClosureRequest{SubjectNodeID: subject, PredicateKey: "home_address"}))
		closed, err := h.assertionRepo.GetAssertion(ctx, current.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
		require.NotNil(t, closed.KnowledgeTo)
		assert.False(t, closed.KnowledgeTo.Before(closed.KnowledgeFrom), "knowledge_to >= knowledge_from (clamped)")
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

	t.Run("degenerate range rejected even when a live row would corroborate", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subject := h.seedPerson(t, ctx, gen.Prefix(), "subj")
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)

		// An open-start accepted row that the degenerate write would dedup-match.
		first, err := h.svc.Assert(ctx, textFactReq(subject, "home_address", "same", gen.Prefix(), "d1"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, first.ID)

		// SAME value, but valid_to=yesterday with NULL valid_from → effective_from=now,
		// valid_to <= effective_from → degenerate. It must be REJECTED in validation
		// (before the dedup lookup), NOT silently corroborated.
		past := now.Add(-24 * time.Hour)
		bad := textFactReq(subject, "home_address", "same", gen.Prefix(), "d2")
		bad.ValidTo = &past
		_, err = h.svc.Assert(ctx, bad)
		require.ErrorIs(t, err, service.ErrAssertValidation, "degenerate range rejected pre-dedup")
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
