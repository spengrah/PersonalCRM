//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backfillPersonNodesVersion is the golang-migrate version of the person-node
// backfill migration (068). The down/up round-trip positions the clone here
// before Steps(-1), so it stays robust to later migrations (069+) being added
// above it (mirrors the assertion-store migration test's positioning).
const backfillPersonNodesVersion = 68

// Contact→node dual-write: ContactService.CreateContact writes a
// node(type='person') at the contact's own id (node.id == contact.id) in the
// same tx, and UpdateContact syncs node.canonical_label on rename. Each
// sub-test is namespace-scoped (migrationGenerator) and cleans up its own
// node by label prefix (the person node's canonical_label == full_name, which
// is namespace-prefixed) so the shared test DB stays isolated under
// t.Parallel().

func TestContactNodeDualWrite_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	t.Run("create contact writes a person node at the same id", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contact, cleanup := seedMigrationContact(ctx, t, database, gen)
		t.Cleanup(cleanup)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err, "create must dual-write a person node at the contact's id")
		assert.Equal(t, contact.ID, node.ID, "node.id == contact.id invariant")
		assert.Equal(t, repository.NodeTypePerson, node.Type)
		assert.Equal(t, contact.FullName, node.CanonicalLabel, "node label seeded from full_name")
		assert.Nil(t, node.DeletedAt)
	})

	t.Run("rename contact updates the node canonical_label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		// UpdateContact requires a wired cadence updater; build a service that
		// owns the same namespace's node cleanup via the prefix above.
		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
		wireCadenceUpdaterForTest(t, database, svc)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
			Cadence:  spec.Cadence,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		// Rename via the profile-update path: the node label must follow.
		renamed := gen.Prefix() + "renamed-person"
		_, _, err = svc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: renamed,
		}, nil, false)
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, renamed, node.CanonicalLabel, "rename syncs node canonical_label")
	})

	t.Run("update without a rename leaves the node label untouched", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
		wireCadenceUpdaterForTest(t, database, svc)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
			Cadence:  spec.Cadence,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		// Same name, a cadence-only edit: the label must not change.
		monthly := "monthly"
		_, _, err = svc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: contact.FullName,
			Cadence:  &monthly,
		}, nil, false)
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, contact.FullName, node.CanonicalLabel)
	})

	t.Run("enrichment rename syncs the node label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
		externalRepo := repository.NewExternalContactRepository(database.Queries)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
		// nil bus/registry → enrichment skips publish.
		enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		display := gen.Prefix() + "ext-display"
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "google",
			SourceID:    gen.Prefix() + "ext-src",
			DisplayName: &display,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = externalRepo.Delete(ctx, external.ID) })

		// Enrichment-driven rename via the no-cadence pool path: the node label
		// must follow (mirrors ContactService.UpdateContact's sync).
		renamed := gen.Prefix() + "enriched-renamed"
		_, err = enrichSvc.EnrichContactFromExternalWithSelections(ctx, contact.ID, external, nil, nil, nil, &renamed)
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, renamed, node.CanonicalLabel, "enrichment rename syncs the node label")
	})

	t.Run("merge with a new name syncs the target node label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
		wireCadenceUpdaterForTest(t, database, svc)

		monthly := "monthly"
		target, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: gen.Prefix() + "merge-target", Cadence: &monthly,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) })
		source, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: gen.Prefix() + "merge-source", Cadence: &monthly,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, source.ID) })

		newName := gen.Prefix() + "merged-renamed"
		_, err = svc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: source.ID,
			TargetContactID: target.ID,
			NewName:         &newName,
		})
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, newName, node.CanonicalLabel, "merge new_name syncs the target node label")
	})

	t.Run("tx rollback leaves no node and no contact", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)

		// Force a failure AFTER the contact + node inserts but inside the same
		// tx: an invalid contact_method type violates the contact_method CHECK
		// constraint, aborting the tx. The dual-write must roll back with it.
		spec := gen.Contact()
		_, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
			Cadence:  spec.Cadence,
		}, []service.ContactMethodInput{
			{Type: "not_a_real_method_type", Value: "x"},
		})
		require.Error(t, err, "invalid method type must abort the create tx")

		// Neither the contact nor its person node may have survived the rollback.
		count, err := support.CountNodesByLabelPrefix(ctx, gen.Prefix())
		require.NoError(t, err)
		assert.Equal(t, int64(0), count, "rolled-back tx leaves no person node")

		contactCount, err := support.CountContactsByFullName(ctx, spec.FullName)
		require.NoError(t, err)
		assert.Equal(t, int64(0), contactCount, "rolled-back tx leaves no contact")
	})
}

// TestContactNodeDualWrite_MigrationDownUp exercises the 068 person-node
// backfill down + up round-trip against an isolated clone (it rolls the schema
// down, so it cannot share the package DB). The down is the GUARDED delete: it
// removes only unreferenced person nodes and leaves any node an assertion still
// points at intact.
func TestContactNodeDualWrite_MigrationDownUp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	// Migration-subject test: rolls the schema down, so it stays serial and uses
	// an isolated clone (never the shared package DB).

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)

	// Seed a contact via the dual-write service path so a live person node
	// exists at the contact's id (this is what 068's down must consider). It
	// has no assertions, so the guarded down is free to delete it.
	contactSvc := service.NewContactService(database, contactRepo,
		repository.NewContactMethodRepository(database.Queries),
		repository.NewInteractionRepository(database.Queries),
		repository.NewContactTaskRepository(database.Queries), nil, nil)
	unreferenced, _, err := contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "migration-unreferenced-person",
	}, nil)
	require.NoError(t, err)
	_, err = nodeRepo.GetNode(ctx, unreferenced.ID)
	require.NoError(t, err, "dual-write created the person node")

	// Seed a SECOND person node that an assertion references as its SUBJECT —
	// the guarded down must leave it intact (deleting it would orphan the
	// assertion).
	referencedID := uuid.New()
	_, err = nodeRepo.CreateNode(ctx, referencedID, repository.NodeTypePerson, "migration-referenced-person")
	require.NoError(t, err)
	value := "anchored"
	_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  referencedID,
		PredicateKey:   "home_address",
		ValueText:      &value,
		KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
		Confidence:     80,
		Salience:       45,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "migration-dualwrite-prop-1",
	})
	require.NoError(t, err)

	// Seed a THIRD person node referenced ONLY as an assertion's OBJECT (a
	// person→person edge). The guarded down checks both positions, so this one
	// must also survive (covering the object_node_id arm of the down's guard).
	objectReferencedID := uuid.New()
	_, err = nodeRepo.CreateNode(ctx, objectReferencedID, repository.NodeTypePerson, "migration-object-referenced-person")
	require.NoError(t, err)
	_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  referencedID,
		PredicateKey:   "parent_of",
		ObjectNodeID:   &objectReferencedID,
		KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
		Confidence:     80,
		Salience:       45,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "migration-dualwrite-prop-2",
	})
	require.NoError(t, err)

	// Seed a merge winner/loser pair with NO assertions: the winner has
	// merged_into NULL (so an assertion-only guard would select it for
	// deletion) but is still referenced by the preserved loser via the
	// merged_into self-FK (restrict). Deleting the winner would FK-violate, so
	// the guarded down must skip it. (The loser carries merged_into, so it is
	// never selected.)
	winnerID, loserID := uuid.New(), uuid.New()
	_, err = nodeRepo.CreateNode(ctx, winnerID, repository.NodeTypePerson, "migration-merge-winner")
	require.NoError(t, err)
	_, err = nodeRepo.CreateNode(ctx, loserID, repository.NodeTypePerson, "migration-merge-loser")
	require.NoError(t, err)
	require.NoError(t, nodeRepo.SetNodeMergedInto(ctx, loserID, winnerID))

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Position the clone at the backfill tip (068) FIRST, so Steps(-1) rolls
	// 068 specifically — robust to later migrations (069+) landing above it.
	if err := m.Migrate(backfillPersonNodesVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the backfill tip")
	}

	// Roll down ONE step: the 068 guarded delete. The unreferenced person node
	// is removed; the assertion-referenced one is preserved.
	require.NoError(t, m.Steps(-1), "roll the person-node backfill down one step")

	_, err = nodeRepo.GetNode(ctx, unreferenced.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "guarded down removes the unreferenced person node")

	stillThere, err := nodeRepo.GetNode(ctx, referencedID)
	require.NoError(t, err, "guarded down preserves the assertion-subject person node")
	assert.Equal(t, referencedID, stillThere.ID)

	objStillThere, err := nodeRepo.GetNode(ctx, objectReferencedID)
	require.NoError(t, err, "guarded down preserves the assertion-object person node")
	assert.Equal(t, objectReferencedID, objStillThere.ID)

	// The merge winner (live, no assertions) survives because the preserved
	// loser still references it via merged_into, so the guarded down must not
	// delete it (an assertion-only guard would have FK-violated here).
	winnerStillThere, err := nodeRepo.GetNode(ctx, winnerID)
	require.NoError(t, err, "guarded down preserves a merge winner referenced by a loser")
	assert.Equal(t, winnerID, winnerStillThere.ID)

	// Roll back up: the backfill re-seeds a person node for every NON-deleted
	// contact at its own id. The unreferenced contact still exists (its row was
	// never touched by the down — only its node was), so the up restores its
	// person node.
	require.NoError(t, m.Steps(1), "re-apply the person-node backfill")

	restored, err := nodeRepo.GetNode(ctx, unreferenced.ID)
	require.NoError(t, err, "the up backfill restores the person node for the surviving contact")
	assert.Equal(t, repository.NodeTypePerson, restored.Type)
	assert.Equal(t, unreferenced.FullName, restored.CanonicalLabel)
}
