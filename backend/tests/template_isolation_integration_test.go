//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemplateIsolation proves that clones are independent snapshots of the
// template, not of each other or of the package clone. It creates a sentinel
// contact in the package clone (the rewritten DATABASE_URL), then obtains a
// second ephemeral clone and asserts the sentinel is absent there. The
// ephemeral clone also has the full schema — proven by the repository query
// succeeding against a known table. No raw SQL: it goes straight to the
// repository layer, matching the existing TestContactRepository_Integration
// convention.
func TestTemplateIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Package clone: create a sentinel contact.
	pkgCfg := config.TestConfig()
	pkgCfg.Database.URL = os.Getenv("DATABASE_URL")
	pkgDB, err := db.NewDatabase(ctx, pkgCfg.Database)
	require.NoError(t, err)
	defer pkgDB.Close()

	pkgRepo := repository.NewContactRepository(pkgDB.Queries)
	sentinel, err := pkgRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Template Isolation Sentinel",
	})
	require.NoError(t, err)
	defer func() { _ = pkgRepo.HardDeleteContact(ctx, sentinel.ID) }()

	// Ephemeral clone: a fresh snapshot of the template.
	ephURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	ephCfg := config.TestConfig()
	ephCfg.Database.URL = ephURL
	ephDB, err := db.NewDatabase(ctx, ephCfg.Database)
	require.NoError(t, err)
	defer ephDB.Close()

	ephRepo := repository.NewContactRepository(ephDB.Queries)

	// The repository query succeeding proves the ephemeral clone has the full
	// schema (the contact table exists).
	_, err = ephRepo.GetContact(ctx, sentinel.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "sentinel from the package clone must be absent in an independent ephemeral clone")

	// And the sentinel must not appear in a listing either.
	contacts, err := ephRepo.ListContacts(ctx, repository.ListContactsParams{Limit: 1000, Offset: 0})
	require.NoError(t, err)
	for _, c := range contacts {
		assert.NotEqual(t, sentinel.ID, c.ID, "ephemeral clone must not see the package clone's sentinel")
	}
}
