package tests

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// matcherEnrichTestEnv bundles repos + services constructed in the test setup.
type matcherEnrichTestEnv struct {
	matcher        *tgpkg.PeerMatcher
	contactRepo    *repository.ContactRepository
	methodRepo     *repository.ContactMethodRepository
	externalRepo   *repository.ExternalContactRepository
	enrichmentRepo *repository.EnrichmentRepository
	identitySvc    *service.IdentityService
	database       *db.Database
}

// setupMatcherEnrichmentTest builds a real PeerMatcher backed by a real
// IdentityService and EnrichmentService.
//
// DB lifecycle: registers database.Close via t.Cleanup so any per-test
// t.Cleanup the caller adds runs BEFORE the connection is closed (Go runs
// t.Cleanup callbacks in LIFO order — opposite of `defer cleanup()`).
func setupMatcherEnrichmentTest(t *testing.T) *matcherEnrichTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

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
	messageRepo := repository.NewTelegramMessageRepository(database.Queries)

	identitySvc := service.NewIdentityService(identityRepo)
	enrichmentSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo)

	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, enrichmentSvc, 100)

	return &matcherEnrichTestEnv{
		matcher:        matcher,
		contactRepo:    contactRepo,
		methodRepo:     methodRepo,
		externalRepo:   externalRepo,
		enrichmentRepo: enrichmentRepo,
		identitySvc:    identitySvc,
		database:       database,
	}
}

// uniqueTestIDs returns a peer user ID and matching string ID derived from a
// fresh UUID. Each test invocation gets unique values so cross-test or
// cross-run pollution in the shared test DB cannot affect results.
//
// The integer is offset by 9_000_000_000 to keep it well above any
// hand-picked low test IDs elsewhere in the suite. The string form is the
// decimal representation of that int — digit-only, safe for phone-shaped values.
func uniqueTestIDs(t *testing.T) (int64, string) {
	t.Helper()
	suffix := uuid.New().String()[:8]
	var n int64
	for _, c := range suffix {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int64(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int64(c-'a') + 10
		}
	}
	id := int64(9_000_000_000) + n
	return id, strconv.FormatInt(id, 10)
}

func newTestContact(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, name string) *repository.Contact {
	t.Helper()
	c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	return c
}

// registerCleanupBySource removes any external_contact, external_identity, and
// contact rows tied to the given peer ID string after the test runs. Hard
// deletes — keeps the shared DB free of orphans. Order matters: identity and
// external_contact rows clear first so the contact's FK references drop before
// the contact itself is deleted.
func registerCleanupBySource(t *testing.T, env *matcherEnrichTestEnv, ctx context.Context, contactID uuid.UUID, peerIDStr string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = env.database.Queries.DeleteExternalContactsBySourceIDPrefix(ctx, pgtype.Text{String: peerIDStr, Valid: true})
		_, _ = env.database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
		_ = env.contactRepo.HardDeleteContact(ctx, contactID)
	})
}

func TestMatcherEnrichment_AboveThreshold_UsernameMatch(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "AboveUser" + peerIDStr

	contact := newTestContact(t, ctx, env.contactRepo, "Above Thresh "+peerIDStr)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	// Pre-link the username via cached identity (mimics #272 name match path).
	_, err := env.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	// Pre-seed external_contact (above-threshold path).
	displayName := "Above Thresh External " + peerIDStr
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    peerIDStr,
		DisplayName: &displayName,
		Metadata:    map[string]any{"username": "@" + username},
	})
	require.NoError(t, err)

	before, err := env.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)

	result, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	assert.Equal(t, "telegram", methods[0].Type)
	assert.Equal(t, username, methods[0].Value)

	enrichments, err := env.enrichmentRepo.ListForContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotEmpty(t, enrichments)
	var found bool
	for _, e := range enrichments {
		if e.Field == "method:telegram:"+identity.Normalize(username, identity.IdentifierTypeTelegram) {
			found = true
			require.NotNil(t, e.ExternalContactID, "persisted external_contact_id should be set")
			assert.Equal(t, external.ID, *e.ExternalContactID)
		}
	}
	assert.True(t, found, "expected enrichment audit row for telegram method")

	after, err := env.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, before.FullName, after.FullName)
	assert.Equal(t, before.ProfilePhoto, after.ProfilePhoto)
	assert.Equal(t, before.Birthday, after.Birthday)
	assert.Equal(t, before.Location, after.Location)
	assert.Equal(t, before.Cadence, after.Cadence)
}

func TestMatcherEnrichment_BelowThreshold_NoExternalContact(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "BelowUser" + peerIDStr

	contact := newTestContact(t, ctx, env.contactRepo, "Below Thresh "+peerIDStr)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	// Pre-link the username; do NOT seed an external_contact row.
	_, err := env.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	result, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	assert.Equal(t, "telegram", methods[0].Type)
	assert.Equal(t, username, methods[0].Value)

	// Audit row's external_contact_id must be NULL (synthesized — no persisted row).
	enrichments, err := env.enrichmentRepo.ListForContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotEmpty(t, enrichments)
	var found bool
	for _, e := range enrichments {
		if e.Field == "method:telegram:"+identity.Normalize(username, identity.IdentifierTypeTelegram) {
			found = true
			assert.Nil(t, e.ExternalContactID, "synthesized — external_contact_id should be NULL")
		}
	}
	assert.True(t, found)
}

func TestMatcherEnrichment_PhoneMatchWithUsername(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "PhoneMatched" + peerIDStr
	// Build a unique 11-digit phone number from peerUserID's last 8 digits.
	phone := "+1555" + peerIDStr[:7]

	contact := newTestContact(t, ctx, env.contactRepo, "Phone Match "+peerIDStr)
	_, err := env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     phone,
	})
	require.NoError(t, err)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	result, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, &phone)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	var hasTelegram bool
	for _, m := range methods {
		if m.Type == "telegram" && m.Value == username {
			hasTelegram = true
		}
	}
	assert.True(t, hasTelegram, "telegram method should be added from peerUsername after phone match")
}

func TestMatcherEnrichment_PhoneMatchWithoutUsername(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	phone := "+1555" + peerIDStr[:7]

	contact := newTestContact(t, ctx, env.contactRepo, "Phone Only "+peerIDStr)
	_, err := env.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     phone,
	})
	require.NoError(t, err)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	result, err := env.matcher.MatchPeer(ctx, peerUserID, nil, nil, nil, &phone)
	require.NoError(t, err)
	require.NotNil(t, result)

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	for _, m := range methods {
		assert.NotEqual(t, "telegram", m.Type, "no telegram method when peerUsername absent")
	}
}

func TestMatcherEnrichment_Idempotency(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "IdemUser" + peerIDStr

	contact := newTestContact(t, ctx, env.contactRepo, "Idem "+peerIDStr)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	_, err := env.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	for range 2 {
		_, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
		require.NoError(t, err)
	}

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	count := 0
	for _, m := range methods {
		if m.Type == "telegram" {
			count++
		}
	}
	assert.Equal(t, 1, count, "idempotent — exactly one telegram method after two MatchPeer calls")
}

// TestMatcherEnrichment_ConcurrentMatch_TreatsUniqueViolationAsSuccess
// drives multiple concurrent MatchPeer calls released by a barrier so the
// CreateContactMethod inserts overlap. Exactly one wins; the rest hit PG
// unique_violation (23505) and must treat it as success — no error returned,
// no duplicate row. Higher concurrency is used to make 23505 path coverage
// deterministic in practice; a single sequential run cannot trigger it.
func TestMatcherEnrichment_ConcurrentMatch_TreatsUniqueViolationAsSuccess(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "RaceUser" + peerIDStr

	contact := newTestContact(t, ctx, env.contactRepo, "Race "+peerIDStr)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	_, err := env.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	// Barrier release — every goroutine waits, then races into MatchPeer
	// simultaneously. With 8 concurrent callers any sequential interleaving
	// is vanishingly unlikely; at least one is guaranteed to hit 23505.
	const concurrency = 8
	start := make(chan struct{})
	errs := make(chan error, concurrency)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "concurrent MatchPeer must not return an error even on 23505 race")
	}

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	count := 0
	for _, m := range methods {
		if m.Type == "telegram" {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one telegram method even after concurrent matches")
}

// TestMatcherEnrichment_AlreadyMatchedExternalContact_RepairsMissingMethod
// covers the original bug: an external_contact whose match_status is already
// 'matched'/'imported' but the bound CRM contact lacks the telegram method.
// markExternalContactMatched early-returns in this case, so enrichment must
// run from MatchPeer regardless of stored match status.
func TestMatcherEnrichment_AlreadyMatchedExternalContact_RepairsMissingMethod(t *testing.T) {
	env := setupMatcherEnrichmentTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "RepairUser" + peerIDStr

	contact := newTestContact(t, ctx, env.contactRepo, "Repair "+peerIDStr)
	registerCleanupBySource(t, env, ctx, contact.ID, peerIDStr)

	// Pre-link the username so the matcher resolves to this contact via cache.
	_, err := env.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	// Pre-seed external_contact already in 'matched' status pointing at the contact —
	// this is the exact prod state of Jack Laing's row. No telegram contact_method.
	displayName := "Repair External " + peerIDStr
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    peerIDStr,
		DisplayName: &displayName,
		Metadata:    map[string]any{"username": "@" + username},
	})
	require.NoError(t, err)
	_, err = env.externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusMatched)
	require.NoError(t, err)

	result, err := env.matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := env.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, methods, 1, "telegram method should be added even though external_contact was already matched")
	assert.Equal(t, "telegram", methods[0].Type)
	assert.Equal(t, username, methods[0].Value)
}

// failingEnricher always errors on SyncMethodsFromExternal — used to verify
// that enrichment failures do NOT fail the match itself.
type failingEnricher struct{}

func (failingEnricher) SyncMethodsFromExternal(_ context.Context, _ uuid.UUID, _ *repository.ExternalContact) error {
	return errors.New("simulated enrichment failure")
}

func TestMatcherEnrichment_EnricherErrorDoesNotBreakMatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Migrations are applied once by TestMain.

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	username := "FaultyUser" + peerIDStr

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	identitySvc := service.NewIdentityService(identityRepo)

	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, failingEnricher{}, 100)

	contact := newTestContact(t, ctx, contactRepo, "Faulty Enricher "+peerIDStr)
	t.Cleanup(func() {
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	})

	_, err = identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  username,
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	result, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err, "enricher errors must NOT fail the match")
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	for _, m := range methods {
		assert.NotEqual(t, "telegram", m.Type, "no telegram method when enricher fails")
	}
}
