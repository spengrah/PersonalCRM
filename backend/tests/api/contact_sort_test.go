package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	_ "personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupContactSortTestRouter(t *testing.T) (*gin.Engine, func()) {
	t.Helper()

	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

	// Migrations are applied once by TestMain.
	// MaxConns/MinConns mirror config.TestConfig() (8/1) to cap the per-pool
	// connection ceiling under parallel execution.
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	if err != nil {
		panic("Failed to connect to test database: " + err.Error())
	}

	cfg := &config.Config{River: config.RiverConfig{WorkerConcurrency: 1}}
	manualHandler, contactService := mustBuildManualHandlerForTest(t, ctx, database, cfg)
	contactHandler := handlers.NewContactHandler(contactService)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, manualHandler)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("", contactHandler.ListContacts)
		contacts.PUT("/:id", contactHandler.UpdateContact)
		contacts.DELETE("/:id", contactHandler.DeleteContact)
		contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

// sortTestHelper provides shared test utilities for sort tests
type sortTestHelper struct {
	router *gin.Engine
	t      *testing.T
}

func (h *sortTestHelper) createContact(name string) string {
	body := fmt.Sprintf(`{"full_name": "%s"}`, name)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)

	var resp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(h.t, err)
	contactData := resp.Data.(map[string]interface{})
	return contactData["id"].(string)
}

func (h *sortTestHelper) createContactWithLocation(name, location string) string {
	body := fmt.Sprintf(`{"full_name": "%s", "location": "%s"}`, name, location)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)

	var resp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(h.t, err)
	contactData := resp.Data.(map[string]interface{})
	return contactData["id"].(string)
}

func (h *sortTestHelper) createContactWithBirthday(name, birthday string) string {
	body := fmt.Sprintf(`{"full_name": "%s", "birthday": "%s"}`, name, birthday)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)

	var resp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(h.t, err)
	contactData := resp.Data.(map[string]interface{})
	return contactData["id"].(string)
}

func (h *sortTestHelper) createContactWithCadence(name, cadence string) string {
	body := fmt.Sprintf(`{"full_name": "%s", "cadence": "%s"}`, name, cadence)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)

	var resp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(h.t, err)
	contactData := resp.Data.(map[string]interface{})
	return contactData["id"].(string)
}

// markContacted seeds last_contacted by posting a mutual interaction
// at the given occurred_at. Replaces the legacy PATCH
// /contacts/:id/last-contacted endpoint, which is no longer registered.
func (h *sortTestHelper) markContacted(id, date string) {
	// Date is YYYY-MM-DD; convert to RFC3339 by appending T00:00:00Z.
	occurredAt := date + "T00:00:00Z"
	body := fmt.Sprintf(`{"direction": "mutual", "occurred_at": "%s"}`, occurredAt)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/"+id+"/interactions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)
}

// recordInteraction posts a directional interaction so that direction-aware
// timestamp columns (last_response_at, last_outreach_at) can be seeded.
// direction must be one of: "outbound", "inbound", "mutual".
func (h *sortTestHelper) recordInteraction(id, direction, date string) {
	body := fmt.Sprintf(`{"direction": "%s", "occurred_at": "%s"}`, direction, date)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/"+id+"/interactions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusCreated, w.Code)
}

func (h *sortTestHelper) deleteContact(id string) {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/contacts/"+id, nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
}

func (h *sortTestHelper) getIDsResponse(url string) ContactIDsResponse {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusOK, w.Code)

	var apiResp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &apiResp)
	require.NoError(h.t, err)
	require.True(h.t, apiResp.Success)

	data := apiResp.Data.(map[string]interface{})
	var resp ContactIDsResponse
	resp.Total = int(data["total"].(float64))
	if idsInterface, ok := data["ids"].([]interface{}); ok {
		for _, id := range idsInterface {
			resp.IDs = append(resp.IDs, id.(string))
		}
	}
	return resp
}

// getListResponseIDs requests the full contact list (no ids_only) so the
// (Search)ContactsSorted SQL paths are exercised, and returns the resulting
// contact IDs in response order.
func (h *sortTestHelper) getListResponseIDs(url string) []string {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusOK, w.Code)

	var apiResp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &apiResp)
	require.NoError(h.t, err)
	require.True(h.t, apiResp.Success)

	dataIface, ok := apiResp.Data.([]interface{})
	require.True(h.t, ok, "expected list response data to be an array")

	ids := make([]string, 0, len(dataIface))
	for _, item := range dataIface {
		row, ok := item.(map[string]interface{})
		require.True(h.t, ok)
		ids = append(ids, row["id"].(string))
	}
	return ids
}

func (h *sortTestHelper) findPosition(ids []string, target string) int {
	for i, id := range ids {
		if id == target {
			return i
		}
	}
	return -1
}

func TestContactSort_LastContactedNullsLast(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactSortTestRouter(t)
	defer cleanup()

	h := &sortTestHelper{router: router, t: t}

	t.Run("last_contacted descending sorts most recent first", func(t *testing.T) {
		// Both contacts get last_contacted=now on creation, then we set specific dates
		idOld := h.createContact("SortLCD Old Test")
		idRecent := h.createContact("SortLCD Recent Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)

		h.markContacted(idOld, "2024-01-01")
		h.markContacted(idRecent, "2025-06-01")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortLCD&sort=last_contacted&order=desc")

		posRecent := h.findPosition(resp.IDs, idRecent)
		posOld := h.findPosition(resp.IDs, idOld)

		assert.NotEqual(t, -1, posRecent, "Recent should be in results")
		assert.NotEqual(t, -1, posOld, "Old should be in results")
		assert.Less(t, posRecent, posOld, "Recent should come before old in desc order")
	})

	t.Run("last_contacted ascending sorts oldest first", func(t *testing.T) {
		idOld := h.createContact("SortLCA Old Test")
		idRecent := h.createContact("SortLCA Recent Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)

		h.markContacted(idOld, "2024-01-01")
		h.markContacted(idRecent, "2025-06-01")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortLCA&sort=last_contacted&order=asc")

		posOld := h.findPosition(resp.IDs, idOld)
		posRecent := h.findPosition(resp.IDs, idRecent)

		assert.NotEqual(t, -1, posOld, "Old should be in results")
		assert.NotEqual(t, -1, posRecent, "Recent should be in results")
		assert.Less(t, posOld, posRecent, "Old should come before recent in asc order")
	})
}

func TestContactSort_ContactBy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactSortTestRouter(t)
	defer cleanup()

	h := &sortTestHelper{router: router, t: t}

	t.Run("contact_by ascending sorts earliest due date first", func(t *testing.T) {
		// Create contacts with different cadences, then mark contacted on same date
		// Weekly cadence → contact_by = 7 days later (sooner)
		// Annual cadence → contact_by = 365 days later (further out)
		idWeekly := h.createContactWithCadence("SortCB Weekly Test", "weekly")
		idAnnual := h.createContactWithCadence("SortCB Annual Test", "annual")
		defer h.deleteContact(idWeekly)
		defer h.deleteContact(idAnnual)

		// Mark both as contacted on the same date
		h.markContacted(idWeekly, "2025-01-15")
		h.markContacted(idAnnual, "2025-01-15")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortCB&sort=contact_by&order=asc")

		posWeekly := h.findPosition(resp.IDs, idWeekly)
		posAnnual := h.findPosition(resp.IDs, idAnnual)

		assert.NotEqual(t, -1, posWeekly, "Weekly should be in results")
		assert.NotEqual(t, -1, posAnnual, "Annual should be in results")
		assert.Less(t, posWeekly, posAnnual, "Weekly (sooner due) should come before Annual in asc order")
	})

	t.Run("contact_by descending sorts latest due date first", func(t *testing.T) {
		idWeekly := h.createContactWithCadence("SortCBD Weekly Test", "weekly")
		idAnnual := h.createContactWithCadence("SortCBD Annual Test", "annual")
		defer h.deleteContact(idWeekly)
		defer h.deleteContact(idAnnual)

		h.markContacted(idWeekly, "2025-01-15")
		h.markContacted(idAnnual, "2025-01-15")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortCBD&sort=contact_by&order=desc")

		posWeekly := h.findPosition(resp.IDs, idWeekly)
		posAnnual := h.findPosition(resp.IDs, idAnnual)

		assert.NotEqual(t, -1, posWeekly, "Weekly should be in results")
		assert.NotEqual(t, -1, posAnnual, "Annual should be in results")
		assert.Less(t, posAnnual, posWeekly, "Annual (later due) should come before Weekly in desc order")
	})

	t.Run("null contact_by appears after all values in ascending order", func(t *testing.T) {
		// Contact with cadence + last_contacted → has contact_by
		idWithCB := h.createContactWithCadence("SortCBN With Test", "monthly")
		// Contact without cadence → no contact_by
		idWithoutCB := h.createContact("SortCBN Without Test")
		defer h.deleteContact(idWithCB)
		defer h.deleteContact(idWithoutCB)

		h.markContacted(idWithCB, "2025-01-15")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortCBN&sort=contact_by&order=asc")

		posWithCB := h.findPosition(resp.IDs, idWithCB)
		posWithoutCB := h.findPosition(resp.IDs, idWithoutCB)

		assert.NotEqual(t, -1, posWithCB, "Contact with contact_by should be in results")
		assert.NotEqual(t, -1, posWithoutCB, "Contact without contact_by should be in results")
		assert.Less(t, posWithCB, posWithoutCB, "Contact with contact_by should come before null in asc order (NULLS LAST)")
	})

	t.Run("null contact_by appears after all values in descending order", func(t *testing.T) {
		idWithCB := h.createContactWithCadence("SortCBND With Test", "monthly")
		idWithoutCB := h.createContact("SortCBND Without Test")
		defer h.deleteContact(idWithCB)
		defer h.deleteContact(idWithoutCB)

		h.markContacted(idWithCB, "2025-01-15")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=SortCBND&sort=contact_by&order=desc")

		posWithCB := h.findPosition(resp.IDs, idWithCB)
		posWithoutCB := h.findPosition(resp.IDs, idWithoutCB)

		assert.NotEqual(t, -1, posWithCB, "Contact with contact_by should be in results")
		assert.NotEqual(t, -1, posWithoutCB, "Contact without contact_by should be in results")
		assert.Less(t, posWithCB, posWithoutCB, "Contact with contact_by should come before null in desc order (NULLS LAST)")
	})

	t.Run("contact_by sort is accepted by API validation", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?sort=contact_by&order=asc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestContactSort_LastResponseAtNullsLast(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactSortTestRouter(t)
	defer cleanup()

	h := &sortTestHelper{router: router, t: t}

	t.Run("last_response_at descending sorts most recent first", func(t *testing.T) {
		// Per-subtest unique prefix so trigram matching from prior runs / sibling
		// subtests doesn't leak across t.Run blocks (shared DB).
		prefix := "SortLRAD"
		idOld := h.createContact(prefix + " Old Test")
		idRecent := h.createContact(prefix + " Recent Test")
		idNull := h.createContact(prefix + " Null Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)
		defer h.deleteContact(idNull)

		// Inbound interaction bumps last_response_at (and last_contacted, but we
		// only assert ordering on last_response_at).
		h.recordInteraction(idOld, "inbound", "2024-01-01")
		h.recordInteraction(idRecent, "inbound", "2025-06-01")
		// idNull intentionally has no interaction → last_response_at IS NULL.

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=last_response_at&order=desc")

		posRecent := h.findPosition(resp.IDs, idRecent)
		posOld := h.findPosition(resp.IDs, idOld)
		posNull := h.findPosition(resp.IDs, idNull)

		assert.NotEqual(t, -1, posRecent, "Recent should be in results")
		assert.NotEqual(t, -1, posOld, "Old should be in results")
		assert.NotEqual(t, -1, posNull, "Null should be in results")
		assert.Less(t, posRecent, posOld, "Recent should come before old in desc order")
		assert.Less(t, posOld, posNull, "Non-null should come before null (NULLS LAST) in desc order")
	})

	t.Run("last_response_at ascending sorts oldest first NULLS LAST", func(t *testing.T) {
		prefix := "SortLRAA"
		idOld := h.createContact(prefix + " Old Test")
		idRecent := h.createContact(prefix + " Recent Test")
		idNull := h.createContact(prefix + " Null Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)
		defer h.deleteContact(idNull)

		h.recordInteraction(idOld, "inbound", "2024-01-01")
		h.recordInteraction(idRecent, "inbound", "2025-06-01")

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=last_response_at&order=asc")

		posOld := h.findPosition(resp.IDs, idOld)
		posRecent := h.findPosition(resp.IDs, idRecent)
		posNull := h.findPosition(resp.IDs, idNull)

		assert.NotEqual(t, -1, posOld, "Old should be in results")
		assert.NotEqual(t, -1, posRecent, "Recent should be in results")
		assert.NotEqual(t, -1, posNull, "Null should be in results")
		assert.Less(t, posOld, posRecent, "Old should come before recent in asc order")
		assert.Less(t, posRecent, posNull, "Non-null should come before null (NULLS LAST) in asc order")
	})

	t.Run("full list path with search desc returns ordered rows", func(t *testing.T) {
		// Exercises SearchContactsSorted (search query + sort, no ids_only).
		prefix := "SortLRAList"
		idOld := h.createContact(prefix + " Old Test")
		idRecent := h.createContact(prefix + " Recent Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)

		h.recordInteraction(idOld, "inbound", "2024-01-01")
		h.recordInteraction(idRecent, "inbound", "2025-06-01")

		ids := h.getListResponseIDs("/api/v1/contacts?search=" + prefix + "&sort=last_response_at&order=desc")

		posRecent := h.findPosition(ids, idRecent)
		posOld := h.findPosition(ids, idOld)
		assert.NotEqual(t, -1, posRecent, "Recent should appear in full list")
		assert.NotEqual(t, -1, posOld, "Old should appear in full list")
		assert.Less(t, posRecent, posOld, "Recent should come before old in desc order")
	})

	t.Run("full list path with search asc puts oldest first", func(t *testing.T) {
		// Same as above but ASC; both directions hit the same SQL CASE pair.
		prefix := "SortLRAListAsc"
		idOld := h.createContact(prefix + " Old Test")
		idRecent := h.createContact(prefix + " Recent Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)

		h.recordInteraction(idOld, "inbound", "2024-01-01")
		h.recordInteraction(idRecent, "inbound", "2025-06-01")

		ids := h.getListResponseIDs("/api/v1/contacts?search=" + prefix + "&sort=last_response_at&order=asc")

		posOld := h.findPosition(ids, idOld)
		posRecent := h.findPosition(ids, idRecent)
		assert.NotEqual(t, -1, posOld, "Old should appear in full list")
		assert.NotEqual(t, -1, posRecent, "Recent should appear in full list")
		assert.Less(t, posOld, posRecent, "Old should come before recent in asc order")
	})

	t.Run("no-search list path returns rows in sorted order", func(t *testing.T) {
		// No search query → routes to ListContactsSorted (full) and
		// ListContactIDsSorted (ids_only). Two contacts with distinct
		// last_response_at values; we assert relative ordering via
		// findPosition rather than absolute row index because the shared
		// test DB carries other contacts with arbitrary dates.
		idOld := h.createContact("SortLRANoSearch Old Test")
		idRecent := h.createContact("SortLRANoSearch Recent Test")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idRecent)

		h.recordInteraction(idOld, "inbound", "2024-01-01")
		h.recordInteraction(idRecent, "inbound", "2025-06-01")

		// Full list path (no search): exercises ListContactsSorted. Use a
		// large limit so both seeded contacts land in the response window.
		listIDs := h.getListResponseIDs("/api/v1/contacts?sort=last_response_at&order=desc&limit=1000")
		posRecent := h.findPosition(listIDs, idRecent)
		posOld := h.findPosition(listIDs, idOld)
		require.NotEqual(t, -1, posRecent, "Recent should be in full list response")
		require.NotEqual(t, -1, posOld, "Old should be in full list response")
		assert.Less(t, posRecent, posOld, "Recent should precede old (desc order, no search)")

		// IDs-only path (no search): exercises ListContactIDsSorted.
		idsResp := h.getIDsResponse("/api/v1/contacts?ids_only=true&sort=last_response_at&order=desc&limit=1000")
		posRecentIDs := h.findPosition(idsResp.IDs, idRecent)
		posOldIDs := h.findPosition(idsResp.IDs, idOld)
		require.NotEqual(t, -1, posRecentIDs, "Recent should be in ids response")
		require.NotEqual(t, -1, posOldIDs, "Old should be in ids response")
		assert.Less(t, posRecentIDs, posOldIDs, "Recent should precede old in ids-only desc order")
	})

	t.Run("last_response_at sort is accepted by API validation", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?sort=last_response_at&order=desc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("legacy last_contacted sort still accepted", func(t *testing.T) {
		// last_contacted remains a valid sort value on the API even though no UI
		// surface emits it; regression-guard for any external caller still using
		// the legacy field.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?sort=last_contacted&order=desc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("unknown sort field rejected with 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?sort=bogus&order=desc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestContactSort_Location(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactSortTestRouter(t)
	defer cleanup()

	h := &sortTestHelper{router: router, t: t}

	t.Run("location ascending sorts alphabetically", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortLocAsc"
		idA := h.createContactWithLocation(prefix+" Alpha Test", "Alphaville")
		idZ := h.createContactWithLocation(prefix+" Zulu Test", "Zurich Town")
		defer h.deleteContact(idA)
		defer h.deleteContact(idZ)

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=location&order=asc")

		posA := h.findPosition(resp.IDs, idA)
		posZ := h.findPosition(resp.IDs, idZ)

		assert.NotEqual(t, -1, posA, "Alphaville contact should be in results")
		assert.NotEqual(t, -1, posZ, "Zurich contact should be in results")
		assert.Less(t, posA, posZ, "Alphaville should come before Zurich Town in asc order")
	})

	t.Run("location descending sorts reverse alphabetically", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortLocDesc"
		idA := h.createContactWithLocation(prefix+" Alpha Test", "Alphaville")
		idZ := h.createContactWithLocation(prefix+" Zulu Test", "Zurich Town")
		defer h.deleteContact(idA)
		defer h.deleteContact(idZ)

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=location&order=desc")

		posA := h.findPosition(resp.IDs, idA)
		posZ := h.findPosition(resp.IDs, idZ)

		assert.NotEqual(t, -1, posA, "Alphaville contact should be in results")
		assert.NotEqual(t, -1, posZ, "Zurich contact should be in results")
		assert.Less(t, posZ, posA, "Zurich Town should come before Alphaville in desc order")
	})

	t.Run("location sort is accepted by API validation and returned in the response body", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortLocShape"
		id := h.createContactWithLocation(prefix+" Shape Test", "Shapeburg")
		defer h.deleteContact(id)

		ids := h.getListResponseIDs("/api/v1/contacts?search=" + prefix + "&sort=location&order=asc")
		require.NotEqual(t, -1, h.findPosition(ids, id), "seeded contact should be in the full list response")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?search="+prefix+"&sort=location&order=asc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var apiResp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apiResp))
		require.True(t, apiResp.Success)

		dataIface, ok := apiResp.Data.([]interface{})
		require.True(t, ok, "expected list response data to be an array")
		require.Len(t, dataIface, 1)
		row := dataIface[0].(map[string]interface{})
		assert.Equal(t, "Shapeburg", row["location"], "response body should carry the seeded location value")
	})
}

func TestContactSort_Birthday(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactSortTestRouter(t)
	defer cleanup()

	h := &sortTestHelper{router: router, t: t}

	t.Run("birthday ascending sorts earliest first", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortBdayAsc"
		idOld := h.createContactWithBirthday(prefix+" Old Test", "1980-01-01")
		idYoung := h.createContactWithBirthday(prefix+" Young Test", "2000-06-01")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idYoung)

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=birthday&order=asc")

		posOld := h.findPosition(resp.IDs, idOld)
		posYoung := h.findPosition(resp.IDs, idYoung)

		assert.NotEqual(t, -1, posOld, "Old birthday contact should be in results")
		assert.NotEqual(t, -1, posYoung, "Young birthday contact should be in results")
		assert.Less(t, posOld, posYoung, "Earlier birthday should come before later birthday in asc order")
	})

	t.Run("birthday descending sorts latest first", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortBdayDesc"
		idOld := h.createContactWithBirthday(prefix+" Old Test", "1980-01-01")
		idYoung := h.createContactWithBirthday(prefix+" Young Test", "2000-06-01")
		defer h.deleteContact(idOld)
		defer h.deleteContact(idYoung)

		resp := h.getIDsResponse("/api/v1/contacts?ids_only=true&search=" + prefix + "&sort=birthday&order=desc")

		posOld := h.findPosition(resp.IDs, idOld)
		posYoung := h.findPosition(resp.IDs, idYoung)

		assert.NotEqual(t, -1, posOld, "Old birthday contact should be in results")
		assert.NotEqual(t, -1, posYoung, "Young birthday contact should be in results")
		assert.Less(t, posYoung, posOld, "Later birthday should come before earlier birthday in desc order")
	})

	t.Run("birthday sort is accepted by API validation and returned in the response body", func(t *testing.T) {
		// spec: CON-018[0]
		prefix := "SortBdayShape"
		id := h.createContactWithBirthday(prefix+" Shape Test", "1990-01-15")
		defer h.deleteContact(id)

		ids := h.getListResponseIDs("/api/v1/contacts?search=" + prefix + "&sort=birthday&order=asc")
		require.NotEqual(t, -1, h.findPosition(ids, id), "seeded contact should be in the full list response")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?search="+prefix+"&sort=birthday&order=asc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var apiResp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apiResp))
		require.True(t, apiResp.Success)

		dataIface, ok := apiResp.Data.([]interface{})
		require.True(t, ok, "expected list response data to be an array")
		require.Len(t, dataIface, 1)
		row := dataIface[0].(map[string]interface{})
		assert.Equal(t, "1990-01-15T00:00:00Z", row["birthday"], "response body should carry the seeded birthday value")
	})
}
