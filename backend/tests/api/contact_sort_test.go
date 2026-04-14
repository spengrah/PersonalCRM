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
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupContactSortTestRouter() (*gin.Engine, func()) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(context.Background(), databaseURL, migrationsPath); err != nil {
		panic("Failed to run migrations: " + err.Error())
	}

	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	if err != nil {
		panic("Failed to connect to test database: " + err.Error())
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries))
	contactHandler := handlers.NewContactHandler(contactService)

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
		contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
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

func (h *sortTestHelper) markContacted(id, date string) {
	body := fmt.Sprintf(`{"last_contacted": "%s"}`, date)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/contacts/"+id+"/last-contacted", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusOK, w.Code)
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

	router, cleanup := setupContactSortTestRouter()
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

	router, cleanup := setupContactSortTestRouter()
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
