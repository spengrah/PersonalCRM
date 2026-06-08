package tests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
)

// Shared migration helpers for the Element 5 suite migration onto the synthetic
// toolkit. These provide the LIGHTWEIGHT (no-harness, no-River) isolation
// primitives that Transform-F/lite repo tests adopt: a namespaced factory
// generator plus a nil-bus contact seed with a scoped cleanup closure. Transform-R
// tests use the full synthetic.NewHarnessForNamespace + h.SeedContact directly —
// the harness already is the heavy primitive, so there is no helper for that path.

// migrationGenerator returns a factory.Generator scoped to a unique per-test
// namespace plus the namespace token. It is the LIGHTWEIGHT isolation primitive:
// the generator imports only accelerated (no DB, no River client), so a fast repo
// test gets namespace-disjoint identifiers without paying the harness's cost.
//
// Use this for Transform-F/lite tests that keep their own DB setup and only need
// namespaced fixtures. Transform-R tests use synthetic.NewHarnessForNamespace
// instead.
//
// Phone-band caveat: the lightweight path SKIPS the harness's resolveNamespace
// numeric-band collision check. It is SAFE for namespaced name + string
// identifiers (guids/handles/emails), which never cross-match DB-wide. It is NOT
// automatically safe for a fixture that seeds a PHONE identifier matched DB-wide:
// such a test must either use the full harness (which collision-checks the phone
// area band) or assert only on rows it created by exact id, never relying on a
// DB-wide phone lookup.
func migrationGenerator(t *testing.T) (*factory.Generator, string) {
	t.Helper()
	ns := syntheticNS(t)
	return factory.NewGenerator(factory.DefaultSeed, ns), ns
}

// seedMigrationContact writes a factory ContactSpec through a nil-bus
// ContactService.CreateContact — the sanctioned multi-method write path — and
// returns the persisted contact plus a scoped HardDeleteContact cleanup closure.
//
// The nil bus is deliberate: ContactService.CreateContact guards its event
// publish with `s.bus != nil`, so a service built with a nil bus writes the
// contact + all its methods in ONE tx with NO River client and NO event publish.
// That is exactly the lightweight (no-harness) seed path.
//
// The returned cleanup covers the CONTACT only — not FK-child rows on other
// tables. A repo test that writes rows keyed by a different FK (e.g.
// messages_message keyed by mac_host_id) keeps its OWN scoped cleanup for those
// rows and composes it alongside this closure.
func seedMigrationContact(
	ctx context.Context,
	t *testing.T,
	database *db.Database,
	gen *factory.Generator,
	opts ...factory.ContactOption,
) (*repository.Contact, func()) {
	t.Helper()

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)

	// nil bus + nil rematchRegistry → CreateContact writes contact+methods in one
	// tx with no event publish, no River client (the lightweight seed path).
	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)

	spec := gen.Contact(opts...)
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}

	contact, _, err := contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      spec.FullName,
		Cadence:       spec.Cadence,
		LastContacted: spec.LastContacted,
		Birthday:      spec.Birthday,
		Location:      spec.Location,
		HowMet:        spec.HowMet,
	}, methods)
	require.NoError(t, err)

	cleanup := func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }
	return contact, cleanup
}

// LEGACY — staged removal.
//
// randomSuffix returns a 12-char hex string for per-test isolation. It is
// superseded by the namespace token from migrationGenerator and is centralized
// here only so the still-unmigrated callers compile; it is deleted once the last
// caller migrates off it. Do NOT add new callers — use migrationGenerator.
func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
