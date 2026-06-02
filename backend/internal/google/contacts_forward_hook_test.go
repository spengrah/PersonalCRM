//go:build integration_testdb

package google

import (
	"context"
	"os"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"google.golang.org/api/people/v1"

	"github.com/stretchr/testify/require"
)

// gcontactsForwardEnv bundles the DB-backed deps for the gcontacts
// processContact forward-reconcile tests.
type gcontactsForwardEnv struct {
	provider     *ContactsProvider
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	externalRepo *repository.ExternalContactRepository
}

// setupGcontactsForwardEnv builds a ContactsProvider with real repos +
// identity + reconcile services against the live DB. Skips when
// DATABASE_URL is unset (unit-build / no DB). The oauthService is nil —
// processContact is driven directly with a *people.Person, never through
// Sync, so no Google client is needed.
func setupGcontactsForwardEnv(t *testing.T) *gcontactsForwardEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping DB-backed test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)

	identitySvc := service.NewIdentityService(identityRepo)
	// nil bus/registry → enrichment adds methods but skips publish (the
	// forward-hook assertion is on the method landing, not the rematch
	// event, which the reconcile integration suite covers).
	enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil)
	reconcile := service.NewAddressBookReconcileService(enrichSvc, contactRepo, methodRepo, externalRepo)

	provider := NewContactsProvider(nil, externalRepo, enrichSvc, identitySvc, reconcile)

	return &gcontactsForwardEnv{
		provider:     provider,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		externalRepo: externalRepo,
	}
}

func gcontactsForwardSuffix() string { return uuid.New().String()[:8] }

func gPerson(resourceName, displayName string, emails ...string) *people.Person {
	p := &people.Person{
		ResourceName: resourceName,
		Names:        []*people.Name{{DisplayName: displayName, GivenName: displayName}},
	}
	for _, e := range emails {
		p.EmailAddresses = append(p.EmailAddresses, &people.EmailAddress{Value: e})
	}
	return p
}

func contactHasNormalizedMethod(t *testing.T, ctx context.Context, methodRepo *repository.ContactMethodRepository, contactID uuid.UUID, normalized string) bool {
	t.Helper()
	methods, err := methodRepo.ListContactMethodsByContact(ctx, contactID)
	require.NoError(t, err)
	for _, m := range methods {
		if m.ValueNormalized == strings.ToLower(normalized) {
			return true
		}
	}
	return false
}

// seedContactWithEmail creates a CRM contact carrying the given email as
// a method so attemptMatch's discovery-mode MatchOrCreate (which searches
// the contact_method table) links the address-book row to it.
func (env *gcontactsForwardEnv) seedContactWithEmail(t *testing.T, ctx context.Context, name, email string) uuid.UUID {
	t.Helper()
	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	_, err = env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
	})
	require.NoError(t, err)
	return contact.ID
}

// Forward hook: processContact for a row that auto-matches an existing
// contact and whose payload carries an additional email re-propagates the
// new email to the contact. Proves the attemptMatch first-match-only
// early return is no longer the end of the story (the leak is closed
// forward).
func TestProcessContact_ForwardReconcile_LinkedRowGainsMethod(t *testing.T) {
	env := setupGcontactsForwardEnv(t)
	ctx := context.Background()
	sfx := gcontactsForwardSuffix()
	accountID := "acct-" + sfx + "@example.com"
	resource := "people/c-fwd-" + sfx
	firstEmail := "fwd-first-" + sfx + "@example.com"
	newEmail := "fwd-new-" + sfx + "@example.com"

	contactID := env.seedContactWithEmail(t, ctx, "Fwd Person "+sfx, firstEmail)

	// First sync: the address-book row matches the contact by firstEmail
	// and links it. processContact upserts + auto-matches + enriches.
	require.NoError(t, env.provider.processContact(ctx, gPerson(resource, "Fwd Person "+sfx, firstEmail), accountID))

	external, err := env.externalRepo.GetBySource(ctx, ContactsSourceName, resource, &accountID)
	require.NoError(t, err)
	require.NotNil(t, external)
	require.NotNil(t, external.CRMContactID, "first sync must auto-match + link the contact")
	require.Equal(t, contactID, *external.CRMContactID)

	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, external.ID)
		_ = env.methodRepo.DeleteContactMethodsByContact(ctx, contactID)
		_ = env.contactRepo.HardDeleteContact(ctx, contactID)
	})

	require.False(t, contactHasNormalizedMethod(t, ctx, env.methodRepo, contactID, newEmail),
		"precondition: the new email is not on the contact yet")

	// Second sync: same resource now carries BOTH emails. The forward
	// reconcile must propagate the new one (the old attemptMatch path
	// would early-return and leak it).
	require.NoError(t, env.provider.processContact(ctx, gPerson(resource, "Fwd Person "+sfx, firstEmail, newEmail), accountID))

	require.True(t, contactHasNormalizedMethod(t, ctx, env.methodRepo, contactID, newEmail),
		"forward reconcile must propagate the newly-added address-book email to the linked contact")
}

// Forward hook + duplicate: a NEW row that matches an already-linked
// contact's email is marked a duplicate by checkDuplicates, then its
// unique method must reconcile into the canonical contact — NOT auto-pushed
// via attemptMatch's match path (which a stale-struct read would allow).
func TestProcessContact_ForwardReconcile_DupOfLinkedReconcilesToCanonical(t *testing.T) {
	env := setupGcontactsForwardEnv(t)
	ctx := context.Background()
	sfx := gcontactsForwardSuffix()
	acctA := "acctA-" + sfx + "@example.com"
	acctB := "acctB-" + sfx + "@example.com"
	resourceA := "people/c-canon-" + sfx
	resourceB := "people/c-dup-" + sfx
	sharedEmail := "shared-" + sfx + "@example.com"
	dupUniqueEmail := "dupuniq-" + sfx + "@example.com"

	// Pre-seed a CRM contact carrying the shared email so the address-book
	// rows auto-match it.
	contactID := env.seedContactWithEmail(t, ctx, "Dup Canon "+sfx, sharedEmail)

	// Account A: canonical row, auto-matched + linked to the seeded contact.
	require.NoError(t, env.provider.processContact(ctx, gPerson(resourceA, "Dup Canon "+sfx, sharedEmail), acctA))
	canon, err := env.externalRepo.GetBySource(ctx, ContactsSourceName, resourceA, &acctA)
	require.NoError(t, err)
	require.NotNil(t, canon.CRMContactID)
	require.Equal(t, contactID, *canon.CRMContactID)

	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, canon.ID)
		_ = env.methodRepo.DeleteContactMethodsByContact(ctx, contactID)
		_ = env.contactRepo.HardDeleteContact(ctx, contactID)
	})

	// Account B: a second row sharing the email (→ duplicate of A) plus a
	// unique email. checkDuplicates marks it a dup; the re-read + reconcile
	// must land the unique email on the canonical contact.
	require.NoError(t, env.provider.processContact(ctx, gPerson(resourceB, "Dup Other "+sfx, sharedEmail, dupUniqueEmail), acctB))
	dup, err := env.externalRepo.GetBySource(ctx, ContactsSourceName, resourceB, &acctB)
	require.NoError(t, err)
	require.NotNil(t, dup)
	t.Cleanup(func() { _ = env.externalRepo.Delete(ctx, dup.ID) })
	require.NotNil(t, dup.DuplicateOfID, "second row sharing the email must be marked a duplicate")

	require.True(t, contactHasNormalizedMethod(t, ctx, env.methodRepo, contactID, dupUniqueEmail),
		"the dup's unique email must reconcile into the canonical contact")
}
