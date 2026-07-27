package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
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

// newSuggestionRouter builds a Gin router carrying only the production
// suggestions route surface (the /imports/candidates and /imports/:id
// routes are also registered per RegisterImportRoutes' fixed ordering, but
// with nil handlers — safe because these tests never route to them).
// gin.SetMode is hoisted to TestMain so parallel tests don't race on it.
func newSuggestionRouter(svc *service.SuggestionService) *gin.Engine {
	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterImportRoutes(v1, handlers.ImportRouteDeps{
		Suggestions: handlers.NewSuggestionHandler(svc),
	})
	return router
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

// spec: IMP-018.resolve-confirms-requested-methods
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

// spec: IMP-018.actions-re-check-live
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

// TestSuggestions_Resolve_RequestedSubset_OnlyRequestedConfirmed proves the
// requested-subset half of IMP-018.resolve-confirms-requested-methods: with two live pending methods,
// requesting only one confirms exactly that one (enrichment + pending
// clear) and leaves the other untouched in pending.
//
// spec: IMP-018.resolve-confirms-requested-methods
func TestSuggestions_Resolve_RequestedSubset_OnlyRequestedConfirmed(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	emailX := "resolve-subset-x-" + abSuffix(t) + "@example.com"
	emailY := "resolve-subset-y-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{emailX, emailY})

	res, err := svc.ResolveMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: strings.ToLower(emailX)},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied, "only the requested method is confirmed")

	require.True(t, contactHasMethod(t, ctx, env, contact.ID, emailX), "requested method is added")
	require.False(t, contactHasMethod(t, ctx, env, contact.ID, emailY), "unrequested method must NOT be added")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1, "unrequested method stays pending")
	assert.Equal(t, strings.ToLower(emailY), after.PendingMethodSuggestions[0].Value)
}

// TestSuggestions_Resolve_Unspecified_ConfirmsAllPending proves the
// unspecified-means-all half of IMP-018.resolve-confirms-requested-methods: an empty/nil methods list
// confirms EVERY live pending method (not just one), enriching all of
// them and fully clearing pending.
//
// spec: IMP-018.resolve-confirms-requested-methods
func TestSuggestions_Resolve_Unspecified_ConfirmsAllPending(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	emailX := "resolve-all-x-" + abSuffix(t) + "@example.com"
	emailY := "resolve-all-y-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{emailX, emailY})

	res, err := svc.ResolveMethodSuggestions(ctx, external.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Applied, "unspecified methods must confirm ALL live pending")

	require.True(t, contactHasMethod(t, ctx, env, contact.ID, emailX), "first pending method added")
	require.True(t, contactHasMethod(t, ctx, env, contact.ID, emailY), "second pending method added")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Nil(t, after.PendingMethodSuggestions, "pending fully clears")
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

// TestSuggestions_Dismiss_RecordsStickyAndDropsPending proves the
// value dimension of IMP-018.dismiss-sticky-per-method's sticky-per-(type,value) stickiness: a
// dismissed email is sticky and a second PENDING entry of the SAME type
// but a DIFFERENT value survives.
//
// spec: IMP-018.dismiss-sticky-per-method
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

// spec: IMP-018.actions-re-check-live
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

// TestSuggestions_Dismiss_StickyPerType_SameValueSurvivesDifferentType proves
// the TYPE dimension of IMP-018.dismiss-sticky-per-method's sticky-per-(type,value) rule: an
// email pending entry and a telegram pending entry that happen to share
// the SAME normalized value are independent stickiness keys — dismissing
// the email entry must not sweep up the telegram entry with the same
// value. "sticky" + a per-test namespace token normalizes identically for
// both email (lowercase+trim) and telegram (strip '@'+lowercase) so the
// VALUE half of the key is deliberately held constant while TYPE differs.
//
// spec: IMP-018.dismiss-sticky-per-method
func TestSuggestions_Dismiss_StickyPerType_SameValueSurvivesDifferentType(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	sfx := abSuffix(t)
	sharedValue := "sticky" + sfx

	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Sticky Type " + sfx})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, contact.ID) })

	display := "Sticky Type Ext " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    "telegram-sticky-" + sfx,
		DisplayName: &display,
		Emails:      []repository.EmailEntry{{Value: sharedValue}},
		Metadata:    map[string]any{"username": sharedValue},
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
	require.Equal(t, 1, res.SuggestionsRecorded)
	external, err = env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, external.PendingMethodSuggestions, 2, "precondition: email + telegram pending sharing one value")

	// Dismiss ONLY the email entry.
	dres, err := svc.DismissMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: sharedValue},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, dres.Dismissed)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, after.PendingMethodSuggestions, 1, "the telegram entry with the SAME value must survive an email-only dismiss")
	assert.Equal(t, "telegram", after.PendingMethodSuggestions[0].Type)
	assert.Equal(t, sharedValue, after.PendingMethodSuggestions[0].Value)
	require.Len(t, after.DismissedMethodSuggestions, 1)
	assert.Equal(t, "email", after.DismissedMethodSuggestions[0].Type)
}

// spec: IMP-018.stale-entries-pruned-not-dismissed
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

// TestSuggestions_Dismiss_NoLongerOffered_PrunedNotDismissed proves the
// second prune reason of IMP-018.stale-entries-pruned-not-dismissed: a pending method the source no
// longer offers (the external row's current method set drifted since the
// suggestion was recorded) is pruned from pending WITHOUT being recorded
// as a dismissal.
//
// spec: IMP-018.stale-entries-pruned-not-dismissed
func TestSuggestions_Dismiss_NoLongerOffered_PrunedNotDismissed(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)

	email := "dismiss-gone-source-" + abSuffix(t) + "@example.com"
	_, external := seedImportedWithPending(t, ctx, env, []string{email})

	// Producer resync: the source no longer carries the email at all.
	// Pending/dismissed columns survive a wholesale upsert untouched
	// (TestABReconcile_ProducerUpsertPreservesSuggestionColumns), so the
	// stale pending entry is still there, but the external row's CURRENT
	// method set (BuildMethodsFromExternal) no longer offers it.
	_, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      external.Source,
		SourceID:    external.SourceID,
		DisplayName: external.DisplayName,
	})
	require.NoError(t, err)
	external, err = env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	require.Len(t, external.PendingMethodSuggestions, 1, "precondition: stale pending survives the resync")
	require.Empty(t, external.Emails, "precondition: the source no longer offers the email")

	res, err := svc.DismissMethodSuggestions(ctx, external.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Dismissed, "no-longer-offered entry is pruned, not dismissed")

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Nil(t, after.PendingMethodSuggestions, "no-longer-offered entry is pruned from pending")
	assert.Nil(t, after.DismissedMethodSuggestions, "no-longer-offered entry must not be sticky-dismissed")
}

// spec: IMP-018.actions-re-check-live
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

// TestSuggestionsAPI_Resolve_SoftDeletedContact_Returns410 proves the
// literal HTTP status half of IMP-018.actions-re-check-live: through the real
// production route (not the service call directly), a soft-deleted
// effective contact reports 410 Gone on the wire.
//
// spec: IMP-018.actions-re-check-live
func TestSuggestionsAPI_Resolve_SoftDeletedContact_Returns410(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)
	router := newSuggestionRouter(svc)

	email := "gone-api-resolve-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports/suggestions/"+external.ID.String()+"/methods/resolve", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusGone, w.Code, "soft-deleted effective contact must report 410 Gone on the wire")
}

// TestSuggestionsAPI_Dismiss_SoftDeletedContact_Returns410 is the dismiss
// counterpart: the same soft-deleted-contact-gone check applies to the
// dismiss action route too.
//
// spec: IMP-018.actions-re-check-live
func TestSuggestionsAPI_Dismiss_SoftDeletedContact_Returns410(t *testing.T) {
	t.Parallel()
	env := setupABReconcileEnv(t)
	ctx := context.Background()
	svc := newSuggestionService(env)
	router := newSuggestionRouter(svc)

	email := "gone-api-dismiss-" + abSuffix(t) + "@example.com"
	contact, external := seedImportedWithPending(t, ctx, env, []string{email})
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contact.ID))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports/suggestions/"+external.ID.String()+"/methods/dismiss", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusGone, w.Code, "soft-deleted effective contact must report 410 Gone on the wire")
}
