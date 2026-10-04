//go:build integration_testdb

package tests

import (
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		cadenceUpdater := buildCadenceUpdaterForTest(t, database)
		assertSvc, cache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, nil)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
			Cadence:  spec.Cadence,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		// Rename via the profile-update path: the node label must follow.
		renamed := gen.Prefix() + "renamed-person"
		_, err = svc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: renamed,
		})
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, renamed, node.CanonicalLabel, "rename syncs node canonical_label")
	})

	t.Run("rename alongside a cadence edit syncs the node label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		cadenceUpdater := buildCadenceUpdaterForTest(t, database)
		assertSvc, cache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, nil)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
			Cadence:  spec.Cadence,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		// A rename combined with a cadence change (the cadence-recompute branch
		// of UpdateContact): the node label must follow the new name — this
		// fails if the in-tx sync regresses, unlike a same-name no-op.
		renamed := gen.Prefix() + "renamed-with-cadence"
		monthly := "monthly"
		_, err = svc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
			FullName: renamed,
			Cadence:  &monthly,
		})
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, renamed, node.CanonicalLabel, "rename+cadence edit syncs the node label")
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
		knowledgeAssertSvc, knowledgeCache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
			nil, knowledgeAssertSvc, knowledgeCache, nil)
		// nil bus/registry → enrichment skips publish.
		enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil,
			nil, knowledgeAssertSvc, knowledgeCache)

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

	t.Run("enrichment rename with cadence syncs the node label in-tx", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
		externalRepo := repository.NewExternalContactRepository(database.Queries)
		// The cadence-present branch routes through CadenceUpdater and writes
		// the node label inside the same tx — pass cadence to both services.
		cadenceUpdater := buildCadenceUpdaterForTest(t, database)
		assertSvc, knowledgeCache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
			cadenceUpdater, assertSvc, knowledgeCache, nil)
		enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil,
			cadenceUpdater, assertSvc, knowledgeCache)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		display := gen.Prefix() + "ext-display-cad"
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "google",
			SourceID:    gen.Prefix() + "ext-src-cad",
			DisplayName: &display,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = externalRepo.Delete(ctx, external.ID) })

		// name + cadence → the cadence-tx branch; the node label sync rides the
		// same tx as the contact update.
		renamed := gen.Prefix() + "enriched-cad-renamed"
		monthly := "monthly"
		_, err = enrichSvc.EnrichContactFromExternalWithSelections(ctx, contact.ID, external, nil, nil, &monthly, &renamed)
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, renamed, node.CanonicalLabel, "cadence-path enrichment rename syncs the node label")
	})

	t.Run("no-name enrichment keeps the node label matching the contact", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
		externalRepo := repository.NewExternalContactRepository(database.Queries)
		knowledgeAssertSvc, knowledgeCache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
			nil, knowledgeAssertSvc, knowledgeCache, nil)
		enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil,
			nil, knowledgeAssertSvc, knowledgeCache)

		spec := gen.Contact()
		contact, _, err := svc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: spec.FullName,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		// External carries a birthday the contact lacks → the legacy
		// EnrichContactFromExternal path performs a (non-rename) UpdateContact,
		// which now also unconditionally syncs the node label in-tx. The label
		// must still equal the (unchanged) contact name afterward.
		bday := accelerated.GetCurrentTime().UTC()
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "google",
			SourceID: gen.Prefix() + "ext-src-noname",
			Birthday: &bday,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = externalRepo.Delete(ctx, external.ID) })

		_, err = enrichSvc.EnrichContactFromExternal(ctx, contact.ID, external)
		require.NoError(t, err)

		node, err := support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Equal(t, contact.FullName, node.CanonicalLabel, "no-name enrichment leaves the node label in sync")
	})

	t.Run("merge with a new name syncs the target node label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		contactRepo := repository.NewContactRepository(database.Queries)
		methodRepo := repository.NewContactMethodRepository(database.Queries)
		interactionRepo := repository.NewInteractionRepository(database.Queries)
		taskRepo := repository.NewContactTaskRepository(database.Queries)
		cadenceUpdater := buildCadenceUpdaterForTest(t, database)
		assertSvc, cache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, nil)

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
		assertSvc, cache := buildKnowledgeDeps(t, database, nil)
		svc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil, nil, assertSvc, cache, nil)

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

// TestContactNodeDualWrite_HarnessSeedCreatesNode confirms the synthetic
// harness's SeedContact — which drives the real ContactService.CreateContact —
// implicitly dual-writes a person node, and that the harness teardown removes
// it (the cleanup step added alongside the dual-write). SLOW-gated because the
// harness spins up a River client.
func TestContactNodeDualWrite_HarnessSeedCreatesNode(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	support := repository.NewSyntheticSupportRepository(h.Database().Queries)

	spec := h.Generator().Contact()
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	node, err := support.GetNodeForContact(ctx, contact.ID)
	require.NoError(t, err, "SeedContact must dual-write a person node")
	assert.Equal(t, contact.ID, node.ID)
	assert.Equal(t, repository.NodeTypePerson, node.Type)
	assert.Equal(t, contact.FullName, node.CanonicalLabel)
}
