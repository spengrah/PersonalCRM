// Package tests — DB-free guard-clause coverage for the derived-writer
// authorization helpers (backend/internal/repository/derived_writer.go).
// Deliberately untagged so it runs under `make test-unit`, alongside
// sole_writer_static_test.go: these guard clauses gate every derived-column
// write before a transaction is even touched, so they earn coverage that
// does not depend on a database.
package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestDerivedWriterHelpers_RejectsUnknownOwner asserts SetDerivedWriterTx
// rejects any DerivedWriter value outside the two declared constants before
// it ever touches a transaction. The owner check runs before the nil-tx
// check (see derived_writer.go), so passing a nil tx here still exercises
// only the owner branch.
func TestDerivedWriterHelpers_RejectsUnknownOwner(t *testing.T) {
	t.Parallel()
	err := repository.SetDerivedWriterTx(context.Background(), nil, repository.DerivedWriter("bogus"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown owner")
}

// TestDerivedWriterHelpers_RejectsNilTx asserts SetDerivedWriterTx rejects a
// nil tx for a KNOWN owner — the owner branch passes, so this exercises the
// second guard clause.
func TestDerivedWriterHelpers_RejectsNilTx(t *testing.T) {
	t.Parallel()
	err := repository.SetDerivedWriterTx(context.Background(), nil, repository.DerivedWriterCadence)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil tx")
}

// TestDerivedWriterHelpers_SeedRequiresPool asserts the pool-level cadence
// fixture writer fails fast on a repository with no configured pool, before
// ever touching its (nil) querier — the r.pool == nil check returns before
// TestSeedContactCadenceFieldsTx or the querier is reached, so a nil querier
// is safe here.
func TestDerivedWriterHelpers_SeedRequiresPool(t *testing.T) {
	t.Parallel()
	repo := repository.NewContactRepository(nil)
	err := repo.TestSeedContactCadenceFields(context.Background(), uuid.New(), repository.TestCadenceSeed{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SetPool")
}
