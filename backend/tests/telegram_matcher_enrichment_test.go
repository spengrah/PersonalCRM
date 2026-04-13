package tests

import (
	"context"
	"errors"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
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

// setupMatcherEnrichmentTest builds a real PeerMatcher backed by a real
// IdentityService and EnrichmentService. Tests inject identity links and
// external_contact rows, then call MatchPeer to drive the full flow.
func setupMatcherEnrichmentTest(t *testing.T) (*tgpkg.PeerMatcher, *repository.ContactRepository, *repository.ContactMethodRepository, *repository.ExternalContactRepository, *repository.EnrichmentRepository, *service.IdentityService, *db.Database, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	require.NoError(t, db.RunMigrations(databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	messageRepo := repository.NewTelegramMessageRepository(database.Queries)

	identitySvc := service.NewIdentityService(identityRepo)
	enrichmentSvc := service.NewEnrichmentService(contactRepo, methodRepo, enrichmentRepo)

	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, enrichmentSvc, 100)

	cleanup := func() { database.Close() }

	return matcher, contactRepo, methodRepo, externalRepo, enrichmentRepo, identitySvc, database, cleanup
}

func newTestContact(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, name string) *repository.Contact {
	t.Helper()
	c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	return c
}

func TestMatcherEnrichment_AboveThreshold_UsernameMatch(t *testing.T) {
	matcher, contactRepo, methodRepo, externalRepo, enrichmentRepo, identitySvc, database, cleanup := setupMatcherEnrichmentTest(t)
	defer cleanup()

	ctx := context.Background()
	const peerUserID int64 = 770001
	peerIDStr := "770001"

	contact := newTestContact(t, ctx, contactRepo, "Above Thresh "+uuid.New().String()[:8])
	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalContactsBySourceIDPrefix(ctx, pgtype.Text{String: peerIDStr, Valid: true})
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	// Pre-link the username via cached identity (mimics #272 name match path).
	_, err := identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  "AutoUser",
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	// Pre-seed external_contact (above-threshold path).
	displayName := "Above Thresh External"
	external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    peerIDStr,
		DisplayName: &displayName,
		Metadata:    map[string]any{"username": "@AutoUser"},
	})
	require.NoError(t, err)

	// Snapshot profile fields for "untouched" assertion.
	before, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)

	username := "AutoUser"
	result, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	assert.Equal(t, "telegram", methods[0].Type)
	assert.Equal(t, "AutoUser", methods[0].Value)
	assert.Equal(t, "autouser", methods[0].ValueNormalized)

	// Audit row references the persisted external_contact.
	enrichments, err := enrichmentRepo.ListForContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotEmpty(t, enrichments)
	var found bool
	for _, e := range enrichments {
		if e.Field == "method:telegram:autouser" {
			found = true
			require.NotNil(t, e.ExternalContactID, "persisted external_contact_id should be set")
			assert.Equal(t, external.ID, *e.ExternalContactID)
		}
	}
	assert.True(t, found, "expected enrichment audit row for telegram method")

	after, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, before.FullName, after.FullName)
	assert.Equal(t, before.ProfilePhoto, after.ProfilePhoto)
	assert.Equal(t, before.Birthday, after.Birthday)
	assert.Equal(t, before.Location, after.Location)
	assert.Equal(t, before.Cadence, after.Cadence)
}

func TestMatcherEnrichment_BelowThreshold_NoExternalContact(t *testing.T) {
	matcher, contactRepo, methodRepo, _, enrichmentRepo, identitySvc, database, cleanup := setupMatcherEnrichmentTest(t)
	defer cleanup()

	ctx := context.Background()
	const peerUserID int64 = 770002
	peerIDStr := "770002"

	contact := newTestContact(t, ctx, contactRepo, "Below Thresh "+uuid.New().String()[:8])
	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	// Pre-link the username; do NOT seed an external_contact row.
	_, err := identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  "BelowUser",
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	username := "BelowUser"
	result, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	assert.Equal(t, "telegram", methods[0].Type)
	assert.Equal(t, "BelowUser", methods[0].Value)

	// Audit row's external_contact_id must be NULL (synthesized — no persisted row).
	enrichments, err := enrichmentRepo.ListForContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotEmpty(t, enrichments)
	var found bool
	for _, e := range enrichments {
		if e.Field == "method:telegram:belowuser" {
			found = true
			assert.Nil(t, e.ExternalContactID, "synthesized — external_contact_id should be NULL")
		}
	}
	assert.True(t, found)
}

func TestMatcherEnrichment_PhoneMatchWithUsername(t *testing.T) {
	matcher, contactRepo, methodRepo, _, _, _, database, cleanup := setupMatcherEnrichmentTest(t)
	defer cleanup()

	ctx := context.Background()
	const peerUserID int64 = 770003
	peerIDStr := "770003"

	contact := newTestContact(t, ctx, contactRepo, "Phone Match "+uuid.New().String()[:8])
	// Pre-create phone contact_method so identity service finds the contact via phone.
	_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     "+15557770003",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	username := "PhoneMatched"
	phone := "+15557770003"
	result, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, &phone)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	var hasTelegram bool
	for _, m := range methods {
		if m.Type == "telegram" && m.Value == "PhoneMatched" {
			hasTelegram = true
		}
	}
	assert.True(t, hasTelegram, "telegram method should be added from peerUsername after phone match")
}

func TestMatcherEnrichment_PhoneMatchWithoutUsername(t *testing.T) {
	matcher, contactRepo, methodRepo, _, _, _, database, cleanup := setupMatcherEnrichmentTest(t)
	defer cleanup()

	ctx := context.Background()
	const peerUserID int64 = 770004
	peerIDStr := "770004"

	contact := newTestContact(t, ctx, contactRepo, "Phone Only "+uuid.New().String()[:8])
	_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     "+15557770004",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	phone := "+15557770004"
	result, err := matcher.MatchPeer(ctx, peerUserID, nil, nil, nil, &phone)
	require.NoError(t, err)
	require.NotNil(t, result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	for _, m := range methods {
		assert.NotEqual(t, "telegram", m.Type, "no telegram method when peerUsername absent")
	}
}

func TestMatcherEnrichment_Idempotency(t *testing.T) {
	matcher, contactRepo, methodRepo, _, _, identitySvc, database, cleanup := setupMatcherEnrichmentTest(t)
	defer cleanup()

	ctx := context.Background()
	const peerUserID int64 = 770005
	peerIDStr := "770005"

	contact := newTestContact(t, ctx, contactRepo, "Idem "+uuid.New().String()[:8])
	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	_, err := identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  "IdemUser",
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	username := "IdemUser"
	for range 2 {
		_, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
		require.NoError(t, err)
	}

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	count := 0
	for _, m := range methods {
		if m.Type == "telegram" {
			count++
		}
	}
	assert.Equal(t, 1, count, "idempotent — exactly one telegram method after two MatchPeer calls")
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
	require.NoError(t, db.RunMigrations(databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	ctx := context.Background()
	const peerUserID int64 = 770006
	peerIDStr := "770006"

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	identitySvc := service.NewIdentityService(identityRepo)

	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, failingEnricher{}, 100)

	contact := newTestContact(t, ctx, contactRepo, "Faulty Enricher "+uuid.New().String()[:8])
	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySourceID(ctx, pgtype.Text{String: peerIDStr, Valid: true})
	})

	_, err = identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier:  "FaultyUser",
		Type:           identity.IdentifierTypeTelegram,
		Source:         "telegram",
		SourceID:       &peerIDStr,
		KnownContactID: &contact.ID,
	})
	require.NoError(t, err)

	username := "FaultyUser"
	result, err := matcher.MatchPeer(ctx, peerUserID, &username, nil, nil, nil)
	require.NoError(t, err, "enricher errors must NOT fail the match")
	require.NotNil(t, result)
	assert.Equal(t, contact.ID, *result)

	methods, err := methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	require.NoError(t, err)
	for _, m := range methods {
		assert.NotEqual(t, "telegram", m.Type, "no telegram method when enricher fails")
	}

	_ = accelerated.GetCurrentTime()
}
