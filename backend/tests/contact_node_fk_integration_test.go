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
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
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
// and the migration that installs the constraint (backfill, type-collision
// preflight, then the FK).

// contactNodeFKPreVersion is the golang-migrate version immediately before the
// contact→node identity FK migration (077). The round-trip positions each
// clone here first so Steps(1) applies 077 specifically, robust to later
// migrations landing above it as the schema evolves.
const contactNodeFKPreVersion = 76

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
		// node when its delete is rejected and swallowed.
		t.Cleanup(func() {
			_, _ = support.DeleteAssertionsForNode(ctx, contact.ID)
			_, _ = support.DeleteNodesByIds(ctx, []uuid.UUID{contact.ID})
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
		// above: assertions must clear before either node.
		t.Cleanup(func() {
			_, _ = support.DeleteAssertionsForNode(ctx, subjectID)
			_, _ = support.DeleteNodesByIds(ctx, []uuid.UUID{subjectID, contact.ID})
		})

		require.NoError(t, contactRepo.HardDeleteContact(ctx, contact.ID))

		_, err = contactRepo.GetContact(ctx, contact.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "hard delete must still remove the contact row")
		_, err = nodeRepo.GetNodeIncludingDeleted(ctx, contact.ID)
		assert.NoError(t, err, "a node referenced only as an assertion's object must survive the hard delete")
	})
}

// contactNodeFKEnv is one migration round-trip case's isolated clone,
// positioned at contactNodeFKPreVersion. Every case gets its own clone: the
// two subtests below seed mutually exclusive fixtures and a refused/aborted
// migration leaves the migrator dirty.
type contactNodeFKEnv struct {
	ctx      context.Context
	database *db.Database
	migrator *migrate.Migrate
}

func newContactNodeFKEnv(t *testing.T) *contactNodeFKEnv {
	t.Helper()
	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	if err := m.Migrate(contactNodeFKPreVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone before the contact-node identity FK migration")
	}

	return &contactNodeFKEnv{ctx: ctx, database: database, migrator: m}
}

// TestContactNodeFK_MigrationUpDown covers migration 077 directly: the
// backfill, the type-collision preflight, and the constraint itself. The two
// subtests seed mutually exclusive fixtures and cannot share a database, so
// each gets its own clone via newContactNodeFKEnv.
//
// Migration-subject test: it rolls the schema up/down, so it stays serial (no
// t.Parallel()) per .ai/rules/testing.md "Migration-subject tests", mirroring
// migration_076_down_test.go.
func TestContactNodeFK_MigrationUpDown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Run("success/up_and_down", func(t *testing.T) {
		env := newContactNodeFKEnv(t)
		support := repository.NewSyntheticSupportRepository(env.database.Queries)
		contactRepo := repository.NewContactRepository(env.database.Queries)
		nodeRepo := repository.NewNodeRepository(env.database.Queries)

		// The pre-068 shape: a live contact and a soft-deleted contact, both
		// with NO node row.
		liveID := uuid.New()
		require.NoError(t, support.InsertContactAtID(env.ctx, liveID, "contact-node-fk-live"))
		deletedID := uuid.New()
		require.NoError(t, support.InsertContactAtID(env.ctx, deletedID, "contact-node-fk-deleted"))
		require.NoError(t, contactRepo.SoftDeleteContact(env.ctx, deletedID))
		deletedAt, err := support.GetContactDeletedAtIncludingDeleted(env.ctx, deletedID)
		require.NoError(t, err)
		require.NotNil(t, deletedAt, "the soft delete above must have set deleted_at")

		require.NoError(t, env.migrator.Steps(1), "077 must backfill both contacts and add the FK")

		liveNode, err := support.GetNodeForContact(env.ctx, liveID)
		require.NoError(t, err, "the backfill must mint a node for the live contact")
		assert.Equal(t, repository.NodeTypePerson, liveNode.Type)
		assert.Equal(t, "contact-node-fk-live", liveNode.CanonicalLabel)
		assert.Nil(t, liveNode.DeletedAt)

		deletedNode, err := nodeRepo.GetNodeIncludingDeleted(env.ctx, deletedID)
		require.NoError(t, err, "the backfill must mint a node for the soft-deleted contact too")
		assert.Equal(t, repository.NodeTypePerson, deletedNode.Type)
		assert.Equal(t, "contact-node-fk-deleted", deletedNode.CanonicalLabel)
		require.NotNil(t, deletedNode.DeletedAt, "the backfilled node must mirror the contact's tombstone")
		assert.WithinDuration(t, *deletedAt, *deletedNode.DeletedAt, 0,
			"node.deleted_at must exactly equal contact.deleted_at")

		assert.True(t, constraintExists(env.ctx, t, env.database, "contact_id_node_fk"),
			"077 must add the FK after the backfill+preflight succeed")

		catalog, err := support.GetContactIdNodeFkCatalog(env.ctx)
		require.NoError(t, err)
		assert.True(t, catalog.Validated,
			"contact_id_node_fk must be VALIDATED, not NOT VALID — a NOT VALID FK enforces new writes but admits pre-existing orphans")
		assert.True(t, catalog.NoAction,
			"contact_id_node_fk must be NO ACTION — a CASCADE or SET NULL delete would silently take the contact with its node")

		require.NoError(t, env.migrator.Steps(-1), "the down must drop only the constraint")

		assert.False(t, constraintExists(env.ctx, t, env.database, "contact_id_node_fk"),
			"the down must remove the constraint")
		_, err = nodeRepo.GetNodeIncludingDeleted(env.ctx, liveID)
		assert.NoError(t, err, "the down must NOT delete the backfilled live node")
		_, err = nodeRepo.GetNodeIncludingDeleted(env.ctx, deletedID)
		assert.NoError(t, err, "the down must NOT delete the backfilled tombstoned node")
	})

	t.Run("collision/aborts", func(t *testing.T) {
		env := newContactNodeFKEnv(t)
		support := repository.NewSyntheticSupportRepository(env.database.Queries)
		nodeRepo := repository.NewNodeRepository(env.database.Queries)

		// Contact X's id collides with an existing entity node — the case the
		// preflight exists to catch. The preflight runs before the backfill, so
		// this collision is detected against the pre-existing node, not one the
		// backfill created.
		collidingID := uuid.New()
		_, err := nodeRepo.CreateNode(env.ctx, collidingID, repository.NodeTypeEntity, "contact-node-fk-collision-entity")
		require.NoError(t, err)
		require.NoError(t, support.InsertContactAtID(env.ctx, collidingID, "contact-node-fk-collision-contact"))

		// Contact Y has no node at all and no collision of its own. Because the
		// preflight runs before the backfill, the abort on X happens before Y's
		// backfill insert ever executes — Y stays nodeless either way, proving the
		// abort leaves no partial state rather than proving anything about
		// BEGIN/COMMIT specifically.
		nodelessID := uuid.New()
		require.NoError(t, support.InsertContactAtID(env.ctx, nodelessID, "contact-node-fk-collision-nodeless"))

		err = env.migrator.Steps(1)
		require.Error(t, err, "the preflight must abort the migration on a type collision")
		assert.Contains(t, err.Error(), "collides with a non-person node")

		assert.False(t, constraintExists(env.ctx, t, env.database, "contact_id_node_fk"),
			"an aborted migration must not leave the FK in place")
		_, err = nodeRepo.GetNodeIncludingDeleted(env.ctx, nodelessID)
		assert.ErrorIs(t, err, db.ErrNotFound,
			"the aborted migration must leave the unrelated nodeless contact without a node")
	})
}
