//go:build integration_testdb

package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeNodeHarness bundles a fully-wired ContactService (knowledge writer +
// cadence) and the graph repos a merge/soft-delete test reads back. Created
// contacts + place nodes are tracked for FK-ordered cleanup (assertion → node FK
// is restrict, so assertions clear before nodes).
type mergeNodeHarness struct {
	database      *db.Database
	contactSvc    *service.ContactService
	assertSvc     *service.AssertService
	contactRepo   *repository.ContactRepository
	assertionRepo *repository.AssertionRepository
	nodeRepo      *repository.NodeRepository
	entityRepo    *repository.EntityRepository
	eventRepo     *repository.EventRepository
	support       *repository.SyntheticSupportRepository

	contactIDs []uuid.UUID
	placeNorms []string
}

func newMergeNodeHarness(t *testing.T, ctx context.Context) *mergeNodeHarness {
	t.Helper()
	database, cfg := newSharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// Insert-only river client (no fetch loop): AssertService.PublishTx enqueues
	// assertion.* jobs the no-op worker registers; the cache is filled inline.
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)

	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	cacheUpdater := consumer.NewKnowledgeCacheUpdater(assertionRepo, nodeRepo, contactRepo)

	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
	contactSvc.SetKnowledgeWriter(assertSvc, cacheUpdater)
	wireCadenceUpdaterForTest(t, database, contactSvc)

	h := &mergeNodeHarness{
		database:      database,
		contactSvc:    contactSvc,
		assertSvc:     assertSvc,
		contactRepo:   contactRepo,
		assertionRepo: assertionRepo,
		nodeRepo:      nodeRepo,
		entityRepo:    entityRepo,
		eventRepo:     eventRepo,
		support:       support,
	}
	h.registerCleanup(t, ctx)
	return h
}

func (h *mergeNodeHarness) track(id uuid.UUID)     { h.contactIDs = append(h.contactIDs, id) }
func (h *mergeNodeHarness) trackPlace(norm string) { h.placeNorms = append(h.placeNorms, norm) }

func (h *mergeNodeHarness) registerCleanup(t *testing.T, ctx context.Context) {
	t.Cleanup(func() {
		for _, cid := range h.contactIDs {
			assertions, _ := h.assertionRepo.ListAssertionsBySubject(ctx, cid)
			for _, a := range assertions {
				_ = h.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, "assertion", a.ID.String())
			}
			_, _ = h.support.DeleteAssertionsForNode(ctx, cid)
		}
		var placeNodeIDs []uuid.UUID
		for _, norm := range h.placeNorms {
			entity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypePlace, norm)
			if err == nil {
				placeNodeIDs = append(placeNodeIDs, entity.NodeID)
			}
		}
		_, _ = h.support.DeleteNodesByIds(ctx, placeNodeIDs)
		_, _ = h.support.DeleteNodesByIds(ctx, h.contactIDs)
		for _, cid := range h.contactIDs {
			_ = h.contactRepo.HardDeleteContact(ctx, cid)
		}
	})
}

// createContactWithLocation creates a contact with a location, which the cutover
// emits as a lives_in assertion + place node. Returns the created contact.
func (h *mergeNodeHarness) createContactWithLocation(t *testing.T, ctx context.Context, name, location string) *repository.Contact {
	t.Helper()
	loc := location
	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: name,
		Location: &loc,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)
	// trackPlace records the place's normalized dedup key (lower+trim, mirroring
	// EnsurePlaceTx) so cleanup resolves the entity node.
	h.trackPlace(strings.ToLower(strings.TrimSpace(location)))
	return contact
}

func nowMicro() time.Time {
	return accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
}

// TestContactMergeNode_Integration exercises the contact-merge node integration:
// the loser node is tombstoned, its assertions re-point onto the winner, and the
// single-cardinality slot ends with exactly one live assertion (D9 / PR10).
func TestContactMergeNode_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	// Case 1: merge two contacts each with a DIFFERENT lives_in → loser node
	// merged_into/deleted_at set; exactly one live lives_in remains on the winner
	// (the re-pointed loser overlaps the winner slot → valid-time supersession).
	t.Run("1 merge different lives_in collapses to one live edge", func(t *testing.T) {
		t.Parallel()
		h := newMergeNodeHarness(t, ctx)
		gen, _ := migrationGenerator(t)

		winner := h.createContactWithLocation(t, ctx, gen.Prefix()+"winner", gen.Prefix()+"NYC")
		loser := h.createContactWithLocation(t, ctx, gen.Prefix()+"loser", gen.Prefix()+"LA")

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: loser.ID,
			TargetContactID: winner.ID,
		})
		require.NoError(t, err)

		// Loser node is tombstoned (merged_into=winner, deleted_at set).
		loserNode, err := h.nodeRepo.GetNodeIncludingDeleted(ctx, loser.ID)
		require.NoError(t, err)
		require.NotNil(t, loserNode.MergedInto, "loser node merged_into set")
		assert.Equal(t, winner.ID, *loserNode.MergedInto)
		require.NotNil(t, loserNode.DeletedAt, "loser node deleted_at set")

		// Exactly one live lives_in for the winner.
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		assert.Len(t, edges, 1, "winner has exactly one live lives_in after merge")

		// And it is the CURRENT accepted slot value on the winner.
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, winner.ID, repository.PredicateLivesIn, nowMicro())
		require.NoError(t, err)
		assert.Equal(t, winner.ID, cur.SubjectNodeID)

		// No field selection → the target's kept value (NYC) is authoritative: the
		// field-selection apply runs AFTER the source re-point, so the chosen value
		// wins and the cache column reflects it (cache/store consistency).
		merged, err := h.contactRepo.GetContact(ctx, winner.ID)
		require.NoError(t, err)
		require.NotNil(t, merged.Location)
		assert.Equal(t, gen.Prefix()+"NYC", *merged.Location, "kept target location is current after merge")
	})

	// Case 1b: target has NO location, source HAS one → the merged contact INHERITS
	// the source's location (D9 migrates the loser's knowledge onto the survivor;
	// field-selection governs CONFLICTING values, not gap-filling an empty target).
	// The cache column reflects the inherited value (store/cache consistency).
	t.Run("1b empty target inherits source knowledge (gap-fill)", func(t *testing.T) {
		t.Parallel()
		h := newMergeNodeHarness(t, ctx)
		gen, _ := migrationGenerator(t)

		winner, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: gen.Prefix() + "winner-noloc",
		}, nil)
		require.NoError(t, err)
		h.track(winner.ID)
		loser := h.createContactWithLocation(t, ctx, gen.Prefix()+"loser", gen.Prefix()+"Denver")

		_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: loser.ID,
			TargetContactID: winner.ID,
		})
		require.NoError(t, err)

		// The winner now has the source's lives_in (one live edge) + cache column.
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		assert.Len(t, edges, 1, "merged contact inherits the source lives_in edge")
		merged, err := h.contactRepo.GetContact(ctx, winner.ID)
		require.NoError(t, err)
		require.NotNil(t, merged.Location, "empty target inherits source location")
		assert.Equal(t, gen.Prefix()+"Denver", *merged.Location)
	})

	// Case 2: merge two contacts with the SAME lives_in value (collision) →
	// provenance merged onto the winner's assertion, loser row closed superseded,
	// exactly one live row, no 23505.
	t.Run("2 merge same lives_in collision merges provenance", func(t *testing.T) {
		t.Parallel()
		h := newMergeNodeHarness(t, ctx)
		gen, _ := migrationGenerator(t)

		place := gen.Prefix() + "Boston"
		winner := h.createContactWithLocation(t, ctx, gen.Prefix()+"winner", place)
		loser := h.createContactWithLocation(t, ctx, gen.Prefix()+"loser", place)

		// Capture the loser's pre-merge lives_in id so we can assert it closed.
		loserEdges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, loser.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		require.Len(t, loserEdges, 1)
		loserEdgeID := loserEdges[0].ID

		winnerEdges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		require.Len(t, winnerEdges, 1)
		winnerEdgeID := winnerEdges[0].ID

		_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: loser.ID,
			TargetContactID: winner.ID,
		})
		require.NoError(t, err, "same-value merge must not 23505")

		// Exactly one live lives_in for the winner (the survivor).
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assert.Equal(t, winnerEdgeID, edges[0].ID, "winner's own assertion survives")

		// The loser's lives_in row is closed superseded with superseded_by=winner.
		closed, err := h.assertionRepo.GetAssertion(ctx, loserEdgeID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
		require.NotNil(t, closed.SupersededBy)
		assert.Equal(t, winnerEdgeID, *closed.SupersededBy)

		// The survivor carries BOTH provenance locators (loser's moved onto it).
		provs, err := h.assertionRepo.ListProvenance(ctx, winnerEdgeID)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(provs), 2, "loser provenance merged onto the winner assertion")
	})

	// Case 3: soft-delete a contact → its node.deleted_at is set; its assertions
	// remain in the table but drop from live (deleted_at IS NULL) reads.
	t.Run("3 soft-delete propagates node tombstone and drops live reads", func(t *testing.T) {
		t.Parallel()
		h := newMergeNodeHarness(t, ctx)
		gen, _ := migrationGenerator(t)

		contact := h.createContactWithLocation(t, ctx, gen.Prefix()+"deleted", gen.Prefix()+"Austin")

		// Live read before delete: one lives_in edge.
		before, err := h.assertionRepo.ListLiveEdgesForNode(ctx, contact.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		require.Len(t, before, 1)
		edgeID := before[0].ID

		require.NoError(t, h.contactSvc.DeleteContact(ctx, contact.ID))

		// Node tombstoned.
		node, err := h.nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID)
		require.NoError(t, err)
		require.NotNil(t, node.DeletedAt, "node deleted_at set on contact soft-delete")
		_, err = h.nodeRepo.GetNode(ctx, contact.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "live node read excludes the tombstoned node")

		// The assertion row is RETAINED in the table (the tombstone is on the NODE,
		// not the assertion).
		retained, err := h.assertionRepo.GetAssertion(ctx, edgeID)
		require.NoError(t, err)
		assert.Equal(t, edgeID, retained.ID, "assertion retained after soft-delete")
		n, err := h.support.CountAssertionsForSubject(ctx, contact.ID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "assertion remains in the table after soft-delete")

		// But graph LIVE reads drop it: the lives_in edge and the current-accepted
		// value are gone once the contact node is tombstoned.
		after, err := h.assertionRepo.ListLiveEdgesForNode(ctx, contact.ID, repository.PredicateLivesIn)
		require.NoError(t, err)
		assert.Empty(t, after, "lives_in edge drops from live reads after node tombstone")
		_, err = h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, nowMicro())
		assert.ErrorIs(t, err, db.ErrNotFound, "current-accepted drops from live reads after node tombstone")
	})
}

// mergeAssertionsInTx runs the graph node-merge (tombstone loser + re-point its
// assertions onto the winner) in one tx, mirroring what ContactService.MergeContacts
// does, so the AssertService-level cases can exercise MergeAssertionsTx directly.
func mergeAssertionsInTx(t *testing.T, ctx context.Context, h *assertHarness, loser, winner uuid.UUID) {
	t.Helper()
	err := pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := h.nodeRepo.SetNodeMergedIntoTx(ctx, tx, loser, winner); err != nil {
			return err
		}
		return h.svc.MergeAssertionsTx(ctx, tx, loser, winner)
	})
	require.NoError(t, err)
}

// liveEdgesToObject returns the live edges of a predicate from subject pointing at
// a specific object node (a node may have several live edges of one predicate; this
// scopes to one object so a same-value widen assertion targets the right row).
func liveEdgesToObject(t *testing.T, ctx context.Context, h *assertHarness, subject uuid.UUID, predicate string, object uuid.UUID) []repository.Assertion {
	t.Helper()
	all, err := h.assertionRepo.ListLiveEdgesForNode(ctx, subject, predicate)
	require.NoError(t, err)
	var out []repository.Assertion
	for _, e := range all {
		if e.SubjectNodeID == subject && e.ObjectNodeID != nil && *e.ObjectNodeID == object {
			out = append(out, e)
		}
	}
	return out
}

// TestMergeAssertions_Integration exercises MergeAssertionsTx directly (the graph
// re-point primitive) for the cases the contact-profile predicates can't reach:
// the object-side slot-lock case and the latent-person promotion mechanic (D9).
func TestMergeAssertions_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	// Case 4: the loser is the OBJECT of introduced_by(A, loser) (single, asymmetric).
	// After loser→winner the slot belongs to A — NOT the winner — so the merge must
	// lock A's slot and re-point the object correctly to introduced_by(A, winner).
	t.Run("4 object-side introduced_by re-points to A's slot", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		a := h.seedPerson(t, ctx, gen.Prefix(), "introducer-A")
		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser-B")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner-C")

		// introduced_by(A, loser): A is subject, loser is object.
		edge, err := h.svc.Assert(ctx, edgeReq(a, loser, "introduced_by", gen.Prefix(), "intro"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)
		require.Equal(t, a, edge.SubjectNodeID)
		require.NotNil(t, edge.ObjectNodeID)
		require.Equal(t, loser, *edge.ObjectNodeID)

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		// The edge now reads introduced_by(A, winner): same row, object re-pointed.
		repointed, err := h.assertionRepo.GetAssertion(ctx, edge.ID)
		require.NoError(t, err)
		assert.Equal(t, a, repointed.SubjectNodeID, "subject A unchanged")
		require.NotNil(t, repointed.ObjectNodeID)
		assert.Equal(t, winner, *repointed.ObjectNodeID, "object re-pointed loser→winner")
		assert.Equal(t, repository.AssertionStatusAccepted, repointed.Status, "still live/accepted")

		// Exactly one live introduced_by edge touches A, pointing at the winner.
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, a, "introduced_by")
		require.NoError(t, err)
		require.Len(t, edges, 1)
		require.NotNil(t, edges[0].ObjectNodeID)
		assert.Equal(t, winner, *edges[0].ObjectNodeID)
		// No live edge dangles on the merged-away loser.
		loserEdges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, loser, "introduced_by")
		require.NoError(t, err)
		assert.Empty(t, loserEdges, "no live edge references the merged-away loser")
	})

	// Case 5: a latent person edge (knows → a non-CRM person via EnsureLatentPerson),
	// then promote the latent node by adding a contact row at its id. The contact
	// exists at that id and the edge is intact.
	t.Run("5 latent person edge then promote at node id", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		subject := h.seedPerson(t, ctx, gen.Prefix(), "knower")

		// Mint a latent person node + a knows edge to it, in one tx.
		var latentID uuid.UUID
		var edgeID uuid.UUID
		err := pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			id, err := h.svc.EnsureLatentPerson(ctx, tx, gen.Prefix()+"latent-friend")
			if err != nil {
				return err
			}
			latentID = id
			obj := latentID
			edge, err := h.svc.AssertTx(ctx, tx, service.AssertRequest{
				SubjectNodeID: subject,
				PredicateKey:  "knows",
				ObjectNodeID:  &obj,
				Confidence:    80,
				Locators:      []service.ProvenanceLocator{userLocator(gen.Prefix(), "knows")},
			})
			if err != nil {
				return err
			}
			edgeID = edge.ID
			return nil
		})
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edgeID)
		// Clean the latent node's assertions + the node itself (it has no contact row
		// unless promoted; the promote below adds one we hard-delete in cleanup).
		t.Cleanup(func() { _, _ = h.support.DeleteAssertionsForNode(ctx, latentID) })

		// The latent node exists as a person node with NO contact row.
		latentNode, err := h.nodeRepo.GetNode(ctx, latentID)
		require.NoError(t, err)
		assert.Equal(t, repository.NodeTypePerson, latentNode.Type)
		_, err = h.support.GetNodeForContact(ctx, latentID) // live node read, fine
		require.NoError(t, err)

		// Promote: add a contact row AT the latent node's id.
		require.NoError(t, h.support.InsertContactAtID(ctx, latentID, gen.Prefix()+"promoted-friend"))
		contactRepo := repository.NewContactRepository(h.database.Queries)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, latentID) })

		promoted, err := contactRepo.GetContact(ctx, latentID)
		require.NoError(t, err)
		assert.Equal(t, latentID, promoted.ID, "contact exists at the latent node id")

		// The knows edge is intact and still points at the (now promoted) node.
		edge, err := h.assertionRepo.GetAssertion(ctx, edgeID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, edge.Status)
		// knows is symmetric → the pair is stored UUID-ordered; the latent id is one
		// of the two participants regardless of orientation.
		touchesLatent := edge.SubjectNodeID == latentID ||
			(edge.ObjectNodeID != nil && *edge.ObjectNodeID == latentID)
		assert.True(t, touchesLatent, "knows edge still references the promoted node")
	})

	// Case 6: merging two people who knows() EACH OTHER collapses the between-edge
	// into a self-edge → it must be CLOSED, not re-pointed into knows(winner, winner).
	t.Run("6 edge between loser and winner closes (no self-loop)", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")

		edge, err := h.svc.Assert(ctx, edgeReq(loser, winner, "knows", gen.Prefix(), "knows"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		// The between-edge is closed (superseded), NOT re-pointed to a self-loop.
		closed, err := h.assertionRepo.GetAssertion(ctx, edge.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status, "between-edge closed on merge")
		require.NotNil(t, closed.KnowledgeTo)
		// No live knows edge on the winner referencing itself.
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner, "knows")
		require.NoError(t, err)
		for _, e := range edges {
			selfLoop := e.SubjectNodeID == winner && e.ObjectNodeID != nil && *e.ObjectNodeID == winner
			assert.False(t, selfLoop, "no live self-loop knows(winner, winner)")
		}
	})

	// Case 7: a live node A knows() a node B that is then soft-deleted → the edge
	// drops from A's live edge read (an edge is live only when BOTH endpoints are).
	t.Run("7 edge to a soft-deleted node drops from live reads", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")
		edge, err := h.svc.Assert(ctx, edgeReq(a, b, "knows", gen.Prefix(), "knows"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)

		before, err := h.assertionRepo.ListLiveEdgesForNode(ctx, a, "knows")
		require.NoError(t, err)
		require.Len(t, before, 1)

		require.NoError(t, h.nodeRepo.SoftDeleteNode(ctx, b))

		after, err := h.assertionRepo.ListLiveEdgesForNode(ctx, a, "knows")
		require.NoError(t, err)
		assert.Empty(t, after, "edge to a soft-deleted endpoint drops from live reads")
	})

	// Case 8: merge two nodes each with lives_in(SAME place) in DIFFERENT valid-time
	// buckets (year) → the re-pointed loser does NOT collide on proposition_key (the
	// bucket differs) but is the SAME fact → it WIDENS/merges with the winner's row
	// (one live edge, union window), NOT a supersession.
	t.Run("8 same-value different-bucket merge widens not supersedes", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")
		place := h.seedPlace(t, ctx, gen.Prefix(), "shared-place")

		from2018 := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		from2024 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

		loserReq := edgeReq(loser, place, "lives_in", gen.Prefix(), "loser-lives")
		loserReq.ValidFrom = &from2018
		loserEdge, err := h.svc.Assert(ctx, loserReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, loserEdge.ID)

		winnerReq := edgeReq(winner, place, "lives_in", gen.Prefix(), "winner-lives")
		winnerReq.ValidFrom = &from2024
		winnerEdge, err := h.svc.Assert(ctx, winnerReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, winnerEdge.ID)
		require.NotEqual(t, loserEdge.ID, winnerEdge.ID, "different buckets → two distinct propositions pre-merge")

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		// Exactly one live lives_in for the winner (same place, the union of the two
		// stints), NOT two competing accepted rows and NOT a supersession that loses
		// one stint.
		edges, err := h.assertionRepo.ListLiveEdgesForNode(ctx, winner, "lives_in")
		require.NoError(t, err)
		assert.Len(t, edges, 1, "same-value stints collapse to one live edge")
		// The survivor's window covers the earlier (2018) stint (widened backward).
		require.NotNil(t, edges[0].ValidFrom)
		assert.False(t, edges[0].ValidFrom.After(from2018), "survivor window widened to cover the earlier stint")
	})

	// Case 9: a FUTURE-dated edge between loser and winner (valid_from > now)
	// collapses to a self-loop on merge → closing it must NOT stamp valid_to=now
	// (which would be < valid_from and violate assertion_valid_range).
	t.Run("9 future-dated self-loop closes without range violation", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")

		future := accelerated.GetCurrentTime().UTC().Add(72 * time.Hour)
		req := edgeReq(loser, winner, "knows", gen.Prefix(), "future-knows")
		req.ValidFrom = &future
		edge, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)

		// Must not error on the assertion_valid_range CHECK.
		mergeAssertionsInTx(t, ctx, h, loser, winner)

		closed, err := h.assertionRepo.GetAssertion(ctx, edge.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status, "future-dated self-loop closed")
	})

	// Case 11: a same-value widen during merge must NOT extend past the survivor's
	// (or an absorbed row's) pending-future-successor bound, or the survivor and that
	// successor would BOTH be current once the successor's date passes.
	t.Run("11 widen caps at a pending-successor bound", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")
		nyc := h.seedPlace(t, ctx, gen.Prefix(), "nyc")
		la := h.seedPlace(t, ctx, gen.Prefix(), "la")

		// Winner: lives_in(NYC) from 2022, then a FUTURE lives_in(LA) → NYC is bounded
		// (valid_to=+3w, superseded_by=LA) but stays accepted; LA is future-accepted.
		// (An explicit 2022 start, not open, keeps the post-merge union start defined.)
		winnerNYCReq := edgeReq(winner, nyc, "lives_in", gen.Prefix(), "w-nyc")
		from2022 := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
		winnerNYCReq.ValidFrom = &from2022
		winnerNYC, err := h.svc.Assert(ctx, winnerNYCReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, winnerNYC.ID)
		future := accelerated.GetCurrentTime().UTC().Add(3 * 7 * 24 * time.Hour)
		laReq := edgeReq(winner, la, "lives_in", gen.Prefix(), "w-la")
		laReq.ValidFrom = &future
		winnerLA, err := h.svc.Assert(ctx, laReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, winnerLA.ID)

		boundedNYC, err := h.assertionRepo.GetAssertion(ctx, winnerNYC.ID)
		require.NoError(t, err)
		require.NotNil(t, boundedNYC.SupersededBy, "winner NYC bounded by pending LA")
		require.NotNil(t, boundedNYC.ValidTo)

		// Loser: lives_in(NYC) in an EARLIER year bucket (so no proposition_key
		// collision with the winner's NYC; it is the same fact in another bucket).
		past := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		loserReq := edgeReq(loser, nyc, "lives_in", gen.Prefix(), "l-nyc")
		loserReq.ValidFrom = &past
		loserNYC, err := h.svc.Assert(ctx, loserReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, loserNYC.ID)

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		// Exactly ONE live NYC edge survives on the winner (the absorbed winner-NYC is
		// closed). The survivor is widened backward to cover 2018 but its valid_to is
		// NOT extended past the pending-LA bound, and it INHERITS the superseded_by
		// linkage so the rollover terminalizes it when LA's date arrives — NYC and LA
		// never both become current.
		liveNYC := liveEdgesToObject(t, ctx, h, winner, "lives_in", nyc)
		require.Len(t, liveNYC, 1, "exactly one live NYC after the same-value widen")
		survivor, err := h.assertionRepo.GetAssertion(ctx, liveNYC[0].ID)
		require.NoError(t, err)
		require.NotNil(t, survivor.ValidFrom)
		assert.False(t, survivor.ValidFrom.After(past), "survivor widened back to cover the 2018 stint")
		require.NotNil(t, survivor.ValidTo, "survivor stays bounded by the pending LA")
		assert.True(t, survivor.ValidTo.Equal(*boundedNYC.ValidTo), "valid_to NOT extended past the pending-successor bound")
		require.NotNil(t, survivor.SupersededBy, "survivor inherits the pending-successor linkage")
		assert.Equal(t, winnerLA.ID, *survivor.SupersededBy, "pending-successor linkage preserved")

		// LA is still a live future row (its date hasn't passed).
		la2, err := h.assertionRepo.GetAssertion(ctx, winnerLA.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, la2.Status, "pending LA stays accepted")
	})

	// Case 12: an ACCEPTED loser collides with a PROPOSED winner proposition → the
	// merge must keep the ACCEPTED row (not demote the merged node's current value to
	// proposed by picking the proposed collider as survivor).
	t.Run("12 accepted loser wins over proposed collider", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")

		// Winner: an ACCEPTED home_address("Y") AND a PROPOSED home_address("X")
		// (ForceConfirm routes X to proposed; proposed does not supersede accepted, so
		// both coexist on the single-card slot). The merge must end with exactly ONE
		// current accepted value, NOT both X and Y accepted.
		winAcceptedY, err := h.svc.Assert(ctx, textFactReq(winner, "home_address", gen.Prefix()+"Y", gen.Prefix(), "wy"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, winAcceptedY.ID)
		require.Equal(t, repository.AssertionStatusAccepted, winAcceptedY.Status)
		winReq := textFactReq(winner, "home_address", gen.Prefix()+"X", gen.Prefix(), "wx")
		winReq.ForceConfirm = true
		winProposed, err := h.svc.Assert(ctx, winReq)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, winProposed.ID)
		require.Equal(t, repository.AssertionStatusProposed, winProposed.Status)

		// Loser: an ACCEPTED home_address("X") (same value + year bucket as winner's
		// proposed X → same proposition key).
		loserAccepted, err := h.svc.Assert(ctx, textFactReq(loser, "home_address", gen.Prefix()+"X", gen.Prefix(), "l"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, loserAccepted.ID)
		require.Equal(t, repository.AssertionStatusAccepted, loserAccepted.Status)

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		// The accepted loser X survives (re-pointed onto the winner); the proposed X
		// collider is closed; AND the winner's other accepted value Y is superseded —
		// so exactly ONE current accepted value remains.
		survivor, err := h.assertionRepo.GetAssertion(ctx, loserAccepted.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusAccepted, survivor.Status, "accepted loser stays accepted")
		assert.Equal(t, winner, survivor.SubjectNodeID, "accepted survivor re-pointed onto the winner")
		collider, err := h.assertionRepo.GetAssertion(ctx, winProposed.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, collider.Status, "proposed collider closed")
		priorY, err := h.assertionRepo.GetAssertion(ctx, winAcceptedY.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, priorY.Status, "the winner's other accepted Y is superseded")
		cur, err := h.assertionRepo.GetCurrentAccepted(ctx, winner, "home_address", nowMicro())
		require.NoError(t, err)
		assert.Equal(t, loserAccepted.ID, cur.ID, "exactly one current accepted value: the X survivor")
	})

	// Case 13: a PAST-bounded self-loop (knows(loser,winner) [2018,2019)) collapses
	// on merge → it is closed but its historical valid_to is NOT stretched to now.
	t.Run("13 past-bounded self-loop keeps its historical valid_to", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		loser := h.seedPerson(t, ctx, gen.Prefix(), "loser")
		winner := h.seedPerson(t, ctx, gen.Prefix(), "winner")

		from := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
		req := edgeReq(loser, winner, "knows", gen.Prefix(), "hist-knows")
		req.ValidFrom = &from
		req.ValidTo = &to
		edge, err := h.svc.Assert(ctx, req)
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, edge.ID)

		mergeAssertionsInTx(t, ctx, h, loser, winner)

		closed, err := h.assertionRepo.GetAssertion(ctx, edge.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status, "self-loop closed")
		require.NotNil(t, closed.ValidTo)
		assert.True(t, closed.ValidTo.Equal(to), "historical valid_to preserved (NOT stretched to now)")
	})

	// Case 10: a CHAINED merge (A→B then B→C) moves the same assertion row twice.
	// The merge-move event is keyed by the WINNER, so each move emits a distinct
	// event (not deduped away) → derived consumers recompute after each re-point.
	t.Run("10 chained merge emits a move event per winner", func(t *testing.T) {
		t.Parallel()
		h, _ := newAssertHarness(t, ctx0())
		gen, _ := migrationGenerator(t)

		a := h.seedPerson(t, ctx, gen.Prefix(), "A")
		b := h.seedPerson(t, ctx, gen.Prefix(), "B")
		c := h.seedPerson(t, ctx, gen.Prefix(), "C")

		// A fact on A, so the subject moves A→B→C (a fact has no object to canonicalize).
		fact, err := h.svc.Assert(ctx, textFactReq(a, "home_address", gen.Prefix()+"addr", gen.Prefix(), "addr"))
		require.NoError(t, err)
		h.cleanupAssertionEvents(t, ctx, fact.ID)

		mergeAssertionsInTx(t, ctx, h, a, b) // A→B
		mergeAssertionsInTx(t, ctx, h, b, c) // B→C

		// Both winner-keyed move events exist (distinct source_id per destination).
		assert.True(t, h.eventExists(t, ctx, fact.ID.String()+":merged:"+b.String()), "move event for A→B")
		assert.True(t, h.eventExists(t, ctx, fact.ID.String()+":merged:"+c.String()), "move event for B→C")

		// The fact is now live on C.
		moved, err := h.assertionRepo.GetAssertion(ctx, fact.ID)
		require.NoError(t, err)
		assert.Equal(t, c, moved.SubjectNodeID, "fact subject moved through to the final winner")
		assert.Equal(t, repository.AssertionStatusAccepted, moved.Status)
	})
}
