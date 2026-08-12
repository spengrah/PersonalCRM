//go:build integration_testdb

// The derived-writer trigger's behavioral coverage (arc GI-4 / PR7). Migration
// 079 pushes the eight-derived-column sole-writer rule from Go convention
// (backend/tests/sole_writer_static_test.go, scripts/check-cadence-sole-writer.sh)
// into the schema itself: a BEFORE UPDATE trigger on contact that rejects any
// change to a derived column unless the transaction has declared its owner via
// repository.SetDerivedWriterTx.
//
// See .ai/log/plan/arch-hygiene-pr7.md D7-6 for the coverage rationale (48
// rejection subtests: 5 unauthorized-cadence + 3 unauthorized-knowledge + 40
// wrong-owner) and the freshness rule that makes the "unset" branch distinct
// from the wrong-owner table's "" row.
package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// derivedColumnCase is one row of the shared column table driving all three
// rejection tests (D7-6). owner names the value that WOULD authorize the
// write; isNil is the distinctness-rule precondition probe; attempt performs
// the unauthorized write itself.
type derivedColumnCase struct {
	column  string
	owner   repository.DerivedWriter
	isNil   func(c *repository.Contact) bool
	attempt func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// cadenceDerivedColumnCases is the five cadence rows, each calling
// TestWriteCadenceColumnsWithoutGUCTx with exactly one field set.
func cadenceDerivedColumnCases() []derivedColumnCase {
	now := accelerated.GetCurrentTime()
	return []derivedColumnCase{
		{
			column: "last_contacted",
			owner:  repository.DerivedWriterCadence,
			isNil:  func(c *repository.Contact) bool { return c.LastContacted == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, id, repository.TestCadenceSeed{LastContacted: &now})
			},
		},
		{
			column: "last_interaction_at",
			owner:  repository.DerivedWriterCadence,
			isNil:  func(c *repository.Contact) bool { return c.LastInteractionAt == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, id, repository.TestCadenceSeed{LastInteractionAt: &now})
			},
		},
		{
			column: "last_outreach_at",
			owner:  repository.DerivedWriterCadence,
			isNil:  func(c *repository.Contact) bool { return c.LastOutreachAt == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, id, repository.TestCadenceSeed{LastOutreachAt: &now})
			},
		},
		{
			column: "last_response_at",
			owner:  repository.DerivedWriterCadence,
			isNil:  func(c *repository.Contact) bool { return c.LastResponseAt == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, id, repository.TestCadenceSeed{LastResponseAt: &now})
			},
		},
		{
			column: "contact_by",
			owner:  repository.DerivedWriterCadence,
			isNil:  func(c *repository.Contact) bool { return c.ContactBy == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, id, repository.TestCadenceSeed{ContactBy: &now})
			},
		},
	}
}

// knowledgeDerivedColumnCases is the three knowledge rows, each calling
// TestWriteKnowledgeColumnsWithoutGUCTx with exactly one field set.
func knowledgeDerivedColumnCases() []derivedColumnCase {
	loc := "Derived Writer Test City"
	bday := accelerated.GetCurrentTime()
	hm := "Derived writer test how_met"
	return []derivedColumnCase{
		{
			column: "location",
			owner:  repository.DerivedWriterKnowledgeCache,
			isNil:  func(c *repository.Contact) bool { return c.Location == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteKnowledgeColumnsWithoutGUCTx(ctx, tx, id, repository.TestKnowledgeSeed{Location: &loc})
			},
		},
		{
			column: "birthday",
			owner:  repository.DerivedWriterKnowledgeCache,
			isNil:  func(c *repository.Contact) bool { return c.Birthday == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteKnowledgeColumnsWithoutGUCTx(ctx, tx, id, repository.TestKnowledgeSeed{Birthday: &bday})
			},
		},
		{
			column: "how_met",
			owner:  repository.DerivedWriterKnowledgeCache,
			isNil:  func(c *repository.Contact) bool { return c.HowMet == nil },
			attempt: func(r *repository.ContactRepository, ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return r.TestWriteKnowledgeColumnsWithoutGUCTx(ctx, tx, id, repository.TestKnowledgeSeed{HowMet: &hm})
			},
		},
	}
}

// allDerivedColumnCases is the CONCATENATION of the other two — not a
// hand-written union — so a column covered by the unauthorized tests can
// never go silently missing from the wrong-owner matrix (D7-6).
func allDerivedColumnCases() []derivedColumnCase {
	cases := append([]derivedColumnCase{}, cadenceDerivedColumnCases()...)
	return append(cases, knowledgeDerivedColumnCases()...)
}

// assertRejected requires err is a *pgconn.PgError with code P0001
// (pgerrcode.RaiseException) whose message names the offending column.
func assertRejected(t *testing.T, err error, column string) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.Truef(t, errors.As(err, &pgErr), "expected a *pgconn.PgError, got %v", err)
	assert.Equal(t, pgerrcode.RaiseException, pgErr.Code)
	assert.Contains(t, pgErr.Message, "contact."+column)
}

// runUnauthorizedRejectionSubtests is the shared body for
// TestDerivedWriterTrigger_RejectsUnauthorizedCadenceWrite and
// …RejectsUnauthorizedKnowledgeWrite (D7-6, "the arc's two named rejection
// tests"). Every write runs on a dedicated physical connection that has NEVER
// set crm.derived_writer (the freshness rule) — never the shared package
// pool, whose connections are recycled across tests that DO declare owners.
func runUnauthorizedRejectionSubtests(t *testing.T, cases []derivedColumnCase) {
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	contactRepo := repository.NewContactRepository(database.Queries)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.column, func(t *testing.T) {
			// Distinctness rule: a fresh, cadence-less contact so all eight
			// derived columns are NULL and the trigger's IS DISTINCT FROM
			// unconditionally fires.
			contact, err := h.SeedContact(ctx, gen.Contact())
			require.NoError(t, err)

			got, err := contactRepo.GetContact(ctx, contact.ID)
			require.NoError(t, err)
			require.True(t, tc.isNil(got), "precondition: contact.%s must be NULL on a fresh cadence-less contact", tc.column)

			// Freshness rule: a dedicated connection that has never run
			// set_config, so current_setting(..., true) is genuinely NULL
			// rather than the placeholder's post-set reset value ''.
			conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close(ctx) })

			tx, err := conn.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()

			isNull, err := db.New(tx).TestDerivedWriterSettingIsNull(ctx)
			require.NoError(t, err)
			require.True(t, isNull, "freshness precondition: crm.derived_writer must read NULL on a virgin connection")

			err = tc.attempt(contactRepo, ctx, tx, contact.ID)
			assertRejected(t, err, tc.column)
		})
	}
}

// TestDerivedWriterTrigger_RejectsUnauthorizedCadenceWrite is one of the
// arc's two named rejection tests (arch-hygiene.md floor list). No GUC is
// ever set on these subtests' connections.
func TestDerivedWriterTrigger_RejectsUnauthorizedCadenceWrite(t *testing.T) {
	t.Parallel()
	runUnauthorizedRejectionSubtests(t, cadenceDerivedColumnCases())
}

// TestDerivedWriterTrigger_RejectsUnauthorizedKnowledgeWrite is the other of
// the arc's two named rejection tests.
func TestDerivedWriterTrigger_RejectsUnauthorizedKnowledgeWrite(t *testing.T) {
	t.Parallel()
	runUnauthorizedRejectionSubtests(t, knowledgeDerivedColumnCases())
}

// TestDerivedWriterTrigger_RejectsWrongOwner is the matrix proof: every
// derived column crossed with five non-owning GUC values (D7-6) — 40
// subtests. Unlike the two unauthorized tests, each subtest declares a value
// itself, so it needs no dedicated connection: the shared package pool is
// fine because the test is testing the DECLARED-BUT-WRONG branch, not the
// absence of a declaration.
func TestDerivedWriterTrigger_RejectsWrongOwner(t *testing.T) {
	t.Parallel()
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	contactRepo := repository.NewContactRepository(database.Queries)

	for _, tc := range allDerivedColumnCases() {
		tc := tc
		otherOwner := repository.DerivedWriterKnowledgeCache
		if tc.owner == repository.DerivedWriterKnowledgeCache {
			otherOwner = repository.DerivedWriterCadence
		}
		guardCases := []struct {
			name  string
			value string
		}{
			{"empty", ""},
			{"bogus", "bogus"},
			{"padded_own_owner", " " + string(tc.owner) + " "},
			{"uppercase_own_owner", strings.ToUpper(string(tc.owner))},
			{"other_valid_owner", string(otherOwner)},
		}
		for _, gc := range guardCases {
			gc := gc
			t.Run(tc.column+"/"+gc.name, func(t *testing.T) {
				contact, err := h.SeedContact(ctx, gen.Contact())
				require.NoError(t, err)

				got, err := contactRepo.GetContact(ctx, contact.ID)
				require.NoError(t, err)
				require.True(t, tc.isNil(got), "precondition: contact.%s must be NULL on a fresh cadence-less contact", tc.column)

				tx, err := database.Pool.Begin(ctx)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback(ctx) }()

				require.NoError(t, db.New(tx).SetDerivedWriter(ctx, gc.value))

				err = tc.attempt(contactRepo, ctx, tx, contact.ID)
				assertRejected(t, err, tc.column)
			})
		}
	}
}

// TestDerivedWriterTrigger_AllowsOwningWrites is the regression pin: a
// correctly-authorized write of every derived column succeeds. It passes
// identically on the pre-079 tree — its value is entirely as a pin, not as
// evidence the trigger works.
func TestDerivedWriterTrigger_AllowsOwningWrites(t *testing.T) {
	t.Parallel()
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	contactRepo := repository.NewContactRepository(database.Queries)

	t.Run("cadence", func(t *testing.T) {
		contact, err := h.SeedContact(ctx, gen.Contact())
		require.NoError(t, err)

		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		now := accelerated.GetCurrentTime()
		seed := repository.TestCadenceSeed{
			LastContacted:     &now,
			LastInteractionAt: &now,
			LastOutreachAt:    &now,
			LastResponseAt:    &now,
			ContactBy:         &now,
		}
		require.NoError(t, contactRepo.TestSeedContactCadenceFieldsTx(ctx, tx, contact.ID, seed))
		require.NoError(t, tx.Commit(ctx))

		got, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.NotNil(t, got.LastContacted)
		assert.NotNil(t, got.LastInteractionAt)
		assert.NotNil(t, got.LastOutreachAt)
		assert.NotNil(t, got.LastResponseAt)
		assert.NotNil(t, got.ContactBy)
	})

	t.Run("knowledge_cache", func(t *testing.T) {
		contact, err := h.SeedContact(ctx, gen.Contact())
		require.NoError(t, err)

		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		loc := "Allowed Location"
		bday := accelerated.GetCurrentTime()
		hm := "Allowed how_met"
		require.NoError(t, contactRepo.UpdateContactLocationCacheTx(ctx, tx, contact.ID, &loc))
		require.NoError(t, contactRepo.UpdateContactBirthdayCacheTx(ctx, tx, contact.ID, &bday))
		require.NoError(t, contactRepo.UpdateContactHowMetCacheTx(ctx, tx, contact.ID, &hm))
		require.NoError(t, tx.Commit(ctx))

		got, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.NotNil(t, got.Location)
		assert.NotNil(t, got.Birthday)
		assert.NotNil(t, got.HowMet)
	})
}

// TestDerivedWriterTrigger_IgnoresProfileOnlyUpdate proves the per-column
// IS DISTINCT FROM rather than a blanket UPDATE veto: UpdateContact's
// profile-only SET clause (full_name, cadence, profile_photo, updated_at)
// touches none of the eight derived columns, so it must succeed with no GUC
// declared anywhere. Regression pin — passes identically pre-079.
func TestDerivedWriterTrigger_IgnoresProfileOnlyUpdate(t *testing.T) {
	t.Parallel()
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	contactRepo := repository.NewContactRepository(database.Queries)

	contact, err := h.SeedContact(ctx, gen.Contact())
	require.NoError(t, err)

	photo := "https://example.com/photo.jpg"
	updated, err := contactRepo.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName:     contact.FullName + " Updated",
		Cadence:      strPtr("monthly"),
		ProfilePhoto: &photo,
	})
	require.NoError(t, err, "profile-only UpdateContact with no GUC set must succeed")
	require.NotNil(t, updated)
}

// TestDerivedWriterTrigger_InsertUnaffected proves the trigger is UPDATE-only:
// CreateContact WITH a cadence, no GUC declared, must still succeed and
// populate contact_by at create time while leaving last_contacted unset (per
// CON-001). This is the one test in this file that creates through the
// repository directly rather than the synthetic harness: the create path
// itself is the subject under test (testing.md's rule governs building STATE
// for a test, not exercising the code the test is about).
func TestDerivedWriterTrigger_InsertUnaffected(t *testing.T) {
	t.Parallel()
	database, _ := newSyntheticDB(t)
	ctx := context.Background()
	contactRepo := repository.NewContactRepository(database.Queries)

	cadenceVal := "monthly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Derived Writer Insert Unaffected " + uuid.NewString(),
		Cadence:  &cadenceVal,
	})
	require.NoError(t, err, "CreateContact with a cadence and no GUC declared must succeed — the trigger is UPDATE-only")
	require.NotNil(t, contact.ContactBy, "contact_by is derived from the cadence at create time")
	require.Nil(t, contact.LastContacted, "last_contacted is left unset on create per CON-001")
}

// TestDerivedWriterGUC_PoolSafety proves crm.derived_writer never leaks
// across a transaction or a pool checkout boundary. Deliberately NOT
// t.Parallel(): its subtests share one MaxConns=1 pinned pool and must run in
// order.
func TestDerivedWriterGUC_PoolSafety(t *testing.T) {
	database, ctx := newSyntheticDB(t)
	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	contactRepo := repository.NewContactRepository(database.Queries)

	poolCfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	seedContact := func(t *testing.T) *repository.Contact {
		t.Helper()
		c, err := h.SeedContact(ctx, gen.Contact())
		require.NoError(t, err)
		return c
	}

	// Subtest 1 obeys the freshness rule's sibling for a DIFFERENT reason: it
	// is the only pool-safety subtest that both sets AND consumes the GUC
	// within the same transaction, so it is a positive control rather than a
	// leakage probe.
	t.Run("set_and_consumed_same_tx", func(t *testing.T) {
		contactA := seedContact(t)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		require.NoError(t, repository.SetDerivedWriterTx(ctx, tx, repository.DerivedWriterCadence))
		now := accelerated.GetCurrentTime()
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, contactA.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, tx.Commit(ctx))
	})

	// Subtests 2-5 deliberately do NOT assert TestDerivedWriterSettingIsNull:
	// by the time each attempts its unauthorized write, the pinned backend
	// has already run set_config in an earlier transaction, so the
	// placeholder's reset value is '' rather than NULL. Both are non-owning
	// and the trigger rejects both identically — that is the correct
	// observation here, not a defect to "fix".
	t.Run("gone_after_commit", func(t *testing.T) {
		contactA := seedContact(t)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, repository.SetDerivedWriterTx(ctx, tx, repository.DerivedWriterCadence))
		pidBefore, err := db.New(tx).TestBackendPID(ctx)
		require.NoError(t, err)
		now := accelerated.GetCurrentTime()
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, contactA.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, tx.Commit(ctx))

		contactB := seedContact(t)
		tx2, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		pidAfter, err := db.New(tx2).TestBackendPID(ctx)
		require.NoError(t, err)
		require.Equal(t, pidBefore, pidAfter, "MaxConns=1 pool must reuse the same backend")

		err = contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx2, contactB.ID, repository.TestCadenceSeed{LastContacted: &now})
		assertRejected(t, err, "last_contacted")
	})

	t.Run("gone_after_rollback", func(t *testing.T) {
		contactA := seedContact(t)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, repository.SetDerivedWriterTx(ctx, tx, repository.DerivedWriterCadence))
		pidBefore, err := db.New(tx).TestBackendPID(ctx)
		require.NoError(t, err)
		now := accelerated.GetCurrentTime()
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, contactA.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, tx.Rollback(ctx))

		contactB := seedContact(t)
		tx2, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		pidAfter, err := db.New(tx2).TestBackendPID(ctx)
		require.NoError(t, err)
		require.Equal(t, pidBefore, pidAfter, "MaxConns=1 pool must reuse the same backend")

		err = contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx2, contactB.ID, repository.TestCadenceSeed{LastContacted: &now})
		assertRejected(t, err, "last_contacted")
	})

	t.Run("no_leak_across_checkout", func(t *testing.T) {
		contactA := seedContact(t)
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		pidBefore, err := db.New(tx).TestBackendPID(ctx)
		require.NoError(t, err)
		require.NoError(t, repository.SetDerivedWriterTx(ctx, tx, repository.DerivedWriterCadence))
		now := accelerated.GetCurrentTime()
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, contactA.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, tx.Commit(ctx))
		conn.Release()

		contactB := seedContact(t)
		conn2, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer conn2.Release()
		tx2, err := conn2.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		pidAfter, err := db.New(tx2).TestBackendPID(ctx)
		require.NoError(t, err)
		require.Equal(t, pidBefore, pidAfter, "MaxConns=1 pool must reuse the same backend across an explicit acquire/release")

		err = contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx2, contactB.ID, repository.TestCadenceSeed{LastContacted: &now})
		assertRejected(t, err, "last_contacted")
	})

	t.Run("savepoint_semantics", func(t *testing.T) {
		now := accelerated.GetCurrentTime()

		// Tx A: declare BEFORE the savepoint. SET LOCAL is transaction-scoped,
		// not savepoint-scoped, so a ROLLBACK TO SAVEPOINT preserves it.
		contactA1 := seedContact(t)
		contactA2 := seedContact(t)
		txA, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = txA.Rollback(ctx) }()
		require.NoError(t, repository.SetDerivedWriterTx(ctx, txA, repository.DerivedWriterCadence))

		spA, err := txA.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, spA, contactA1.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, spA.Rollback(ctx))

		// The declaration made before the savepoint is still in force.
		require.NoError(t, contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, txA, contactA2.ID, repository.TestCadenceSeed{LastContacted: &now}))
		require.NoError(t, txA.Commit(ctx))

		// Tx B: declare INSIDE the savepoint. Rolling back the savepoint rolls
		// back the set_config call along with it.
		contactB := seedContact(t)
		txB, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = txB.Rollback(ctx) }()

		spB, err := txB.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, repository.SetDerivedWriterTx(ctx, spB, repository.DerivedWriterCadence))
		require.NoError(t, spB.Rollback(ctx))

		err = contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, txB, contactB.ID, repository.TestCadenceSeed{LastContacted: &now})
		assertRejected(t, err, "last_contacted")
	})
}

// contactDerivedWriterPreVersion is the golang-migrate version immediately
// before 079. Positioning the clone here first makes Steps(1)/Steps(-1) act
// on 079 specifically, robust to later migrations landing above it — the same
// discipline as whatsappFoundationsVersion (migration_076_down_test.go).
const contactDerivedWriterPreVersion = 78

// TestDerivedWriterTrigger_MigrationUpDown is the arc's named round-trip
// test. Serial (migration-subject) on its own ephemeral clone, following
// migration_076_down_test.go's clone + migrate.New + m.Migrate(N) pattern.
//
// This test uses the direct-repository fallback (testing.md exception (b))
// rather than the synthetic harness: it needs only a single bare cadence-less
// contact per attempt, never replay or matching, so standing up the full
// harness (River client, contact service, matcher) on a short-lived ephemeral
// clone buys nothing over repository.NewContactRepository(database.Queries).
func TestDerivedWriterTrigger_MigrationUpDown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

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

	if err := m.Migrate(contactDerivedWriterPreVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the pre-079 tip")
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	freshContact := func(t *testing.T) *repository.Contact {
		t.Helper()
		c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Migration 079 Fixture " + uuid.NewString(),
		})
		require.NoError(t, err)
		return c
	}
	attemptUnauthorized := func(t *testing.T) error {
		t.Helper()
		c := freshContact(t)
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		now := accelerated.GetCurrentTime()
		writeErr := contactRepo.TestWriteCadenceColumnsWithoutGUCTx(ctx, tx, c.ID, repository.TestCadenceSeed{LastContacted: &now})
		if writeErr == nil {
			require.NoError(t, tx.Commit(ctx))
		}
		return writeErr
	}
	countTrigger := func(t *testing.T) int64 {
		t.Helper()
		n, err := database.Queries.TestCountTriggers(ctx, db.TestCountTriggersParams{
			TableName:   "contact",
			TriggerName: "reject_unauthorized_derived_contact_write",
		})
		require.NoError(t, err)
		return n
	}
	funcExists := func(t *testing.T) bool {
		t.Helper()
		exists, err := database.Queries.TestRegprocedureExists(ctx, "reject_unauthorized_derived_contact_write()")
		require.NoError(t, err)
		return exists
	}

	// 1. Up.
	require.NoError(t, m.Steps(1))

	// 2. Catalog: both objects present.
	require.EqualValues(t, 1, countTrigger(t))
	require.True(t, funcExists(t))

	// 3. Behavior: unauthorized write raises.
	assertRejected(t, attemptUnauthorized(t), "last_contacted")

	// 4. Down.
	require.NoError(t, m.Steps(-1))

	// 5. Catalog: BOTH gone — this is the step that closes D7-9's vacuity.
	// The up uses CREATE OR REPLACE FUNCTION, so a down that dropped only the
	// trigger would leave the function behind and every behavioral assertion
	// would still pass on reapply.
	require.EqualValues(t, 0, countTrigger(t))
	require.False(t, funcExists(t),
		"079 down must DROP FUNCTION reject_unauthorized_derived_contact_write() — "+
			"the up uses CREATE OR REPLACE, so a leftover function is invisible to every behavioral assertion")

	// 6. Behavior: now succeeds.
	require.NoError(t, attemptUnauthorized(t), "after the down, the same write must succeed")

	// 7. Reapply: rejection comes back — proves the down left the schema
	// genuinely re-appliable rather than merely quiet.
	require.NoError(t, m.Steps(1))
	assertRejected(t, attemptUnauthorized(t), "last_contacted")
}
