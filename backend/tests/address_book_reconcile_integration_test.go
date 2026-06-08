package tests

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// abReconcileEnv bundles the repos + reconcile service for the
// address-book method reconcile integration tests.
type abReconcileEnv struct {
	database     *db.Database
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	externalRepo *repository.ExternalContactRepository
	eventRepo    *repository.EventRepository
	reconcile    *service.AddressBookReconcileService
	enrich       *service.EnrichmentService
	matchSvc     *service.ImportMatchService
}

// setupABReconcileEnv wires a real reconcile service with a live event
// bus + rematch service (a stub email/phone handler makes those method
// types eligible so the matched-branch auto-propagate publishes
// contact_methods.added). Migrations are applied by TestMain via the
// template clone.
func setupABReconcileEnv(t *testing.T) *abReconcileEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
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
	eventRepo := repository.NewEventRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)
	rematchSvc := service.NewRematchService()
	rematchSvc.Register(stubRematchHandler{idType: "email"})
	rematchSvc.Register(stubRematchHandler{idType: "phone"})
	bus := setupTestEventBusWithRematch(t, context.Background(), database, contactService, rematchSvc)

	enrichmentSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, bus, rematchSvc)
	reconcile := service.NewAddressBookReconcileService(enrichmentSvc, contactRepo, methodRepo, externalRepo)
	matchSvc := service.NewImportMatchService(contactRepo)

	return &abReconcileEnv{
		database:     database,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		externalRepo: externalRepo,
		eventRepo:    eventRepo,
		reconcile:    reconcile,
		enrich:       enrichmentSvc,
		matchSvc:     matchSvc,
	}
}

// stubRematchHandler is a type-only rematch handler so EligibleMethods
// reports email/phone methods as eligible (gating the publish). Its
// Rematch body never runs (the dispatcher worker re-derives from the
// event).
type stubRematchHandler struct {
	idType string
}

func (h stubRematchHandler) IdentifierType() string { return h.idType }
func (h stubRematchHandler) Rematch(_ context.Context, _ uuid.UUID, _ string) (int, error) {
	return 0, nil
}

// abSuffix returns a per-call namespace token so per-subtest names /
// addresses don't collide across runs in the shared DB (trigram
// cross-talk guard). It draws from the synthetic toolkit's syntheticNS
// helper so isolation tokens come from one shared primitive.
func abSuffix(t *testing.T) string {
	t.Helper()
	return syntheticNS(t)
}

// seedLinkedExternal creates a CRM contact + an external_contact row in
// the given source/status linked to it, with the supplied emails. Returns
// the contact and external rows. Registers cleanup.
func seedLinkedExternal(
	t *testing.T,
	ctx context.Context,
	env *abReconcileEnv,
	source string,
	status repository.MatchStatus,
	emails []string,
) (*repository.Contact, *repository.ExternalContact) {
	t.Helper()
	sfx := abSuffix(t)
	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "AB Reconcile " + sfx})
	require.NoError(t, err)

	emailEntries := make([]repository.EmailEntry, 0, len(emails))
	for _, e := range emails {
		emailEntries = append(emailEntries, repository.EmailEntry{Value: e})
	}
	display := "AB External " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      source,
		SourceID:    source + "-" + sfx,
		DisplayName: &display,
		Emails:      emailEntries,
	})
	require.NoError(t, err)

	_, err = env.externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, status)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, external.ID)
		_ = env.contactRepo.HardDeleteContact(ctx, contact.ID)
	})

	// Re-read so the returned external row reflects the match write.
	updated, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	return contact, updated
}

func contactHasMethod(t *testing.T, ctx context.Context, env *abReconcileEnv, contactID uuid.UUID, normalizedValue string) bool {
	t.Helper()
	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contactID)
	require.NoError(t, err)
	for _, m := range methods {
		if m.ValueNormalized == strings.ToLower(normalizedValue) {
			return true
		}
	}
	return false
}

// --- matched -> auto-propagate (+ rematch event) ------------------------

func TestABReconcile_Matched_AutoPropagates(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "matched-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusMatched, []string{email})

	require.False(t, contactHasMethod(t, ctx, env, contact.ID, email), "precondition: contact has no method yet")

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, external.ID))

	require.True(t, contactHasMethod(t, ctx, env, contact.ID, email), "matched row method must auto-propagate")

	// A rematch dispatcher job was enqueued: EnrichContactFromExternal
	// published contact_methods.added (the email type has a registered
	// handler in this harness), which enqueues a rematch_dispatcher job
	// for the contact. This proves the auto path goes through the rematch
	// fan-out, not the event-skipping SyncMethodsFromExternal.
	jobs, err := env.eventRepo.CountRematchDispatcherJobsByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, jobs, int64(1), "matched auto-propagate must enqueue a rematch dispatcher job")
}

// --- imported -> suggestion recorded, no method added -------------------

func TestABReconcile_Imported_RecordsSuggestion(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "imported-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.SuggestionsRecorded)
	assert.Equal(t, 0, res.MethodsAutoApplied)

	require.False(t, contactHasMethod(t, ctx, env, contact.ID, email), "imported row must NOT auto-apply the method")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1)
	assert.Equal(t, "email", after.PendingMethodSuggestions[0].Type)
	assert.Equal(t, strings.ToLower(email), after.PendingMethodSuggestions[0].Value)
}

// --- ignored -> skip entirely ------------------------------------------

func TestABReconcile_Ignored_Skips(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "ignored-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusIgnored, []string{email})

	// ResolveAndReconcile must resolve to a skip (ok=false) for ignored.
	err := env.reconcile.ResolveAndReconcile(ctx, external.ID)
	require.NoError(t, err)

	require.False(t, contactHasMethod(t, ctx, env, contact.ID, email))
	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Nil(t, after.PendingMethodSuggestions, "ignored row must not record suggestions")
}

// --- soft-deleted CRM contact -> skip (no error, no suggestion) --------
// A contact soft-delete leaves external_contact.crm_contact_id pointing at
// the dead contact; the reconcile must skip it rather than fail forever
// (matched) or record suggestions for a dead contact (imported).

func TestABReconcile_SoftDeletedContact_MatchedSkips(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "softdel-matched-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusMatched, []string{email})

	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	// Skips cleanly (no error) — would otherwise error on GetContact.
	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, external.ID))
}

func TestABReconcile_SoftDeletedContact_ImportedSkips(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "softdel-imported-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.SuggestionsRecorded, "must not record suggestions for a soft-deleted contact")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Nil(t, after.PendingMethodSuggestions, "no suggestion written for a soft-deleted contact")
}

// --- dedup: method already on contact is neither re-added nor suggested -

func TestABReconcile_Dedup_ExistingMethodNotResuggested(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "dedup-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	// Contact already has the method.
	_, err := env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
	})
	require.NoError(t, err)

	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.SuggestionsRecorded, "method already present must not be suggested")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Nil(t, after.PendingMethodSuggestions, "empty missing set clears the column to NULL")
}

// --- phone normalization equivalence ------------------------------------

func TestABReconcile_PhoneNormalization_Equivalence(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	sfx := abSuffix(t)
	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "AB Phone " + sfx})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, contact.ID) })

	// Contact already has the phone in bare form.
	_, err = env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     "5551234567",
	})
	require.NoError(t, err)

	// External contact carries the same number in formatted form.
	display := "AB Phone Ext " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "gcontacts-phone-" + sfx,
		DisplayName: &display,
		Phones:      []repository.PhoneEntry{{Value: "+1 (555) 123-4567"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.externalRepo.Delete(ctx, external.ID) })
	_, err = env.externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusImported)
	require.NoError(t, err)
	external, err = env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)

	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.SuggestionsRecorded, "formatted phone must dedup against bare phone via NormalizePhoneE164")
}

// --- dismissed-method skip ---------------------------------------------

func TestABReconcile_DismissedMethod_NotResuggested(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "dismissed-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	// Pre-seed the dismissed set (normalized value).
	_, err := env.externalRepo.SetDismissedMethodSuggestionsForTest(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	external, err = env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)

	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.SuggestionsRecorded, "dismissed method must not be re-suggested")

	_ = contact
}

// --- empty-set clears pending_method_suggestions to NULL ----------------

func TestABReconcile_EmptySet_ClearsPendingToNull(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "clear-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	// First reconcile records the suggestion.
	_, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	mid, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, mid.PendingMethodSuggestions, 1)

	// User adds the method manually; next reconcile recomputes empty.
	_, err = env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
	})
	require.NoError(t, err)

	mid, err = env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	_, err = env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *mid,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Nil(t, after.PendingMethodSuggestions, "recomputed-empty set must clear the column to NULL")
}

// --- producer-upsert-survival: the dedicated columns survive a
// wholesale UpsertExternalContact (which replaces metadata). ------------

func TestABReconcile_ProducerUpsertPreservesSuggestionColumns(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "survive-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{email})

	// Write a pending suggestion + a dismissal.
	_, err := env.externalRepo.SetMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	_, err = env.externalRepo.SetDismissedMethodSuggestionsForTest(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "phone", Value: "+15555550000"},
	})
	require.NoError(t, err)

	// Simulate a producer resync: upsert the same (source, source_id) with
	// fresh metadata. This replaces metadata wholesale but must NOT touch
	// the suggestion columns.
	display := "AB External resynced"
	_, err = env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      external.Source,
		SourceID:    external.SourceID,
		DisplayName: &display,
		Emails:      []repository.EmailEntry{{Value: email}},
		Metadata:    map[string]any{"resynced": true},
	})
	require.NoError(t, err)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1, "pending suggestions must survive producer upsert")
	assert.Equal(t, strings.ToLower(email), after.PendingMethodSuggestions[0].Value)
	require.Len(t, after.DismissedMethodSuggestions, 1, "dismissed suggestions must survive producer upsert")
	assert.Equal(t, "+15555550000", after.DismissedMethodSuggestions[0].Value)
	assert.Equal(t, true, after.Metadata["resynced"], "metadata IS replaced wholesale (control)")

	_ = contact
}

// --- idempotency: second run adds nothing, suggestion stable ------------

func TestABReconcile_Idempotent(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	email := "idem-" + abSuffix(t) + "@example.com"
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusMatched, []string{email})

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, external.ID))
	methodsAfterFirst, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, external.ID))
	methodsAfterSecond, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)

	assert.Equal(t, len(methodsAfterFirst), len(methodsAfterSecond), "second run must add no methods")
}

// seedDupOfCanonical creates a canonical linked row + a duplicate row
// (marked duplicate_of the canonical) carrying a unique method. The dup's
// own crm_contact_id/match_status are set to dupSelfStatus to exercise
// the precedence (a stale dup status must not override the canonical).
func seedDupOfCanonical(
	t *testing.T,
	ctx context.Context,
	env *abReconcileEnv,
	canonStatus repository.MatchStatus,
	dupSelfStatus repository.MatchStatus,
	uniqueEmail string,
) (canonContact *repository.Contact, dupExternal *repository.ExternalContact) {
	t.Helper()
	sfx := abSuffix(t)

	canonContact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "AB Canon " + sfx})
	require.NoError(t, err)

	canonDisplay := "AB Canon Ext " + sfx
	canonExternal, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "gcontacts-canon-" + sfx,
		DisplayName: &canonDisplay,
		Emails:      []repository.EmailEntry{{Value: "canon-" + sfx + "@example.com"}},
	})
	require.NoError(t, err)
	if canonStatus != repository.MatchStatusUnmatched {
		_, err = env.externalRepo.UpdateMatch(ctx, canonExternal.ID, &canonContact.ID, canonStatus)
		require.NoError(t, err)
	}

	dupDisplay := "AB Dup Ext " + sfx
	dupExternal, err = env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "gcontacts-dup-" + sfx,
		DisplayName: &dupDisplay,
		Emails:      []repository.EmailEntry{{Value: uniqueEmail}},
	})
	require.NoError(t, err)
	// Give the dup its own (stale) match state before marking it a dup.
	if dupSelfStatus != repository.MatchStatusUnmatched {
		_, err = env.externalRepo.UpdateMatch(ctx, dupExternal.ID, &canonContact.ID, dupSelfStatus)
		require.NoError(t, err)
	}
	require.NoError(t, env.externalRepo.MarkAsDuplicate(ctx, dupExternal.ID, canonExternal.ID))

	dupExternal, err = env.externalRepo.GetByID(ctx, dupExternal.ID)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, dupExternal.ID)
		_ = env.externalRepo.Delete(ctx, canonExternal.ID)
		_ = env.contactRepo.HardDeleteContact(ctx, canonContact.ID)
	})
	return canonContact, dupExternal
}

// --- dup of matched canonical -> auto-propagates to canonical contact ---

func TestABReconcile_DupOfMatchedCanonical_AutoPropagates(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	uniqueEmail := "dupmatched-" + abSuffix(t) + "@example.com"
	canon, dup := seedDupOfCanonical(t, ctx, env, repository.MatchStatusMatched, repository.MatchStatusMatched, uniqueEmail)

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, dup.ID))

	require.True(t, contactHasMethod(t, ctx, env, canon.ID, uniqueEmail),
		"unique method on a dup of a matched canonical must land on the canonical contact")
}

// --- dup (stale matched) of IMPORTED canonical -> suggestion, not auto --

func TestABReconcile_DupOfImportedCanonical_Suggests(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	uniqueEmail := "dupimported-" + abSuffix(t) + "@example.com"
	canon, dup := seedDupOfCanonical(t, ctx, env, repository.MatchStatusImported, repository.MatchStatusMatched, uniqueEmail)

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, dup.ID))

	require.False(t, contactHasMethod(t, ctx, env, canon.ID, uniqueEmail),
		"dup of an imported canonical must NOT auto-apply (curated dominates)")
	// The suggestion is recorded on the dup row itself.
	after, err := env.externalRepo.GetByID(ctx, dup.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1, "missing method must be recorded as a suggestion")
}

// --- ignored dup of matched canonical -> skipped entirely ---------------

func TestABReconcile_IgnoredDupOfMatchedCanonical_Skips(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	uniqueEmail := "dupignored-" + abSuffix(t) + "@example.com"
	canon, dup := seedDupOfCanonical(t, ctx, env, repository.MatchStatusMatched, repository.MatchStatusIgnored, uniqueEmail)

	require.NoError(t, env.reconcile.ResolveAndReconcile(ctx, dup.ID))

	require.False(t, contactHasMethod(t, ctx, env, canon.ID, uniqueEmail),
		"ignored dominates: no method applied")
	after, err := env.externalRepo.GetByID(ctx, dup.ID)
	require.NoError(t, err)
	require.Nil(t, after.PendingMethodSuggestions, "ignored dominates: no suggestion recorded")
}

// --- catchup: mixed set, summary counts + idempotent re-run -------------

func TestABReconcile_Catchup_MixedSet(t *testing.T) {
	env := setupABReconcileEnv(t)
	ctx := context.Background()

	matchedEmail := "catchup-matched-" + abSuffix(t) + "@example.com"
	matchedContact, _ := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusMatched, []string{matchedEmail})

	importedEmail := "catchup-imported-" + abSuffix(t) + "@example.com"
	_, importedExternal := seedLinkedExternal(t, ctx, env, "icloud_contacts", repository.MatchStatusImported, []string{importedEmail})

	res, err := env.reconcile.ReconcileAllAddressBookMethods(ctx)
	require.NoError(t, err)
	// Shared DB: other rows may exist, so assert per-invariant lower bounds.
	assert.GreaterOrEqual(t, res.Scanned, 2)
	assert.GreaterOrEqual(t, res.MethodsAutoApplied, 1, "matched row's method auto-applied")
	assert.GreaterOrEqual(t, res.SuggestionsRecorded, 1, "imported row's method recorded as suggestion")
	assert.Equal(t, 0, res.Failed)

	require.True(t, contactHasMethod(t, ctx, env, matchedContact.ID, matchedEmail))
	importedAfter, err := env.externalRepo.GetByID(ctx, importedExternal.ID)
	require.NoError(t, err)
	require.Len(t, importedAfter.PendingMethodSuggestions, 1)

	// Idempotent re-run: no new failures.
	res2, err := env.reconcile.ReconcileAllAddressBookMethods(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Failed)
}
