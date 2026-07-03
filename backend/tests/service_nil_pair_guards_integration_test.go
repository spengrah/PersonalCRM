package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/require"
)

// TestEnrichmentService_NilPairGuards pins the EnrichmentService nil semantics
// preserved by the PR5 setter→constructor-arg conversion: a nil knowledge pair
// still errors on inferred location/birthday, and a nil cadence still errors on
// a cadence-override request — the same "not wired" errors the deleted setters
// guarded, not panics. (crm-admin passes nil cadence in production, so the
// cadence guard is live semantics, not dead code.) The ContactService + half-set
// construction guards are unit-tested in internal/service/wiring_guards_test.go.
func TestEnrichmentService_NilPairGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

	// Seed a contact directly through the repo (no knowledge writer needed for a
	// bare row) with no birthday/location so enrichment infers them below.
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "nil-pair guard subject " + t.Name(),
	})
	require.NoError(t, err)

	t.Run("inferred birthday but nil knowledge pair errors", func(t *testing.T) {
		t.Parallel()
		// nil pair (nil assertSvc + nil cache) ⇒ knowledge stays nil.
		enrichSvc := service.NewEnrichmentService(
			database, contactRepo, methodRepo, enrichmentRepo, nil, nil, nil, nil, nil)
		bday := time.Date(1990, 1, 2, 0, 0, 0, 0, time.UTC)
		external := &repository.ExternalContact{Birthday: &bday}
		_, err := enrichSvc.EnrichContactFromExternal(ctx, contact.ID, external)
		require.Error(t, err)
		require.Contains(t, err.Error(), "knowledge writer not wired")
	})

	t.Run("cadence override but nil cadence errors", func(t *testing.T) {
		t.Parallel()
		// Knowledge wired (so the knowledge guard is skipped), cadence nil (the
		// crm-admin posture) ⇒ a cadence override must error.
		assertSvc, cache := buildKnowledgeDeps(t, database, nil)
		enrichSvc := service.NewEnrichmentService(
			database, contactRepo, methodRepo, enrichmentRepo, nil, nil, nil, assertSvc, cache)
		// External with no birthday/location so only the cadence branch triggers.
		external := &repository.ExternalContact{}
		cadence := "weekly"
		_, err := enrichSvc.EnrichContactFromExternalWithSelections(
			ctx, contact.ID, external, nil, nil, &cadence, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cadence updater not wired")
	})
}
