//go:build integration_testdb

package tests

import (
	"context"
	"encoding/json"
	"strings"
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

// tagMigrationHarness bundles the migration service under test + the repos a
// test reads back. The legacy tag/contact_tag tables are not namespace-scoped
// columns, so the test tracks every seeded tag/contact_tag id and cleans them
// up explicitly (the migrate command itself is a global one-shot).
type tagMigrationHarness struct {
	svc           *service.TagMigrationService
	contactRepo   *repository.ContactRepository
	nodeRepo      *repository.NodeRepository
	entityRepo    *repository.EntityRepository
	assertionRepo *repository.AssertionRepository
	eventRepo     *repository.EventRepository
	support       *repository.SyntheticSupportRepository

	// tracked ids for teardown.
	contactIDs    []uuid.UUID
	tagIDs        []uuid.UUID
	tagNormalized []string // lower(name) of every seeded tag, to resolve its entity node for cleanup
}

func newTagMigrationHarness(t *testing.T, ctx context.Context) (*tagMigrationHarness, context.Context) {
	t.Helper()
	database, cfg := newSharedTestDB(t, ctx)

	eventRepo := repository.NewEventRepository(database.Queries)
	workers := river.NewWorkers()
	river.AddWorker(workers, &assertNoopWorker{})
	// assertion.accepted/superseded now route a knowledge_cache_updater job; the
	// no-op worker registers the kind so the insert-only client accepts the enqueue.
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
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
	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	tagRepo := repository.NewTagRepository(database.Queries)
	svc := service.NewTagMigrationService(database.Pool, tagRepo, nodeRepo, entityRepo, assertSvc)

	return &tagMigrationHarness{
		svc:           svc,
		contactRepo:   repository.NewContactRepository(database.Queries),
		nodeRepo:      nodeRepo,
		entityRepo:    entityRepo,
		assertionRepo: assertionRepo,
		eventRepo:     eventRepo,
		support:       repository.NewSyntheticSupportRepository(database.Queries),
	}, ctx
}

// seedContactWithNode creates a contact — ContactRepository.CreateContact
// creates its person node in the same statement (node.id == contact.id,
// contact_id_node_fk), mirroring the production person-node backfill that
// guarantees every non-deleted contact has a node. Returns the contact id.
func (h *tagMigrationHarness) seedContactWithNode(t *testing.T, ctx context.Context, fullName string) uuid.UUID {
	t.Helper()
	contact, err := h.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
	require.NoError(t, err)
	h.contactIDs = append(h.contactIDs, contact.ID)
	return contact.ID
}

// softDeleteContact soft-deletes a contact AND its person node, mirroring the
// production soft-delete + node tombstone, so the migration's
// non-deleted-contact filter (and the assert path's tombstone rejection) apply.
func (h *tagMigrationHarness) softDeleteContact(t *testing.T, ctx context.Context, contactID uuid.UUID) {
	t.Helper()
	require.NoError(t, h.contactRepo.SoftDeleteContact(ctx, contactID))
	require.NoError(t, h.nodeRepo.SoftDeleteNode(ctx, contactID))
}

// seedTag creates a legacy tag row with the given name + optional color, tracks
// its id + normalized name for teardown, and returns the id.
func (h *tagMigrationHarness) seedTag(t *testing.T, ctx context.Context, name string, color *string) uuid.UUID {
	t.Helper()
	id, err := h.support.InsertTagForMigration(ctx, name, color)
	require.NoError(t, err)
	h.tagIDs = append(h.tagIDs, id)
	h.tagNormalized = append(h.tagNormalized, strings.ToLower(strings.TrimSpace(name)))
	return id
}

// seedContactTag links a contact to a tag at an explicit created_at (the
// knowledge time the migration should preserve).
func (h *tagMigrationHarness) seedContactTag(t *testing.T, ctx context.Context, contactID, tagID uuid.UUID, createdAt time.Time) {
	t.Helper()
	require.NoError(t, h.support.InsertContactTagAtTime(ctx, contactID, tagID, createdAt))
}

// seedContactTagNull links a contact to a tag with a NULL created_at (the legacy
// column is nullable), so a test can assert the migration falls back to "now".
func (h *tagMigrationHarness) seedContactTagNull(t *testing.T, ctx context.Context, contactID, tagID uuid.UUID) {
	t.Helper()
	require.NoError(t, h.support.InsertContactTagNullCreatedAt(ctx, contactID, tagID))
}

// registerCleanup tears down everything the test created, FK-ordered. Registered
// once per test; t.Cleanup runs LIFO so call it right after the harness is built
// (before seeding), and it executes after every subtest finishes.
//
// Order inside the closure: delete each contact node's assertions (+ provenance
// cascade) and their event rows first (assertion→node is a RESTRICT FK); then
// contact_tags; then contact tasks and the contact rows themselves — both must
// clear before the person nodes, since contact.id references node.id via
// contact_id_node_fk (NO ACTION, so a node delete while its contact still
// exists is rejected); then the person nodes and the tag entity nodes; finally
// the legacy tags. A failed step here is logged (not silently discarded), since
// a swallowed error here is exactly what leaks a node into the shared test
// database across runs.
func (h *tagMigrationHarness) registerCleanup(t *testing.T, ctx context.Context) {
	t.Cleanup(func() {
		logIfErr := func(step string, err error) {
			if err != nil {
				t.Logf("tagMigrationHarness cleanup: %s: %v", step, err)
			}
		}

		for _, cid := range h.contactIDs {
			assertions, _ := h.assertionRepo.ListAssertionsBySubject(ctx, cid)
			for _, a := range assertions {
				logIfErr("delete assertion events", h.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, "assertion", a.ID.String()))
			}
			_, err := h.support.DeleteAssertionsForNode(ctx, cid)
			logIfErr("delete assertions for node", err)
		}
		_, err := h.support.DeleteContactTagsByContactIds(ctx, h.contactIDs)
		logIfErr("delete contact_tags", err)

		// contact_task and contact must clear before the person nodes: contact.id
		// references node.id via contact_id_node_fk (NO ACTION), so deleting the
		// node first is rejected by the FK.
		_, err = h.support.DeleteContactTasksByContactIds(ctx, h.contactIDs)
		logIfErr("delete contact tasks", err)
		_, err = h.support.DeleteContactsByIds(ctx, h.contactIDs)
		logIfErr("delete contacts", err)

		// Resolve the tag entity node ids the migration created (node.id != tag.id)
		// by their normalized name, then hard-delete those nodes (entity cascades).
		var tagNodeIDs []uuid.UUID
		for _, norm := range h.tagNormalized {
			entity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, norm)
			if err == nil {
				tagNodeIDs = append(tagNodeIDs, entity.NodeID)
			}
		}
		_, err = h.support.DeleteNodesByIds(ctx, tagNodeIDs)
		logIfErr("delete tag entity nodes", err)

		_, err = h.support.DeleteNodesByIds(ctx, h.contactIDs)
		logIfErr("delete person nodes", err)
		_, err = h.support.DeleteTagsByIds(ctx, h.tagIDs)
		logIfErr("delete legacy tags", err)
	})
}

// findTaggedAs returns the single LIVE accepted tagged_as assertion linking
// subject → objectNode, or fails if it is absent / not unique.
func (h *tagMigrationHarness) findTaggedAs(t *testing.T, ctx context.Context, subject, objectNode uuid.UUID) repository.Assertion {
	t.Helper()
	assertions, err := h.assertionRepo.ListAssertionsBySubject(ctx, subject)
	require.NoError(t, err)
	var found []repository.Assertion
	for _, a := range assertions {
		if a.PredicateKey == "tagged_as" && a.Status == repository.AssertionStatusAccepted &&
			a.KnowledgeTo == nil && a.ObjectNodeID != nil && *a.ObjectNodeID == objectNode {
			found = append(found, a)
		}
	}
	require.Len(t, found, 1, "expected exactly one live accepted tagged_as for subject %s → %s", subject, objectNode)
	return found[0]
}

// TestTagMigration_Integration covers the --migrate-tags command end to end. It
// runs SERIAL (no t.Parallel): the migrate command reads the GLOBAL tag table
// (not namespace-scoped), so concurrent tag-touching tests would perturb its
// input. Each subtest builds a fresh harness, seeds only its own tags, and cleans
// them up, so the global tag set during a subtest is just that subtest's tags.
func TestTagMigration_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()

	t.Run("migrates tags + contact_tags with user provenance and color", func(t *testing.T) {
		h, ctx := newTagMigrationHarness(t, ctx)
		h.registerCleanup(t, ctx)
		ns := syntheticNS(t)

		contactID := h.seedContactWithNode(t, ctx, ns+" Alice")
		color := "#ff8800"
		coloredTagID := h.seedTag(t, ctx, ns+"-vip", &color)
		plainTagID := h.seedTag(t, ctx, ns+"-lead", nil)

		// Distinct knowledge times so we can assert KnowledgeFromOverride preserves
		// each contact_tag.created_at.
		coloredAt := accelerated.GetCurrentTime().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
		plainAt := accelerated.GetCurrentTime().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
		h.seedContactTag(t, ctx, contactID, coloredTagID, coloredAt)
		h.seedContactTag(t, ctx, contactID, plainTagID, plainAt)

		res, err := h.svc.MigrateTags(ctx)
		require.NoError(t, err)
		// Global counts include any stray tag (e.g. a reset marker), so assert >=
		// our seeded volume rather than exact equality on the global figures.
		assert.GreaterOrEqual(t, res.Tags, 2)
		assert.GreaterOrEqual(t, res.ContactTags, 2)

		// Both tag entity nodes exist with subtype='tag'; the colored one carries
		// its color in detail, the plain one has no color key.
		coloredEntity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, ns+"-vip")
		require.NoError(t, err)
		assert.Equal(t, color, detailColor(t, coloredEntity.Detail))

		plainEntity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, ns+"-lead")
		require.NoError(t, err)
		assert.Empty(t, detailColor(t, plainEntity.Detail), "plain tag must carry no color")

		// Each contact_tag became a live accepted tagged_as assertion with user
		// provenance and the contact_tag's created_at as knowledge time.
		coloredAssertion := h.findTaggedAs(t, ctx, contactID, coloredEntity.NodeID)
		assert.Equal(t, coloredAt, coloredAssertion.KnowledgeFrom.UTC().Truncate(time.Microsecond))
		assertUserProvenance(t, ctx, h, coloredAssertion.ID)

		plainAssertion := h.findTaggedAs(t, ctx, contactID, plainEntity.NodeID)
		assert.Equal(t, plainAt, plainAssertion.KnowledgeFrom.UTC().Truncate(time.Microsecond))
		assertUserProvenance(t, ctx, h, plainAssertion.ID)

		count, err := h.support.CountTaggedAsAssertionsForSubject(ctx, contactID)
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)
	})

	t.Run("skips a soft-deleted contact's tags", func(t *testing.T) {
		h, ctx := newTagMigrationHarness(t, ctx)
		h.registerCleanup(t, ctx)
		ns := syntheticNS(t)

		liveID := h.seedContactWithNode(t, ctx, ns+" Live")
		deletedID := h.seedContactWithNode(t, ctx, ns+" Gone")
		tagID := h.seedTag(t, ctx, ns+"-shared", nil)
		at := accelerated.GetCurrentTime().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		h.seedContactTag(t, ctx, liveID, tagID, at)
		h.seedContactTag(t, ctx, deletedID, tagID, at)

		h.softDeleteContact(t, ctx, deletedID)

		res, err := h.svc.MigrateTags(ctx)
		require.NoError(t, err)
		// The skip is surfaced in the summary (global count, so >= our one row).
		assert.GreaterOrEqual(t, res.SkippedDeletedContacts, 1, "skipped-deleted count must include the deleted contact's tag")

		// The live contact got its tagged_as; the deleted contact got none.
		liveCount, err := h.support.CountTaggedAsAssertionsForSubject(ctx, liveID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, liveCount, "live contact's tag should migrate")

		deletedCount, err := h.support.CountTaggedAsAssertionsForSubject(ctx, deletedID)
		require.NoError(t, err)
		assert.EqualValues(t, 0, deletedCount, "soft-deleted contact's tag must be skipped")
	})

	t.Run("fails loudly on a case-insensitive name collision", func(t *testing.T) {
		h, ctx := newTagMigrationHarness(t, ctx)
		h.registerCleanup(t, ctx)
		ns := syntheticNS(t)

		contactID := h.seedContactWithNode(t, ctx, ns+" Carol")
		upperID := h.seedTag(t, ctx, ns+"-Friend", nil)
		lowerID := h.seedTag(t, ctx, ns+"-friend", nil)
		at := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
		h.seedContactTag(t, ctx, contactID, upperID, at)
		h.seedContactTag(t, ctx, contactID, lowerID, at)

		_, err := h.svc.MigrateTags(ctx)
		require.Error(t, err)
		require.ErrorIs(t, err, service.ErrTagCaseCollision)
		// The error names BOTH colliding originals so the operator can dedup.
		assert.Contains(t, err.Error(), ns+"-Friend")
		assert.Contains(t, err.Error(), ns+"-friend")

		// Nothing was written: no tagged_as assertion for the contact, and the
		// colliding tag entity node was not created (preflight runs before any
		// node creation).
		count, err := h.support.CountTaggedAsAssertionsForSubject(ctx, contactID)
		require.NoError(t, err)
		assert.EqualValues(t, 0, count, "a collision must create no assertions")

		_, findErr := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, strings.ToLower(ns+"-friend"))
		require.ErrorIs(t, findErr, db.ErrNotFound, "a collision must create no tag entity node")
	})

	t.Run("is idempotent across re-runs", func(t *testing.T) {
		h, ctx := newTagMigrationHarness(t, ctx)
		h.registerCleanup(t, ctx)
		ns := syntheticNS(t)

		contactID := h.seedContactWithNode(t, ctx, ns+" Dave")
		tagID := h.seedTag(t, ctx, ns+"-repeat", nil)
		at := accelerated.GetCurrentTime().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		h.seedContactTag(t, ctx, contactID, tagID, at)

		_, err := h.svc.MigrateTags(ctx)
		require.NoError(t, err)

		entity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, ns+"-repeat")
		require.NoError(t, err)
		firstAssertion := h.findTaggedAs(t, ctx, contactID, entity.NodeID)
		firstEventIDs, err := h.support.ListEventIdsBySourceAndSourceIDPrefix(ctx, "assertion", firstAssertion.ID.String())
		require.NoError(t, err)

		// Re-run: same proposition identity → corroborate, not duplicate.
		_, err = h.svc.MigrateTags(ctx)
		require.NoError(t, err)

		// Still exactly one tag entity node (find-or-create reused it) and one
		// live accepted tagged_as assertion (same id), with no new event rows.
		entity2, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, ns+"-repeat")
		require.NoError(t, err)
		assert.Equal(t, entity.NodeID, entity2.NodeID, "re-run must reuse the tag entity node")

		secondAssertion := h.findTaggedAs(t, ctx, contactID, entity.NodeID)
		assert.Equal(t, firstAssertion.ID, secondAssertion.ID, "re-run must not create a new assertion")

		count, err := h.support.CountTaggedAsAssertionsForSubject(ctx, contactID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, count, "re-run must not duplicate the assertion")

		secondEventIDs, err := h.support.ListEventIdsBySourceAndSourceIDPrefix(ctx, "assertion", firstAssertion.ID.String())
		require.NoError(t, err)
		assert.Equal(t, len(firstEventIDs), len(secondEventIDs), "re-run must emit no new assertion events")
	})

	t.Run("falls back to now when contact_tag.created_at is NULL", func(t *testing.T) {
		h, ctx := newTagMigrationHarness(t, ctx)
		h.registerCleanup(t, ctx)
		ns := syntheticNS(t)

		contactID := h.seedContactWithNode(t, ctx, ns+" Erin")
		tagID := h.seedTag(t, ctx, ns+"-nulltime", nil)
		h.seedContactTagNull(t, ctx, contactID, tagID)

		before := accelerated.GetCurrentTime().UTC()
		_, err := h.svc.MigrateTags(ctx)
		require.NoError(t, err)
		after := accelerated.GetCurrentTime().UTC()

		entity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, ns+"-nulltime")
		require.NoError(t, err)
		a := h.findTaggedAs(t, ctx, contactID, entity.NodeID)
		// A NULL legacy created_at must NOT become a zero/ancient knowledge time:
		// it falls back to "now" (knowledge_from is within the run window).
		kf := a.KnowledgeFrom.UTC()
		assert.Falsef(t, kf.Before(before.Add(-time.Minute)), "knowledge_from %s must not predate the run (no zero-time)", kf)
		assert.Falsef(t, kf.After(after.Add(time.Minute)), "knowledge_from %s must not postdate the run", kf)
	})
}

// detailColor extracts the color string from an entity detail JSONB (empty when
// absent).
func detailColor(t *testing.T, detail []byte) string {
	t.Helper()
	if len(detail) == 0 {
		return ""
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(detail, &m))
	if c, ok := m["color"].(string); ok {
		return c
	}
	return ""
}

// assertUserProvenance asserts the assertion has exactly one provenance row, with
// user source_kind + user producer_kind (the migration's authorship).
func assertUserProvenance(t *testing.T, ctx context.Context, h *tagMigrationHarness, assertionID uuid.UUID) {
	t.Helper()
	provs, err := h.assertionRepo.ListProvenance(ctx, assertionID)
	require.NoError(t, err)
	require.Len(t, provs, 1)
	assert.Equal(t, repository.SourceKindUser, provs[0].SourceKind)
	assert.Equal(t, repository.ProducerKindUser, provs[0].ProducerKind)
}
