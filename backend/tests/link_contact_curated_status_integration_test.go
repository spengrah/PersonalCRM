package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkCuratedEnv bundles what the LinkContact handler test needs.
type linkCuratedEnv struct {
	database     *db.Database
	contactRepo  *repository.ContactRepository
	externalRepo *repository.ExternalContactRepository
	handler      *handlers.ImportHandler
}

func setupLinkCuratedEnv(t *testing.T) *linkCuratedEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	gin.SetMode(gin.TestMode)

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)

	identitySvc := service.NewIdentityService(identityRepo)
	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)
	matchSvc := service.NewImportMatchService(contactRepo)
	// nil bus/registry → enrichment skips publish (no rematch wiring needed
	// for the status-classification assertion).
	enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil)
	suggestionSvc := service.NewSuggestionService(externalRepo, contactRepo, methodRepo, enrichSvc, matchSvc, database)
	handler := handlers.NewImportHandler(externalRepo, identitySvc, contactSvc, matchSvc, enrichSvc, suggestionSvc)

	return &linkCuratedEnv{
		database:     database,
		contactRepo:  contactRepo,
		externalRepo: externalRepo,
		handler:      handler,
	}
}

// callLink drives the LinkContact handler with the given external id +
// request body and returns the HTTP status.
func (env *linkCuratedEnv) callLink(t *testing.T, externalID string, body handlers.LinkRequest) int {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: externalID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/imports/"+externalID+"/link", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	env.handler.LinkContact(c)
	return w.Code
}

// callImport drives the ImportContact handler with the given external id +
// optional request body and returns the HTTP status.
func (env *linkCuratedEnv) callImport(t *testing.T, externalID string, body *handlers.ImportRequest) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: externalID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/imports/"+externalID+"/import", reader)
	c.Request.Header.Set("Content-Type", "application/json")
	env.handler.ImportContact(c)
	return w.Code
}

// seedUnmatchedExternalSource seeds an unmatched external_contact row for an
// arbitrary source (used to exercise the link-only import guard).
func (env *linkCuratedEnv) seedUnmatchedExternalSource(t *testing.T, ctx context.Context, source string) *repository.ExternalContact {
	t.Helper()
	sfx := abSuffix(t)
	display := "Link Only Ext " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      source,
		SourceID:    source + "-linkonly-" + sfx,
		DisplayName: &display,
		Emails:      []repository.EmailEntry{{Value: "linkonly-" + sfx + "@example.com"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.externalRepo.Delete(ctx, external.ID) })
	return external
}

func (env *linkCuratedEnv) seedUnmatchedExternal(t *testing.T, ctx context.Context) (*repository.Contact, *repository.ExternalContact) {
	t.Helper()
	sfx := abSuffix(t)
	contact, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Link Curated " + sfx})
	require.NoError(t, err)
	display := "Link Curated Ext " + sfx
	external, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "gcontacts-link-" + sfx,
		DisplayName: &display,
		Emails:      []repository.EmailEntry{{Value: "link-" + sfx + "@example.com"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = env.externalRepo.Delete(ctx, external.ID)
		_ = env.contactRepo.HardDeleteContact(ctx, contact.ID)
	})
	return contact, external
}

// Curated link with selected methods → match_status='imported'.
func TestLinkContact_CuratedWithSelections_LandsImported(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	contact, external := env.seedUnmatchedExternal(t, ctx)
	email := external.Emails[0].Value

	code := env.callLink(t, external.ID.String(), handlers.LinkRequest{
		CRMContactID:    contact.ID.String(),
		SelectedMethods: []handlers.SelectedMethodInput{{OriginalValue: email, Type: "email"}},
	})
	require.Equal(t, http.StatusOK, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusImported, after.MatchStatus)
}

// Curated link via cadence (no method selections) → imported.
func TestLinkContact_CuratedWithCadence_LandsImported(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	contact, external := env.seedUnmatchedExternal(t, ctx)
	cadence := "monthly"

	code := env.callLink(t, external.ID.String(), handlers.LinkRequest{
		CRMContactID: contact.ID.String(),
		Cadence:      &cadence,
	})
	require.Equal(t, http.StatusOK, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusImported, after.MatchStatus)
}

// Bare link (no curation signal) → match_status='matched'.
func TestLinkContact_Bare_LandsMatched(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	contact, external := env.seedUnmatchedExternal(t, ctx)

	code := env.callLink(t, external.ID.String(), handlers.LinkRequest{
		CRMContactID: contact.ID.String(),
	})
	require.Equal(t, http.StatusOK, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusMatched, after.MatchStatus)
}

// §4 residual: MethodsCurated=true with empty/nil SelectedMethods (the
// deselect-all case) → match_status='imported', NOT 'matched'. Before the
// methods_curated discriminator, this was indistinguishable from a bare
// link and wrongly classified as 'matched'.
func TestLinkContact_MethodsCuratedDeselectAll_LandsImported(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	contact, external := env.seedUnmatchedExternal(t, ctx)

	code := env.callLink(t, external.ID.String(), handlers.LinkRequest{
		CRMContactID:   contact.ID.String(),
		MethodsCurated: true,
		// SelectedMethods deliberately nil — the user deselected all.
	})
	require.Equal(t, http.StatusOK, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusImported, after.MatchStatus)
}

// Link-only import guard: a gmail_correspondence external row cannot be
// imported as a new contact → 403, and the row stays unmatched.
func TestImportContact_LinkOnlySource_Forbidden(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	external := env.seedUnmatchedExternalSource(t, ctx, "gmail_correspondence")

	code := env.callImport(t, external.ID.String(), nil)
	require.Equal(t, http.StatusForbidden, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusUnmatched, after.MatchStatus, "row must stay unmatched after a rejected import")
}

// Control: a non-link-only source (gcontacts) still imports successfully,
// proving the guard is scoped to link-only sources only.
func TestImportContact_OrdinarySource_Imports(t *testing.T) {
	env := setupLinkCuratedEnv(t)
	ctx := context.Background()
	external := env.seedUnmatchedExternalSource(t, ctx, "gcontacts")
	t.Cleanup(func() {
		// The import creates a CRM contact named after the display_name;
		// clean it up so the shared DB doesn't accumulate.
		after, _ := env.externalRepo.GetByID(ctx, external.ID)
		if after != nil && after.CRMContactID != nil {
			_ = env.contactRepo.HardDeleteContact(ctx, *after.CRMContactID)
		}
	})

	code := env.callImport(t, external.ID.String(), nil)
	require.Equal(t, http.StatusCreated, code)

	after, err := env.externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusImported, after.MatchStatus)
}
