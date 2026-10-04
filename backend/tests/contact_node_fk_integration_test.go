//go:build integration_testdb

package tests

import (
	"errors"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contact_id_node_fk (migration 077) makes "every contact has a node" a
// database constraint, not just a convention ContactService happens to honor.
// These tests exercise the constraint itself, the repository change that makes
// it satisfiable at pool scope (a single data-modifying-CTE insert creates the
// contact and its node atomically, with no surrounding transaction required),
// and the migration that installs the constraint (type-collision preflight,
// backfill, then the FK).

// TestContactNodeFK_RejectsContactWithoutNode proves the constraint itself
// rejects an orphan contact row. Without this, contact_id_node_fk would be
// asserted only by its own existence, which proves nothing.
func TestContactNodeFK_RejectsContactWithoutNode(t *testing.T) {
	// spec: CON-003.fk-enforced
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	gen, _ := migrationGenerator(t)
	orphanID := uuid.New()
	err := support.InsertContactAtID(ctx, orphanID, gen.Prefix()+"orphan-contact")

	require.Error(t, err, "a contact row at an id with no node must be rejected")
	var pgErr *pgconn.PgError
	require.Truef(t, errors.As(err, &pgErr), "expected a *pgconn.PgError, got %v", err)
	assert.Equal(t, pgerrcode.ForeignKeyViolation, pgErr.Code)
	assert.Equal(t, "contact_id_node_fk", pgErr.ConstraintName)
}

// TestContactRepositoryCreate_CreatesPersonNode proves ContactRepository.
// CreateContact itself creates the person node, called at pool level outside
// any transaction — the shape ~150 existing integration-test call sites use.
func TestContactRepositoryCreate_CreatesPersonNode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	gen, _ := migrationGenerator(t)
	fullName := gen.Prefix() + "pool-level-create"

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

	node, err := support.GetNodeForContact(ctx, contact.ID)
	require.NoError(t, err, "CreateContact at pool level must create a matching person node")
	assert.Equal(t, contact.ID, node.ID, "node.id == contact.id invariant")
	assert.Equal(t, repository.NodeTypePerson, node.Type)
	assert.Equal(t, fullName, node.CanonicalLabel)
}

// TestContactRepositoryCreate_IsAtomic proves the contact+node pair commits or
// rolls back together at pool scope, with no surrounding transaction. The
// success subtest is what makes this red on today's code — without it, the
// failure subtest passes vacuously (a create that never makes a node cannot
// orphan one).
func TestContactRepositoryCreate_IsAtomic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	t.Run("success leaves exactly one node", func(t *testing.T) {
		t.Parallel()
		fullName := gen.Prefix() + "atomic-success"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

		count, err := support.CountNodesByLabelPrefix(ctx, fullName)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count, "a successful create must leave exactly one node row")
	})

	t.Run("failed insert leaves no node and no contact", func(t *testing.T) {
		t.Parallel()
		fullName := gen.Prefix() + "atomic-failure"
		invalidCadence := "not_a_real_cadence"
		_, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: fullName,
			Cadence:  &invalidCadence,
		})
		require.Error(t, err, "an invalid cadence must violate contact_cadence_check")

		nodeCount, err := support.CountNodesByLabelPrefix(ctx, fullName)
		require.NoError(t, err)
		assert.Equal(t, int64(0), nodeCount, "a failed create must leave no orphan node")

		contactCount, err := support.CountContactsByFullName(ctx, fullName)
		require.NoError(t, err)
		assert.Equal(t, int64(0), contactCount, "a failed create must leave no contact row")
	})
}

// TestHardDeleteContact_LeavesNoOrphanNode proves the HardDeleteContact
// wrapper cleans up the person node it now creates — otherwise every one of
// the ~170 test-cleanup call sites through HardDeleteContact leaks a node into
// the shared test database across runs.
func TestHardDeleteContact_LeavesNoOrphanNode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	t.Run("no orphan node after hard delete", func(t *testing.T) {
		t.Parallel()
		fullName := gen.Prefix() + "harddelete-no-orphan"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
		require.NoError(t, err)
		_, err = support.GetNodeForContact(ctx, contact.ID)
		require.NoError(t, err, "the create must have made a node to delete")

		require.NoError(t, contactRepo.HardDeleteContact(ctx, contact.ID))

		_, err = nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "hard-deleting the contact must also remove its person node")
	})

	t.Run("node pinned by an assertion survives hard delete", func(t *testing.T) {
		t.Parallel()
		fullName := gen.Prefix() + "harddelete-pinned"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
		require.NoError(t, err)

		assertionRepo := repository.NewAssertionRepository(database.Queries)
		value := "pinned"
		_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
			SubjectNodeID:  contact.ID,
			PredicateKey:   "home_address",
			ValueText:      &value,
			KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
			Confidence:     80,
			Salience:       45,
			Status:         repository.AssertionStatusAccepted,
			PropositionKey: gen.Prefix() + "harddelete-pinned-prop",
		})
		require.NoError(t, err)
		// One closure, not two t.Cleanup registrations: t.Cleanup runs LIFO, and
		// the node delete must run AFTER the assertion delete (assertion.
		// subject_node_id is a RESTRICT FK), so two separate registrations in
		// assertion-then-node order would execute node-then-assertion and leak the
		// node when its delete is rejected and swallowed. Each step's error now
		// fails the test (not swallowed), and the trailing GetNodeIncludingDeleted
		// check is the second line of defense so a reintroduced ordering bug goes
		// red here even if some future delete call silently affects zero rows.
		t.Cleanup(func() {
			if _, err := support.DeleteAssertionsForNode(ctx, contact.ID); err != nil {
				t.Errorf("cleanup: delete assertions for node %s: %v", contact.ID, err)
			}
			if _, err := support.DeleteNodesByIds(ctx, []uuid.UUID{contact.ID}); err != nil {
				t.Errorf("cleanup: delete node %s: %v", contact.ID, err)
			}
			if _, err := nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID); !errors.Is(err, db.ErrNotFound) {
				t.Errorf("cleanup: person node %s still exists after cleanup (err=%v)", contact.ID, err)
			}
		})

		require.NoError(t, contactRepo.HardDeleteContact(ctx, contact.ID))

		_, err = contactRepo.GetContact(ctx, contact.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "hard delete must still remove the contact row")
		_, err = nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID)
		assert.NoError(t, err, "a node an assertion still references must survive the hard delete")
	})

	t.Run("node pinned only as an assertion's object survives hard delete", func(t *testing.T) {
		t.Parallel()
		fullName := gen.Prefix() + "harddelete-object-pinned"
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
		require.NoError(t, err)

		// A second node references contact.ID only as an assertion's OBJECT (a
		// person→person edge) — never as its subject. The guard in
		// TestHardDeleteContactWithNode checks both positions
		// (subject_node_id OR object_node_id); the "pinned by an assertion"
		// subtest above only exercises the subject arm, so a regression that
		// dropped the object_node_id half of the guard would pass that subtest
		// while failing here.
		subjectID := uuid.New()
		_, err = nodeRepo.CreateNode(ctx, subjectID, repository.NodeTypePerson, gen.Prefix()+"harddelete-object-pinned-subject")
		require.NoError(t, err)

		assertionRepo := repository.NewAssertionRepository(database.Queries)
		_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
			SubjectNodeID:  subjectID,
			PredicateKey:   "parent_of",
			ObjectNodeID:   &contact.ID,
			KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
			Confidence:     80,
			Salience:       45,
			Status:         repository.AssertionStatusAccepted,
			PropositionKey: gen.Prefix() + "harddelete-object-pinned-prop",
		})
		require.NoError(t, err)
		// One closure, for the same LIFO reason as the subject-pinned subtest
		// above: assertions must clear before either node. Errors fail the test
		// and the trailing existence checks are the second line of defense, same
		// rationale as that subtest.
		t.Cleanup(func() {
			if _, err := support.DeleteAssertionsForNode(ctx, subjectID); err != nil {
				t.Errorf("cleanup: delete assertions for node %s: %v", subjectID, err)
			}
			if _, err := support.DeleteNodesByIds(ctx, []uuid.UUID{subjectID, contact.ID}); err != nil {
				t.Errorf("cleanup: delete nodes %s,%s: %v", subjectID, contact.ID, err)
			}
			if _, err := nodeRepo.GetNodeIncludingDeleted(ctx, subjectID); !errors.Is(err, db.ErrNotFound) {
				t.Errorf("cleanup: subject node %s still exists after cleanup (err=%v)", subjectID, err)
			}
			if _, err := nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID); !errors.Is(err, db.ErrNotFound) {
				t.Errorf("cleanup: person node %s still exists after cleanup (err=%v)", contact.ID, err)
			}
		})

		require.NoError(t, contactRepo.HardDeleteContact(ctx, contact.ID))

		_, err = contactRepo.GetContact(ctx, contact.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "hard delete must still remove the contact row")
		_, err = nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID)
		assert.NoError(t, err, "a node referenced only as an assertion's object must survive the hard delete")
	})
}
