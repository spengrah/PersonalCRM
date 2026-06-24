//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAssertionStore_MigrationDownUp exercises the 067 down + up round-trip
// against an isolated clone (it rolls the schema down, so it cannot share the
// package DB). It proves the tables drop cleanly (provenance first, then
// assertion — the FK order) and re-create with the constraints intact.
func TestAssertionStore_MigrationDownUp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	// Migration-subject test: rolls the schema down, so it stays serial and uses an
	// isolated clone (never the shared package DB).

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	nodeRepo := repository.NewNodeRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)

	// The clone is template-migrated, so the assertion store is present up front:
	// a subject node + an assertion + a provenance locator round-trips.
	subjectID := uuid.New()
	_, err = nodeRepo.CreateNode(ctx, subjectID, repository.NodeTypePerson, "migration-subject")
	require.NoError(t, err)

	value := "before-rollback"
	a, err := assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  subjectID,
		PredicateKey:   "home_address",
		ValueText:      &value,
		KnowledgeFrom:  time.Now().UTC(),
		Confidence:     80,
		Salience:       45,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "migration-prop-1",
	})
	require.NoError(t, err)
	inserted, err := assertionRepo.InsertProvenance(ctx, repository.InsertProvenanceParams{
		AssertionID:  a.ID,
		LocatorHash:  "migration-loc-1",
		SourceKind:   repository.SourceKindUser,
		SourceID:     "migration-edit-1",
		ProducerKind: repository.ProducerKindUser,
	})
	require.NoError(t, err)
	require.True(t, inserted)

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Roll down ONE step: 067 (the assertion store) down — both tables are dropped
	// (provenance first per the down migration's FK order). A query against the
	// assertion table now errors (the relation is gone, not ErrNotFound).
	require.NoError(t, m.Steps(-1), "roll the assertion-store migration down one step")
	_, err = assertionRepo.GetAssertion(ctx, a.ID)
	require.Error(t, err, "assertion table is dropped after the down migration")

	// Roll back up: the tables are recreated with the constraints intact. The old
	// row does NOT come back (the table was dropped + recreated), and a fresh
	// insert succeeds — but the exactly-one-payload CHECK still rejects a bad row,
	// proving the up migration reinstalled the constraints.
	require.NoError(t, m.Steps(1), "re-apply the assertion store")
	_, err = assertionRepo.GetAssertion(ctx, a.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "table drop+recreate does not restore the old row")

	fresh, err := assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  subjectID,
		PredicateKey:   "home_address",
		ValueText:      &value,
		KnowledgeFrom:  time.Now().UTC(),
		Confidence:     80,
		Salience:       45,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "migration-prop-2",
	})
	require.NoError(t, err, "the recreated table accepts a valid insert")
	assert.NotEqual(t, uuid.Nil, fresh.ID)

	// The reinstalled exactly-one-payload CHECK rejects a zero-payload row.
	_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  subjectID,
		PredicateKey:   "home_address",
		KnowledgeFrom:  time.Now().UTC(),
		Confidence:     80,
		Salience:       45,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "migration-prop-3",
	})
	require.Error(t, err, "the recreated table re-enforces assertion_one_payload")
}
