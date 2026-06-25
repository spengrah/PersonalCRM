//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

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

// dormantCleanupVersion is the golang-migrate version of the dormant-table
// cleanup migration (072). The round-trip positions the clone here first so
// Steps(-1) rolls down 072 specifically, robust to later migrations landing
// above it.
const dormantCleanupVersion = 72

// dormantTables are the four tables 072 drops. The round-trip asserts they are
// ABSENT after up and PRESENT after down.
var dormantTables = []string{"connection", "contact_summary", "note_embedding", "prompt_query"}

// tablesPresent returns the subset of names present in the public schema.
func tablesPresent(ctx context.Context, t *testing.T, support *repository.SyntheticSupportRepository, names []string) map[string]bool {
	t.Helper()
	all, err := support.ListPublicTables(ctx)
	require.NoError(t, err)
	live := make(map[string]bool, len(all))
	for _, n := range all {
		live[n] = true
	}
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = live[n]
	}
	return present
}

// TestDormantCleanup_MigrationDownUp exercises the 072 up → down → up round-trip
// against an isolated clone (it rolls the schema down, so it cannot share the
// package DB). It proves the four dormant tables drop on up, come back on down
// (with connection's indexes/constraints), and drop again on a second up.
func TestDormantCleanup_MigrationDownUp(t *testing.T) {
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

	support := repository.NewSyntheticSupportRepository(database.Queries)
	fixtureRepo := repository.NewTestJSONBFixturesRepository(database.Queries)

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Position the clone at the cleanup tip (072) FIRST, so Steps(-1) rolls down 072
	// specifically — robust to later migrations being added above it. Migrate(72) is
	// a no-op today (the template clone is already past 72: ErrNoChange).
	if err := m.Migrate(dormantCleanupVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the dormant-cleanup tip")
	}

	// Post-up state: the four tables are ABSENT.
	present := tablesPresent(ctx, t, support, dormantTables)
	for _, tbl := range dormantTables {
		assert.Falsef(t, present[tbl], "table %s must be dropped after 072 up", tbl)
	}

	// Roll down ONE step: 072 down — the four tables come back.
	require.NoError(t, m.Steps(-1), "roll the dormant-cleanup migration down one step")
	present = tablesPresent(ctx, t, support, dormantTables)
	for _, tbl := range dormantTables {
		assert.Truef(t, present[tbl], "table %s must be recreated after 072 down", tbl)
	}

	// connection's two indexes must be recreated by the down migration (a bare
	// table recreate without them would be a silent regression).
	for _, idx := range []string{"idx_connection_contact_a", "idx_connection_contact_b"} {
		exists, err := fixtureRepo.IndexExists(ctx, idx)
		require.NoError(t, err)
		assert.Truef(t, exists, "index %s must be recreated by 072 down", idx)
	}
	// connection's named constraints must be restored too: the unique, the self
	// CHECK, and the unnamed strength range CHECK. Probe via pg_constraint through
	// a raw read (schema shape is the subject under test).
	assert.True(t, constraintExists(ctx, t, database, "unique_connection"),
		"unique_connection must be recreated by 072 down")
	assert.True(t, constraintExists(ctx, t, database, "no_self_connection"),
		"no_self_connection CHECK must be recreated by 072 down")
	// The strength CHECK must be recreated enforcing EXACTLY the contiguous 1..5
	// range — not merely some CHECK on the column, not a wrong-bounds predicate, and
	// not a discrete set like IN (1,5) that boundary-only probing would accept.
	// Prove it behaviorally: EVERY in-range value (1,2,3,4,5) is accepted, and each
	// out-of-range value (0,6) is rejected specifically by a CHECK violation
	// (SQLSTATE 23514) — not a NULL/FK error masquerading as a pass. Two real
	// contacts satisfy the FK + no_self_connection CHECK; the connection inserts are
	// raw SQL (subject under test — the table is dropped post-072, so no sqlc query
	// exists).
	contactA := uuid.New()
	contactB := uuid.New()
	require.NoError(t, support.InsertContactAtID(ctx, contactA, "strength-check contact A"))
	require.NoError(t, support.InsertContactAtID(ctx, contactB, "strength-check contact B"))
	insertConnStrength := func(strength int) error {
		_, err := database.Pool.Exec(ctx,
			`INSERT INTO connection (contact_a_id, contact_b_id, strength) VALUES ($1, $2, $3)`,
			contactA, contactB, strength)
		return err
	}
	// Every in-range value 1..5 is accepted (delete between inserts to dodge
	// unique_connection on the fixed contact pair).
	for s := 1; s <= 5; s++ {
		require.NoErrorf(t, insertConnStrength(s), "strength=%d must satisfy the recreated CHECK", s)
		_, err = database.Pool.Exec(ctx, `DELETE FROM connection WHERE contact_a_id = $1`, contactA)
		require.NoError(t, err)
	}
	// Out-of-range values are rejected by the CHECK specifically (SQLSTATE 23514),
	// not by some other constraint.
	for _, s := range []int{0, 6} {
		err := insertConnStrength(s)
		var pgErr *pgconn.PgError
		require.Truef(t, errors.As(err, &pgErr), "strength=%d must fail with a pg error, got %v", s, err)
		assert.Equalf(t, pgerrcode.CheckViolation, pgErr.Code,
			"strength=%d must be rejected by a CHECK violation (23514), got SQLSTATE %s", s, pgErr.Code)
	}

	// Roll back up: 072 up again — the four tables drop once more (proves up is
	// re-appliable after a down).
	require.NoError(t, m.Steps(1), "re-apply the dormant-cleanup migration")
	present = tablesPresent(ctx, t, support, dormantTables)
	for _, tbl := range dormantTables {
		assert.Falsef(t, present[tbl], "table %s must be dropped again after re-applying 072 up", tbl)
	}
}

// constraintExists reports whether a named constraint exists in the public
// schema. Raw read — schema shape is the subject under test.
func constraintExists(ctx context.Context, t *testing.T, database *db.Database, name string) bool {
	t.Helper()
	var exists bool
	err := database.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`,
		name).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// TestDormantCleanup_GuardAbortsOnNonEmptyTable proves the in-transaction
// empty-table guard: with a row present in one of the dormant tables, applying
// 072 up FAILS (the RAISE EXCEPTION aborts the whole transaction) and the table
// survives. The clone is positioned at 071 (where the dormant tables still
// exist) so we can plant a row before applying 072.
func TestDormantCleanup_GuardAbortsOnNonEmptyTable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	support := repository.NewSyntheticSupportRepository(database.Queries)

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Position the clone at 071 — the last version where the dormant tables still
	// exist, so we can plant a row before 072 tries to drop them.
	require.NoError(t, m.Migrate(dormantCleanupVersion-1),
		"position the clone at 071 (dormant tables still present)")

	// Plant a contact_summary row. The FK needs a real contact, created via the
	// sqlc path (contact is not dropped). The contact_summary INSERT itself is raw
	// SQL: a test-only sqlc query is impossible here because sqlc compiles every
	// query against the post-072 schema where contact_summary is already dropped.
	// Raw SQL is allowed as the subject under test (the migration guard), same
	// justification as the migration-runner raw-SQL tests in integration_test.go.
	contactID := uuid.New()
	require.NoError(t, support.InsertContactAtID(ctx, contactID, "guard-test contact"))
	_, err = database.Pool.Exec(ctx,
		`INSERT INTO contact_summary (contact_id, summary) VALUES ($1, $2)`,
		contactID, "guard-test summary")
	require.NoError(t, err, "plant a contact_summary row at version 071")

	// Apply 072 up: the empty-table guard must fire and abort the whole migration.
	err = m.Steps(1)
	require.Error(t, err, "072 up must fail when a dormant table holds a row")
	assert.Contains(t, err.Error(), "cannot drop contact_summary",
		"the failure must be the empty-table guard's RAISE EXCEPTION")

	// Close the migrate handle NOW, before the survival queries. The failed 072 up
	// raised inside its own transaction while holding ACCESS EXCLUSIVE locks on the
	// dormant tables; closing releases migrate's pinned connection (and any lock it
	// still holds) so the COUNT(*) below cannot block behind it. golang-migrate also
	// marks the version dirty on a failed step, so Force it clean first — the same
	// recovery the existing guarded-migration tests use (e.g. mac_host_migration_test.go,
	// phone_call_migration_test.go). The schema is genuinely back at 071 after the
	// transaction rolled back, so Force to 071.
	require.NoError(t, m.Force(dormantCleanupVersion-1), "clear the dirty flag the failed 072 up set")
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr, "close migrate source")
	require.NoError(t, dbErr, "close migrate database handle (release its connection + lock) before survival queries")

	// The transaction rolled back, so contact_summary still exists with its row —
	// nothing was dropped.
	present := tablesPresent(ctx, t, support, dormantTables)
	for _, tbl := range dormantTables {
		assert.Truef(t, present[tbl], "table %s must survive the aborted 072 up", tbl)
	}
	count, err := support.CountAllRows(ctx, "contact_summary")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the planted contact_summary row must survive the aborted drop")
}
