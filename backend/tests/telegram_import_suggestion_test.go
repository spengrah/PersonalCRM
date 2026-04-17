package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueSuffix returns 8 hex chars followed by "z", so results always end
// in a letter. Appending a letter matters for exact-handle bonus tests:
// the handle normalizer strips trailing digits from the pg_trgm search term,
// which would make the strict-equality form diverge from the CRM full_name's
// strict-equality form. Ending in a letter keeps both sides in sync.
func uniqueSuffix() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:8] + "z"
}

// setupTelegramImportSuggestionTest stands up a real DB, migrates, wires the
// import handler, and returns the router plus repositories used for seeding.
// The cleanup function narrowly deletes only rows this file seeds.
func setupTelegramImportSuggestionTest(t *testing.T) (
	*gin.Engine,
	*repository.ExternalContactRepository,
	*repository.ContactRepository,
	func(),
) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Migrations are applied once by TestMain.

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries))
	matchService := service.NewImportMatchService(contactRepo)
	enrichmentService := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo)

	importHandler := handlers.NewImportHandler(externalRepo, contactService, matchService, enrichmentService)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.CORSMiddleware(config.CORSConfig{AllowAll: true}))

	v1 := router.Group("/api/v1")
	imports := v1.Group("/imports")
	imports.GET("/candidates", importHandler.ListImportCandidates)

	cleanup := func() {
		_, _ = database.Queries.DeleteExternalContactsBySourceIDPrefix(
			context.Background(),
			pgtype.Text{String: "tg-suggestion-", Valid: true},
		)
		database.Close()
	}
	return router, externalRepo, contactRepo, cleanup
}

// listImportCandidates issues GET /api/v1/imports/candidates?source=telegram
// and returns the decoded candidate list.
func listImportCandidates(t *testing.T, router *gin.Engine) []handlers.ImportCandidateResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/candidates?source=telegram&limit=500", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var wrapper struct {
		Data []handlers.ImportCandidateResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapper))
	return wrapper.Data
}

// findCandidateByExternalID picks out the response for a specific external ID.
func findCandidateByExternalID(candidates []handlers.ImportCandidateResponse, id uuid.UUID) *handlers.ImportCandidateResponse {
	target := id.String()
	for i := range candidates {
		if candidates[i].ID == target {
			return &candidates[i]
		}
	}
	return nil
}

func seedTelegramCandidate(t *testing.T, repo *repository.ExternalContactRepository, username string) *repository.ExternalContact {
	t.Helper()
	return seedTelegramCandidateWithName(t, repo, username, nil)
}

func seedTelegramCandidateWithName(
	t *testing.T,
	repo *repository.ExternalContactRepository,
	username string,
	displayName *string,
) *repository.ExternalContact {
	t.Helper()
	sourceID := "tg-suggestion-" + uuid.New().String()
	ext, err := repo.Upsert(context.Background(), repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    sourceID,
		DisplayName: displayName,
		Metadata:    map[string]any{"username": username},
	})
	require.NoError(t, err)
	require.NotNil(t, ext)
	return ext
}

// seedContactWithCleanup creates a CRM contact and registers its hard-delete
// for t.Cleanup. Each sub-test must use unique full_names so that other
// sub-tests (or leftover rows from prior test runs) don't collide and trip
// the collision-gap rule by producing multiple equal-score matches.
func seedContactWithCleanup(t *testing.T, repo *repository.ContactRepository, fullName string) uuid.UUID {
	t.Helper()
	c, err := repo.CreateContact(context.Background(), repository.CreateContactRequest{
		FullName: fullName,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.HardDeleteContact(context.Background(), c.ID)
	})
	return c.ID
}

// TestTelegramImportSuggestion_Integration exercises the end-to-end read path
// for suggested matches on Telegram candidates with real pg_trgm. Asserts:
// exact-handle bonus, min-length gating, separator normalization, numeric
// suffix stripping, two-term search behavior, per-contact dedupe, and
// diacritic folding.
//
// Each sub-test uses unique full_names to avoid pg_trgm returning multiple
// equal-score matches (which would trip the collision-gap rule).
func TestTelegramImportSuggestion_Integration(t *testing.T) {
	router, externalRepo, contactRepo, cleanup := setupTelegramImportSuggestionTest(t)
	defer cleanup()

	t.Run("ExactHandleMatch", func(t *testing.T) {
		sfx := uniqueSuffix()
		id := seedContactWithCleanup(t, contactRepo, "Alice Smith "+sfx)
		ext := seedTelegramCandidate(t, externalRepo, "@alicesmith"+sfx)

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch, "expected suggested_match populated") {
			assert.Equal(t, id.String(), cand.SuggestedMatch.ContactID)
			assert.GreaterOrEqual(t, cand.SuggestedMatch.Confidence, 0.5)
		}
	})

	t.Run("BelowMinLengthHandleOmitted", func(t *testing.T) {
		seedContactWithCleanup(t, contactRepo, "Bobby Unique "+uuid.New().String()[:6])
		ext := seedTelegramCandidate(t, externalRepo, "@bob")

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		assert.Nil(t, cand.SuggestedMatch, "no suggested_match for below-min-length handle")
	})

	t.Run("SeparatorNormalization", func(t *testing.T) {
		sfx := uniqueSuffix()
		id := seedContactWithCleanup(t, contactRepo, "Carol Jones "+sfx)
		ext := seedTelegramCandidate(t, externalRepo, "@carol.jones."+sfx)

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch, "separator normalization should produce a match") {
			assert.Equal(t, id.String(), cand.SuggestedMatch.ContactID)
			assert.GreaterOrEqual(t, cand.SuggestedMatch.Confidence, 0.5)
		}
	})

	t.Run("NumericSuffixStripped", func(t *testing.T) {
		// Handle ends in digits (23) — NormalizeHandleForNameMatch strips
		// them off the search term. The strict-eq bonus compares
		// post-normalization forms, so the contact name must match what the
		// handle collapses to after digit-stripping. Use a letter-terminated
		// base then append digits.
		sfx := uniqueSuffix() // ends in "z"
		id := seedContactWithCleanup(t, contactRepo, "David Miller "+sfx)
		ext := seedTelegramCandidate(t, externalRepo, "@davidmiller"+sfx+"23")

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch, "numeric suffix + bonus should match") {
			assert.Equal(t, id.String(), cand.SuggestedMatch.ContactID)
			assert.GreaterOrEqual(t, cand.SuggestedMatch.Confidence, 0.5)
		}
	})

	t.Run("UsernameBonusBeatsDisplayWhenDistinctContacts", func(t *testing.T) {
		// Display matches "Frank Brown <sfx>" (sim ~1.0 → 0.6 score).
		// Username @edithquinn<sfx2> matches "Edith Quinn <sfx2>" with the
		// exact-handle bonus (sim * 0.6 + 0.4 bonus ≈ 0.89). The username
		// path wins on score.
		sfx1 := uniqueSuffix()
		sfx2 := uniqueSuffix()
		frankName := "Frank Brown " + sfx1
		seedContactWithCleanup(t, contactRepo, frankName)
		edithID := seedContactWithCleanup(t, contactRepo, "Edith Quinn "+sfx2)

		ext := seedTelegramCandidateWithName(t, externalRepo, "@edithquinn"+sfx2, &frankName)

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch) {
			assert.Equal(t, edithID.String(), cand.SuggestedMatch.ContactID,
				"username term + exact-handle bonus should beat display-term score on a different contact")
		}
	})

	t.Run("PerContactDedupeSingleContact", func(t *testing.T) {
		// Display + username both resolve to the same contact. Per-contact
		// dedupe means only the best score counts; no spurious runner-up
		// inside the collision-gap window.
		sfx := uniqueSuffix()
		name := "Grace Hopper " + sfx
		id := seedContactWithCleanup(t, contactRepo, name)
		ext := seedTelegramCandidateWithName(t, externalRepo, "@gracehopper"+sfx, &name)

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch, "same-contact hits from two terms should not trip collision rule") {
			assert.Equal(t, id.String(), cand.SuggestedMatch.ContactID)
			assert.GreaterOrEqual(t, cand.SuggestedMatch.Confidence, 0.5)
		}
	})

	t.Run("DiacriticExactMatch", func(t *testing.T) {
		sfx := uniqueSuffix()
		id := seedContactWithCleanup(t, contactRepo, "José Smith "+sfx)
		ext := seedTelegramCandidate(t, externalRepo, "@jose_smith_"+sfx)

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		if assert.NotNil(t, cand.SuggestedMatch, "diacritic exact-handle bonus should fire end-to-end") {
			assert.Equal(t, id.String(), cand.SuggestedMatch.ContactID)
			assert.GreaterOrEqual(t, cand.SuggestedMatch.Confidence, 0.5)
		}
	})

	t.Run("NonMatchingHandleNoSuggestion", func(t *testing.T) {
		// Nothing seeded matches. Handle should produce no suggestion.
		ext := seedTelegramCandidate(t, externalRepo, "@zzzzzznonexistent")

		cand := findCandidateByExternalID(listImportCandidates(t, router), ext.ID)
		require.NotNil(t, cand)
		assert.Nil(t, cand.SuggestedMatch, "unrelated handle should not produce a suggestion")
	})
}
