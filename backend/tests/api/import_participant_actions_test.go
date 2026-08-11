//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gmail_participant candidate's wire-level contract (IMP-046): the
// server declares it import/link/ignore-able (unlike its link-only
// gmail_correspondence sibling), and an import with a user-supplied name
// succeeds even though the candidate itself carries none.
//
// Seeded via the declared-behavior harness (EL3's replay path) rather than a
// raw repository upsert, so these tests exercise the same production
// metadata shape the E2E and unit-test literals pin — a router built on an
// isolated per-test database clone, because the seed starts a live River
// client (see newIsolatedRiverTestDB).

// newParticipantTestRouter builds the production import-route surface
// (ListImportCandidates/GetImportCandidate/ImportContact/LinkContact/
// IgnoreContact + the suggestions endpoint that carries allowed_actions) on
// an isolated database clone, mirroring setupImportTestRouter's wiring.
func newParticipantTestRouter(t *testing.T) (*gin.Engine, *db.Database, context.Context, *repository.ContactRepository) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(
		database, contactRepo, methodRepo, interactionRepo,
		repository.NewContactTaskRepository(database.Queries), nil, nil,
		cadenceUpdater, assertSvc, cache, nil,
	)
	matchService := service.NewImportMatchService(contactRepo)
	enrichmentService := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil,
		cadenceUpdater, assertSvc, cache)
	suggestionService := service.NewSuggestionService(externalRepo, contactRepo, methodRepo, enrichmentService, matchService, database)

	importHandler := handlers.NewImportHandler(externalRepo, nil, contactService, matchService, enrichmentService, suggestionService)
	suggestionHandler := handlers.NewSuggestionHandler(suggestionService)

	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterImportRoutes(v1, handlers.ImportRouteDeps{
		Import:      importHandler,
		Suggestions: suggestionHandler,
	})

	return router, database, ctx, contactRepo
}

// getCandidate reads a candidate back through the production GET endpoint,
// decoded as a generic map so assertions bind to the literal wire keys.
func getCandidate(t *testing.T, router http.Handler, id string) map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/v1/imports/"+id, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var response struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Success)
	return response.Data
}

// spec: IMP-046.allowed-actions-import-link-ignore
//
// The contrast is asserted in one test so a regression that folds
// gmail_participant into linkOnlySources (or drops it from the ordinary
// path) fails loudly against a live sibling, not just an isolated assertion.
func TestParticipantCandidate_AllowedActionsOnWire(t *testing.T) {
	router, database, ctx, _ := newParticipantTestRouter(t)

	participantWorld, err := declare.Run(ctx, database, "IMP-048", declaredAPINS(t), factory.DefaultSeed)
	require.NoError(t, err)
	participantID := participantWorld.Entities["participant"].ID

	correspondenceWorld, err := declare.Run(ctx, database, "IMP-037", declaredAPINS(t), factory.DefaultSeed)
	require.NoError(t, err)
	correspondenceID := correspondenceWorld.Entities["corr"].ID

	items, _ := getSuggestions(t, router, "limit=10000")

	participantIdx := findSuggestionItemIndex(items, "contact", participantID)
	require.GreaterOrEqual(t, participantIdx, 0, "seeded gmail_participant candidate must surface in suggestions")
	participantCand, ok := items[participantIdx]["candidate"].(map[string]interface{})
	require.True(t, ok)
	participantActions, ok := participantCand["allowed_actions"].([]interface{})
	require.True(t, ok, "allowed_actions missing from gmail_participant candidate wire response")
	assert.ElementsMatch(t, []interface{}{"import", "link", "ignore"}, participantActions,
		"gmail_participant must allow import, link, and ignore — it is deliberately absent from linkOnlySources")

	correspondenceIdx := findSuggestionItemIndex(items, "contact", correspondenceID)
	require.GreaterOrEqual(t, correspondenceIdx, 0, "seeded gmail_correspondence candidate must surface in suggestions")
	correspondenceCand, ok := items[correspondenceIdx]["candidate"].(map[string]interface{})
	require.True(t, ok)
	correspondenceActions, ok := correspondenceCand["allowed_actions"].([]interface{})
	require.True(t, ok, "allowed_actions missing from gmail_correspondence candidate wire response")
	assert.ElementsMatch(t, []interface{}{"link", "ignore"}, correspondenceActions,
		"gmail_correspondence stays link-only — the contrast this test pins against a regression adding the new source to the same policy")
}

// spec: IMP-046.import-with-name-succeeds
//
// A nameless gmail_participant candidate (the IMP-047 fixture: an
// address-only, self-anchored sighting) imports successfully once the user
// supplies a name — the link-only rejection never applies to this source.
func TestParticipantCandidate_ImportWithNameSucceeds(t *testing.T) {
	router, database, ctx, contactRepo := newParticipantTestRouter(t)

	ns := declaredAPINS(t)
	world, err := declare.Run(ctx, database, "IMP-047", ns, factory.DefaultSeed)
	require.NoError(t, err)
	candidateID := world.Entities["nameless-participant"].ID

	// The seeded row's metadata, read back over the wire before import
	// consumes it. last_message_at and the self-sender's address are
	// seed-time/generator-owned values (mirroring how the E2E specs read
	// generator-owned values back rather than restate them); every other key,
	// and the complete set of keys present, is asserted as an exact §5.2
	// literal — the other half of the two-sided guard EL2's production-writer
	// literal test (TestParticipant_SelfSenderAnchorsTrust-shaped) pins on the
	// write side.
	before := getCandidate(t, router, candidateID)
	require.Nil(t, before["display_name"], "a nameless sighting must never carry a display_name")
	metadata, ok := before["metadata"].(map[string]interface{})
	require.True(t, ok, "seeded candidate must carry metadata")
	lastMessageAt, ok := metadata["last_message_at"].(string)
	require.True(t, ok && lastMessageAt != "", "seeded candidate must carry last_message_at")
	_, parseErr := time.Parse("2006-01-02T15:04:05Z", lastMessageAt)
	require.NoError(t, parseErr, "last_message_at must match the production UTC format")
	trustedSender, ok := metadata["trusted_sender"].(map[string]interface{})
	require.True(t, ok, "seeded candidate must carry trusted_sender")
	senderAddress, ok := trustedSender["address"].(string)
	require.True(t, ok && senderAddress != "", "trusted_sender must carry an address")

	expectedMetadata := map[string]interface{}{
		"message_count":   float64(1),
		"last_message_at": lastMessageAt,
		"trusted_sender": map[string]interface{}{
			"address": senderAddress,
			"self":    true,
		},
	}
	assert.Equal(t, expectedMetadata, metadata,
		"full §5.2 metadata literal for a nameless self-anchored sighting — every key present, no others")

	name := ns + " Imported By Name"
	payload, err := json.Marshal(map[string]any{"name": name})
	require.NoError(t, err)
	req, _ := http.NewRequest("POST", "/api/v1/imports/"+candidateID+"/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Contact struct {
				ID       string `json:"id"`
				FullName string `json:"full_name"`
			} `json:"contact"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, name, response.Data.Contact.FullName)

	contactID := uuid.MustParse(response.Data.Contact.ID)
	stored, err := contactRepo.GetContact(ctx, contactID)
	require.NoError(t, err)
	assert.Equal(t, name, stored.FullName)

	after := getCandidate(t, router, candidateID)
	assert.Equal(t, "imported", after["match_status"])
}

// No spec citation: a generic validation backstop, not a source-specific
// behavior. Pins the intended interplay with IMP-046 — a gmail_participant
// candidate rejects a nameless import for lack of a name (400), never for
// being link-only (403 is gmail_correspondence's rejection).
func TestParticipantCandidate_ImportWithoutNameRejected(t *testing.T) {
	router, database, ctx, _ := newParticipantTestRouter(t)

	world, err := declare.Run(ctx, database, "IMP-047", declaredAPINS(t), factory.DefaultSeed)
	require.NoError(t, err)
	candidateID := world.Entities["nameless-participant"].ID

	req, _ := http.NewRequest("POST", "/api/v1/imports/"+candidateID+"/import", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var response struct {
		Success bool `json:"success"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	assert.Equal(t, "Cannot import contact without a name", response.Error.Message)
}
