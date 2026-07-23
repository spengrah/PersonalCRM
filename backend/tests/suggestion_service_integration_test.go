package tests

import (
	"context"
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSuggestionService builds a SuggestionService from the reconcile env's
// repos + enricher so the resolve path goes through the SAME enrichment +
// rematch publish as production.
func newSuggestionService(env *abReconcileEnv) *service.SuggestionService {
	return service.NewSuggestionService(
		env.externalRepo,
		env.contactRepo,
		env.methodRepo,
		env.enrich,
		env.matchSvc,
		env.database,
	)
}

// seedImportedWithPending seeds a linked imported address-book row carrying
// the given pending methods (recorded via the reconcile path so storage
// matches production). Returns the contact + external row.
func seedImportedWithPending(
	t *testing.T,
	ctx context.Context,
	env *abReconcileEnv,
	emails []string,
) (*repository.Contact, *repository.ExternalContact) {
	t.Helper()
	contact, external := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, emails)
	// recordSuggestions computes the missing set and writes pending.
	res, err := env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *external,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.SuggestionsRecorded)
	updated, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	return contact, updated
}

func TestSuggestions_List_SurfacesLinkedRowWithName(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "list-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})

	list, err := svc.ListSuggestions(ctx, service.SuggestionListParams{Page: 1, Limit: 20}, 10000)
	require.NoError(t, err)

	var found *service.MethodSuggestionItem
	for i := range list.Methods {
		if list.Methods[i].ExternalID == external.ID {
			found = &list.Methods[i]
			break
		}
	}
	require.NotNil(t, found, "linked imported row with pending must surface")
	assert.Equal(t, contact.ID, found.ContactID)
	assert.Equal(t, contact.FullName, found.ContactName)
	require.Len(t, found.Methods, 1)
	assert.Equal(t, "email", found.Methods[0].Type)
	assert.Equal(t, strings.ToLower(email), found.Methods[0].Value)
}

func TestSuggestions_List_DropsSoftDeletedContact(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "softdel-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	list, err := svc.ListSuggestions(ctx, service.SuggestionListParams{Page: 1, Limit: 20}, 10000)
	require.NoError(t, err)
	for _, m := range list.Methods {
		assert.NotEqual(t, external.ID, m.ExternalID, "soft-deleted contact's row must not surface")
	}
}

func TestSuggestions_List_SourceScopeExcludesNonAddressBook(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	// Seed a telegram row with pending JSONB directly (non-address-book
	// source); it must NOT appear in the suggestions list (source guard).
	sfx := abSuffix(t)
	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Tg Suggestion " + sfx})
	require.NoError(t, err)
	display := "Tg External " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    "tg-suggestion-" + sfx,
		DisplayName: &display,
		Emails:      []repository.EmailEntry{{Value: "tg-" + sfx + "@example.com"}},
	})
	require.NoError(t, err)
	_, err = env.externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusImported)
	require.NoError(t, err)
	_, err = env.externalRepo.SetMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: "tg-" + sfx + "@example.com"},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, external.ID)
		_ = env.contactRepo.HardDeleteContact(ctx, contact.ID)
	})

	list, err := svc.ListSuggestions(ctx, service.SuggestionListParams{Page: 1, Limit: 20}, 10000)
	require.NoError(t, err)
	for _, m := range list.Methods {
		assert.NotEqual(t, external.ID, m.ExternalID, "telegram row must be excluded by the address-book source scope")
	}
}

func TestSuggestions_Resolve_AddsMethodAndClearsPending(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "resolve-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})

	require.False(t, contactHasMethod(t, ctx, env, contact.ID, email), "precondition: method not yet on contact")

	res, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Equal(t, contact.ID, res.ContactID)
	assert.NotEqual(t, uuid.Nil, res.RematchJobID, "resolve must return a rematch job id")

	require.True(t, contactHasMethod(t, ctx, env, contact.ID, email), "resolve must add the method")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Nil(t, after.PendingMethodSuggestions, "pending must clear after resolve")

	// KindContactMethodsAdded published → rematch dispatcher job enqueued.
	jobs, err := env.eventRepo.CountRematchDispatcherJobsByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, jobs, int64(1))
}

func TestSuggestions_Resolve_Idempotent(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "resolve-idem-" + abSuffix(t) + "@example.com"
	_, external := seedImportedWithPending(t, ctx, env, []string{email})

	first, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, first.Applied)

	// Second identical resolve: nothing left in pending → Applied 0, no error.
	second, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, second.Applied)
}

func TestSuggestions_Resolve_AlreadyOnContact_NotReAdded(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "resolve-present-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})

	// Add the method via another path so it's already present.
	_, err := env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
	})
	require.NoError(t, err)

	res, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Applied, "already-present method is pruned, not re-applied")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Nil(t, after.PendingMethodSuggestions, "already-present entry is pruned from pending")
}

func TestSuggestions_Resolve_UnknownMethodIsNoOp(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "resolve-unknown-" + abSuffix(t) + "@example.com"
	_, external := seedImportedWithPending(t, ctx, env, []string{email})

	// A (type,value) never in this row's pending → silent no-op, not 400.
	res, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: "never-there@example.com"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Applied)

	// The genuine pending entry is untouched.
	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1)
}

func TestSuggestions_Resolve_MalformedMethod400(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "resolve-malformed-" + abSuffix(t) + "@example.com"
	_, external := seedImportedWithPending(t, ctx, env, []string{email})

	_, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "", Value: "x@example.com"},
	})
	assert.ErrorIs(t, err, service.ErrSuggestionInvalidMethod)
}

// spec: IMP-018[1]
func TestSuggestions_Dismiss_RecordsStickyAndDropsPending(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	emailX := "dismiss-x-" + abSuffix(t) + "@example.com"
	emailY := "dismiss-y-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{emailX, emailY})

	res, err := svc.DismissMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(emailX)},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Dismissed)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1, "only Y remains pending")
	assert.Equal(t, strings.ToLower(emailY), after.PendingMethodSuggestions[0].Value)
	require.Len(t, after.DismissedMethodSuggestions, 1)
	assert.Equal(t, strings.ToLower(emailX), after.DismissedMethodSuggestions[0].Value)

	// Sticky: a fresh reconcile must NOT re-suggest the dismissed X.
	_, err = env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *after,
		EffectiveContactID: contact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)
	reconciled, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	for _, p := range reconciled.PendingMethodSuggestions {
		assert.NotEqual(t, strings.ToLower(emailX), p.Value, "dismissed X must not be re-suggested")
	}
}

func TestSuggestions_Dismiss_Idempotent(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "dismiss-idem-" + abSuffix(t) + "@example.com"
	_, external := seedImportedWithPending(t, ctx, env, []string{email})

	first, err := svc.DismissMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, first.Dismissed)

	second, err := svc.DismissMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(email)},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, second.Dismissed, "duplicate dismiss is a no-op")
}

func TestSuggestions_Dismiss_AlreadyOnContact_NotStickyDismissed(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "dismiss-present-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})

	// Add the method to the contact via another path.
	_, err := env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
	})
	require.NoError(t, err)

	// Whole-card dismiss: an already-applied method is pruned from pending
	// but NOT pushed into dismissed.
	res, err := svc.DismissMethodSuggestions(ctx, external.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Dismissed)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Nil(t, after.PendingMethodSuggestions, "already-applied entry is pruned from pending")
	assert.Nil(t, after.DismissedMethodSuggestions, "already-applied entry must not be sticky-dismissed")
}

func TestSuggestions_Resolve_ContactGone(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "gone-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	_, err := svc.ResolveMethodSuggestions(ctx, external.ID, nil)
	assert.ErrorIs(t, err, service.ErrSuggestionContactGone)
}

func TestSuggestions_DuplicateOfCanonical_ResolvesToCanonicalContact(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	// Canonical: a linked imported gcontacts row.
	canonEmail := "canon-" + abSuffix(t) + "@example.com"
	canonContact, canon := seedLinkedExternal(t, ctx, env, "gcontacts", repository.MatchStatusImported, []string{canonEmail})

	// Duplicate: an icloud row with its OWN crm_contact_id nil, pointing at
	// the canonical via duplicate_of_id, carrying its own pending method.
	dupEmail := "dup-" + abSuffix(t) + "@example.com"
	sfx := abSuffix(t)
	dupDisplay := "Dup External " + sfx
	dup, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "icloud_contacts",
		SourceID:    "icloud-dup-" + sfx,
		DisplayName: &dupDisplay,
		Emails:      []repository.EmailEntry{{Value: dupEmail}},
	})
	require.NoError(t, err)
	require.NoError(t, env.externalRepo.MarkAsDuplicate(ctx, dup.ID, canon.ID))
	t.Cleanup(func() { _ = env.externalRepo.Delete(ctx, dup.ID) })

	// Record pending on the DUP row against the CANONICAL's contact (the
	// reconcile path's behavior for a dup of a linked canonical).
	dupRow, err := env.externalRepo.GetByID(ctx, dup.ID)
	require.NoError(t, err)
	_, err = env.reconcile.ReconcileLinkedExternalContactMethods(ctx, repository.ReconcileTarget{
		ExternalContact:    *dupRow,
		EffectiveContactID: canonContact.ID,
		EffectiveStatus:    repository.MatchStatusImported,
	})
	require.NoError(t, err)

	// The dup row surfaces with the CANONICAL's contact name.
	list, err := svc.ListSuggestions(ctx, service.SuggestionListParams{Page: 1, Limit: 20}, 10000)
	require.NoError(t, err)
	var found *service.MethodSuggestionItem
	for i := range list.Methods {
		if list.Methods[i].ExternalID == dup.ID {
			found = &list.Methods[i]
			break
		}
	}
	require.NotNil(t, found, "dup row with pending must surface")
	assert.Equal(t, canonContact.ID, found.ContactID, "dup resolves to canonical's contact")
	assert.Equal(t, canonContact.FullName, found.ContactName)

	// Resolve on the dup row enriches the CANONICAL's contact.
	res, err := svc.ResolveMethodSuggestions(ctx, dup.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(dupEmail)},
	})
	require.NoError(t, err)
	assert.Equal(t, canonContact.ID, res.ContactID)
	require.True(t, contactHasMethod(t, ctx, env, canonContact.ID, dupEmail), "dup's method enriches the canonical contact")
}
