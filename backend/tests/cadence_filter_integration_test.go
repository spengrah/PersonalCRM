package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

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

	// Run migrations first
	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactRepository(database.Queries)

	// Create test contacts: one with cadence, one without
	weekly := "weekly"
	withCadence, err := repo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "CadenceFilter With",
		Cadence:  &weekly,
	})
	require.NoError(t, err)
	defer func() { _ = repo.HardDeleteContact(ctx, withCadence.ID) }()

	withoutCadence, err := repo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "CadenceFilter Without",
	})
	require.NoError(t, err)
	defer func() { _ = repo.HardDeleteContact(ctx, withoutCadence.ID) }()

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
		totalHas, err := repo.CountContacts(ctx, "has_cadence")
		require.NoError(t, err)

		totalNo, err := repo.CountContacts(ctx, "no_cadence")
		require.NoError(t, err)

		assert.Greater(t, totalHas, int64(0), "should have at least one contact with cadence")
		assert.Greater(t, totalNo, int64(0), "should have at least one contact without cadence")
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
