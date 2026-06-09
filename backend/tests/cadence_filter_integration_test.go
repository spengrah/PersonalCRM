package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCadenceFilter_Integration tests the cadence_filter parameter for listing contacts.
func TestCadenceFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Migrations are applied once by TestMain.
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	// Seed test contacts via the synthetic factory: one with cadence, one
	// without. Both are referenced by ID only, so namespaced names are fine.
	withCadence, withCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
	defer withCleanup()

	withoutCadence, withoutCleanup := seedMigrationContact(ctx, t, database, gen)
	defer withoutCleanup()

	t.Run("ListContacts_NoCadenceFilter", func(t *testing.T) {
		results, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:         100,
			Offset:        0,
			CadenceFilter: "",
		})
		require.NoError(t, err)

		// Both contacts should be present
		foundWith, foundWithout := false, false
		for _, c := range results {
			if c.ID == withCadence.ID {
				foundWith = true
			}
			if c.ID == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.True(t, foundWith, "contact with cadence should be in unfiltered results")
		assert.True(t, foundWithout, "contact without cadence should be in unfiltered results")
	})

	t.Run("ListContacts_HasCadence", func(t *testing.T) {
		results, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:         100,
			Offset:        0,
			CadenceFilter: "has_cadence",
		})
		require.NoError(t, err)

		foundWith, foundWithout := false, false
		for _, c := range results {
			if c.ID == withCadence.ID {
				foundWith = true
			}
			if c.ID == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.True(t, foundWith, "contact with cadence should be in has_cadence results")
		assert.False(t, foundWithout, "contact without cadence should NOT be in has_cadence results")
	})

	t.Run("ListContacts_NoCadence", func(t *testing.T) {
		results, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:         100,
			Offset:        0,
			CadenceFilter: "no_cadence",
		})
		require.NoError(t, err)

		foundWith, foundWithout := false, false
		for _, c := range results {
			if c.ID == withCadence.ID {
				foundWith = true
			}
			if c.ID == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.False(t, foundWith, "contact with cadence should NOT be in no_cadence results")
		assert.True(t, foundWithout, "contact without cadence should be in no_cadence results")
	})

	t.Run("ListContactsSorted_HasCadence", func(t *testing.T) {
		results, err := repo.ListContacts(ctx, repository.ListContactsParams{
			Limit:         100,
			Offset:        0,
			Sort:          "name",
			Order:         "asc",
			CadenceFilter: "has_cadence",
		})
		require.NoError(t, err)

		foundWith, foundWithout := false, false
		for _, c := range results {
			if c.ID == withCadence.ID {
				foundWith = true
			}
			if c.ID == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.True(t, foundWith, "contact with cadence should be in sorted has_cadence results")
		assert.False(t, foundWithout, "contact without cadence should NOT be in sorted has_cadence results")
	})

	t.Run("CountContacts_HasCadence", func(t *testing.T) {
		// CountContacts is a DB-wide COUNT(*), so it sees other parallel tests'
		// rows. We only assert it is >= 1 for each partition: this test's own
		// withCadence/withoutCadence rows guarantee at least one in each, and
		// concurrent tests can only ADD rows, never drop the count below our
		// contribution. (A stricter "== N" over the whole table would be
		// shared-DB-unsafe under t.Parallel().)
		totalHas, err := repo.CountContacts(ctx, "has_cadence", "")
		require.NoError(t, err)

		totalNo, err := repo.CountContacts(ctx, "no_cadence", "")
		require.NoError(t, err)

		assert.GreaterOrEqual(t, totalHas, int64(1), "this test's own cadence contact guarantees >= 1")
		assert.GreaterOrEqual(t, totalNo, int64(1), "this test's own no-cadence contact guarantees >= 1")
	})

	t.Run("ListContactIDs_HasCadence", func(t *testing.T) {
		ids, err := repo.ListContactIDs(ctx, repository.ListContactIDsParams{
			CadenceFilter: "has_cadence",
		})
		require.NoError(t, err)

		foundWith, foundWithout := false, false
		for _, id := range ids {
			if id == withCadence.ID {
				foundWith = true
			}
			if id == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.True(t, foundWith, "contact with cadence should be in has_cadence IDs")
		assert.False(t, foundWithout, "contact without cadence should NOT be in has_cadence IDs")
	})

	t.Run("ListContactIDs_NoCadence", func(t *testing.T) {
		ids, err := repo.ListContactIDs(ctx, repository.ListContactIDsParams{
			CadenceFilter: "no_cadence",
		})
		require.NoError(t, err)

		foundWith, foundWithout := false, false
		for _, id := range ids {
			if id == withCadence.ID {
				foundWith = true
			}
			if id == withoutCadence.ID {
				foundWithout = true
			}
		}
		assert.False(t, foundWith, "contact with cadence should NOT be in no_cadence IDs")
		assert.True(t, foundWithout, "contact without cadence should be in no_cadence IDs")
	})

	// Note: Empty string cadence cannot be tested directly because the
	// contact_cadence_check constraint only allows valid enum values or NULL.
	// The cadence != '' check in SQL is a defensive measure for any legacy data.
}
