//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchNS builds a unique, hyphen-free per-subtest FTS token so search terms
// seeded by this test never collide with sibling rows on the shared package
// DB. The list search path is plainto_tsquery full-text (contact.sql
// ListContacts), so a single unique lexeme matches exactly the rows this
// subtest seeded, regardless of other tests' data.
func searchNS(t *testing.T) string {
	return "srch" + uuid.NewString()[:8]
}

// newContactSearchAPITest builds a router carrying the production contact
// route surface (RegisterContactRoutes) against the shared package test DB,
// plus the repositories used to seed contacts/methods directly (bypassing
// the HTTP layer for setup; the assertions still go through the full
// handler -> service -> repository -> SQL read path).
func newContactSearchAPITest(t *testing.T) (*gin.Engine, *repository.ContactRepository, *repository.ContactMethodRepository) {
	t.Helper()
	ctx := context.Background()
	database, _ := newAPISharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contentService := service.NewInteractionContentService(interactionRepo, repository.NewCommsMessageRepository(database.Queries), repository.NewTelegramMessageRepository(database.Queries), repository.NewMessagesMessageRepository(database.Queries), repository.NewMeetingNoteRepository(database.Queries), repository.NewCalendarEventRepository(database.Queries), repository.NewPhoneCallRepository(database.Queries), repository.NewContactRepository(database.Queries))

	// List-only wiring (contact_ids_test.go pattern): nil bus/rematch/followUp
	// plus the light cadence/knowledge deps. This deliberately avoids
	// mustBuildManualHandlerForTest, which starts a live River client on the
	// shared package DB — shared TestOnly clients steal each other's
	// river_job work (see river_isolated_testdb_test.go), and this GET-only
	// suite never exercises a write path that needs the event-bus wiring.
	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil, cadenceUpdater, assertSvc, cache, nil)
	contactHandler := handlers.NewContactHandler(contactService)

	// The interaction handler's manual write path is likewise unwired (nil
	// ManualInteractionHandler); no search subtest touches an interaction
	// write route.
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, nil, contentService)

	noteHandler := handlers.NewNoteHandler(service.NewNoteService(repository.NewNoteRepository(database.Queries), contactRepo))

	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:     contactHandler,
		Interaction: interactionHandler,
		Note:        noteHandler,
	})

	return router, contactRepo, methodRepo
}

// createSearchContact seeds a contact via the real repository and registers
// a HardDeleteContact cleanup (no raw SQL; FK cascade drops its
// contact_method rows).
func createSearchContact(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, fullName string) uuid.UUID {
	t.Helper()
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: fullName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(context.Background(), contact.ID) })
	return contact.ID
}

// createSearchMethod seeds a contact method via the real repository.
func createSearchMethod(t *testing.T, ctx context.Context, methodRepo *repository.ContactMethodRepository, contactID uuid.UUID, methodType repository.ContactMethodType, value string, isPrimary bool) {
	t.Helper()
	_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      string(methodType),
		Value:     value,
		IsPrimary: isPrimary,
	})
	require.NoError(t, err)
}

// doSearchGet drives a GET against the production router and returns the raw
// recorder so callers can assert on status/body/JSON as needed.
func doSearchGet(t *testing.T, router *gin.Engine, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	router.ServeHTTP(w, req)
	return w
}

// searchResultIDs unwraps api.APIResponse and returns the contact ids from
// the list response body, in response order.
func searchResultIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)

	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Success)

	dataIface, ok := resp.Data.([]interface{})
	require.True(t, ok, "expected list response data to be an array")

	ids := make([]string, 0, len(dataIface))
	for _, item := range dataIface {
		row, ok := item.(map[string]interface{})
		require.True(t, ok)
		id, ok := row["id"].(string)
		require.True(t, ok, "expected contact row to carry a string id")
		ids = append(ids, id)
	}
	return ids
}

func searchContainsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func searchPositionOf(ids []string, target string) int {
	for i, id := range ids {
		if id == target {
			return i
		}
	}
	return -1
}

// TestContactSearchAPI_MatchesNameAndMethod proves CON-023's match arm: the
// search term matches full-text against both the full name and all contact
// method values (not just the primary method).
func TestContactSearchAPI_MatchesNameAndMethod(t *testing.T) {
	t.Parallel()
	router, contactRepo, methodRepo := newContactSearchAPITest(t)

	t.Run("NameMatch", func(t *testing.T) {
		// spec: CON-023
		ctx := context.Background()
		tok := searchNS(t)
		id := createSearchContact(t, ctx, contactRepo, "NameHit "+tok)

		w := doSearchGet(t, router, "/api/v1/contacts?search="+tok)
		ids := searchResultIDs(t, w)

		assert.True(t, searchContainsID(ids, id.String()), "a name-matched contact should be in the search results")
	})

	t.Run("MethodValueMatch", func(t *testing.T) {
		// spec: CON-023
		ctx := context.Background()
		tokM := searchNS(t)
		// The name deliberately does NOT contain tokM, and the token-bearing
		// method is a non-primary telegram handle (a primary phone with a
		// non-matching value is seeded first) -- so a match here can only
		// come from the "all contact method values" facet, not the name and
		// not just the primary method. This guards a regression that only
		// searched the primary method.
		id := createSearchContact(t, ctx, contactRepo, "MethodHit "+searchNS(t))
		createSearchMethod(t, ctx, methodRepo, id, repository.ContactMethodPhone, "+15550001234", true)
		createSearchMethod(t, ctx, methodRepo, id, repository.ContactMethodTelegram, tokM, false)

		w := doSearchGet(t, router, "/api/v1/contacts?search="+tokM)
		ids := searchResultIDs(t, w)

		assert.True(t, searchContainsID(ids, id.String()), "a match via a non-primary contact method value should surface the contact")
	})
}

// TestContactSearchAPI_RelevanceAndSortOverride proves CON-023's ordering
// arms: without an explicit sort, results order by relevance; an explicit
// sort overrides relevance ordering. Both contacts are constructed so
// relevance order is the REVERSE of the alphabetical fallback, making the
// proof non-vacuous -- a test that only ever observed full_name ASC could
// not tell "ranked by relevance" from "fell through to the tie-breaker."
func TestContactSearchAPI_RelevanceAndSortOverride(t *testing.T) {
	t.Parallel()
	router, contactRepo, _ := newContactSearchAPITest(t)

	t.Run("DefaultOrder_IsRelevance", func(t *testing.T) {
		// spec: CON-023
		ctx := context.Background()
		tok := searchNS(t)
		// HI: tok appears twice -> higher ts_rank (term frequency); name
		// sorts last alphabetically ("Zzz").
		hiID := createSearchContact(t, ctx, contactRepo, "Zzz "+tok+" "+tok)
		// LO: tok appears once -> lower ts_rank; name sorts first
		// alphabetically ("Aaa").
		loID := createSearchContact(t, ctx, contactRepo, "Aaa "+tok)

		w := doSearchGet(t, router, "/api/v1/contacts?search="+tok)
		ids := searchResultIDs(t, w)

		hiPos := searchPositionOf(ids, hiID.String())
		loPos := searchPositionOf(ids, loID.String())
		require.NotEqual(t, -1, hiPos, "HI contact should be in the search results")
		require.NotEqual(t, -1, loPos, "LO contact should be in the search results")

		assert.Less(t, hiPos, loPos, "without an explicit sort, results should order by relevance: HI (2x term frequency) before LO (1x) -- the reverse of the alphabetical fallback")
	})

	t.Run("ExplicitSort_OverridesRelevance", func(t *testing.T) {
		// spec: CON-023
		ctx := context.Background()
		// A fresh token: this subtest's own matched set, independent of
		// DefaultOrder_IsRelevance's.
		tok := searchNS(t)
		hiID := createSearchContact(t, ctx, contactRepo, "Zzz "+tok+" "+tok)
		loID := createSearchContact(t, ctx, contactRepo, "Aaa "+tok)

		w := doSearchGet(t, router, "/api/v1/contacts?search="+tok+"&sort=name&order=asc")
		ids := searchResultIDs(t, w)

		hiPos := searchPositionOf(ids, hiID.String())
		loPos := searchPositionOf(ids, loID.String())
		require.NotEqual(t, -1, hiPos, "HI contact should be in the search results")
		require.NotEqual(t, -1, loPos, "LO contact should be in the search results")

		assert.Less(t, loPos, hiPos, "an explicit sort=name&order=asc should override relevance: alphabetical LO before HI -- the flip of the default (relevance) order")
	})
}

// TestContactSearchAPI_EmptyResult is a generic robustness check, not a
// CON-023 then-item (it is the trivial complement of "the term is
// matched"), so it deliberately carries no behavior citation.
func TestContactSearchAPI_EmptyResult(t *testing.T) {
	t.Parallel()
	router, contactRepo, _ := newContactSearchAPITest(t)

	t.Run("NoMatch_ReturnsEmptyArray", func(t *testing.T) {
		ctx := context.Background()
		ns := searchNS(t)
		seededID := createSearchContact(t, ctx, contactRepo, "SeededButUnrelated "+ns)

		unmatchedTok := searchNS(t) + "nomatch"
		w := doSearchGet(t, router, "/api/v1/contacts?search="+unmatchedTok)
		require.Equal(t, http.StatusOK, w.Code)

		body := w.Body.String()
		assert.Contains(t, body, `"data":[`, "data must serialize as a JSON array literal, got: %s", body)
		assert.NotContains(t, body, `"data":null`)

		// The unmatched token is a unique lexeme no row anywhere contains, so
		// the FTS-filtered result set must be fully empty -- this emptiness
		// assertion is sibling-data-proof on the shared DB.
		ids := searchResultIDs(t, w)
		assert.Empty(t, ids, "an unmatched search term must return no contacts")
		assert.False(t, searchContainsID(ids, seededID.String()), "an unmatched search term must not return the unrelated seeded contact")
	})
}
