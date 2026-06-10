package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupContactIDsTestRouter() (*gin.Engine, *repository.ContactRepository, func()) {
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil)
	wireCadenceUpdaterForAPITest(nil, database, contactService)
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
		contacts.DELETE("/:id", contactHandler.DeleteContact)
	}

	cleanup := func() {
		database.Close()
	}

	return router, contactRepo, cleanup
}

// ContactIDsResponse represents the response from ids_only endpoint
type ContactIDsResponse struct {
	IDs   []string `json:"ids"`
	Total int      `json:"total"`
}

func TestContactAPI_ListContactIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, _, cleanup := setupContactIDsTestRouter()
	defer cleanup()

	// Per-test namespace so fixtures and search reads stay scoped to this run
	// under concurrent execution.
	ns := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	// Helper to create a contact
	createContact := func(name string) string {
		body := fmt.Sprintf(`{"full_name": "%s"}`, name)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		var resp api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.True(t, resp.Success)
		contactData := resp.Data.(map[string]interface{})
		return contactData["id"].(string)
	}

	// Helper to delete a contact
	deleteContact := func(id string) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/contacts/"+id, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
	}

	// Helper to parse IDs-only response (wrapped in api.APIResponse)
	parseIDsResponse := func(body []byte) ContactIDsResponse {
		var apiResp api.APIResponse
		err := json.Unmarshal(body, &apiResp)
		require.NoError(t, err)
		require.True(t, apiResp.Success)

		// Convert map to ContactIDsResponse
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

	t.Run("returns IDs only when ids_only=true", func(t *testing.T) {
		// Create test contacts
		id1 := createContact("IDs Test Contact Alpha " + ns)
		id2 := createContact("IDs Test Contact Beta " + ns)
		defer deleteContact(id1)
		defer deleteContact(id2)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		resp := parseIDsResponse(w.Body.Bytes())

		// Should have IDs array and total count
		assert.NotNil(t, resp.IDs)
		assert.GreaterOrEqual(t, len(resp.IDs), 2)
		assert.GreaterOrEqual(t, resp.Total, 2)

		// IDs should be valid UUIDs
		for _, id := range resp.IDs {
			assert.NotEmpty(t, id)
			assert.Len(t, id, 36) // UUID format
		}

		// Our created contacts should be in the list
		assert.Contains(t, resp.IDs, id1)
		assert.Contains(t, resp.IDs, id2)
	})

	t.Run("returns sorted IDs when sort parameter provided", func(t *testing.T) {
		// Create contacts with different names for sorting
		id1 := createContact("IDs Sort Zebra " + ns)
		id2 := createContact("IDs Sort Alpha " + ns)
		defer deleteContact(id1)
		defer deleteContact(id2)

		// Get IDs sorted by name ascending (limit to our test contacts)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=IDs+Sort+"+ns+"&sort=name&order=asc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		respAsc := parseIDsResponse(w.Body.Bytes())

		// Get IDs sorted by name descending (limit to our test contacts)
		req = httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=IDs+Sort&sort=name&order=desc", nil)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		respDesc := parseIDsResponse(w.Body.Bytes())

		// Both should have same total but potentially different order
		assert.Equal(t, respAsc.Total, respDesc.Total)

		// Find positions of our test contacts
		var posAlphaAsc, posZebraAsc, posAlphaDesc, posZebraDesc int
		for i, id := range respAsc.IDs {
			if id == id2 { // Alpha
				posAlphaAsc = i
			}
			if id == id1 { // Zebra
				posZebraAsc = i
			}
		}
		for i, id := range respDesc.IDs {
			if id == id2 { // Alpha
				posAlphaDesc = i
			}
			if id == id1 { // Zebra
				posZebraDesc = i
			}
		}

		// In ascending order, Alpha should come before Zebra
		assert.Less(t, posAlphaAsc, posZebraAsc)
		// In descending order, Zebra should come before Alpha
		assert.Less(t, posZebraDesc, posAlphaDesc)
	})

	t.Run("returns filtered IDs when search parameter provided", func(t *testing.T) {
		// Create contacts with unique names for searching
		id1 := createContact("Searchable Unique " + ns)
		id2 := createContact("Other Contact " + ns)
		defer deleteContact(id1)
		defer deleteContact(id2)

		// Search for the unique contact
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=Searchable+Unique+"+ns, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		// Should find the searchable contact
		assert.Contains(t, resp.IDs, id1)
	})

	t.Run("returns empty array when no contacts match search", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=NonexistentContactXYZ123456"+ns, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		assert.Equal(t, 0, len(resp.IDs))
		assert.Equal(t, 0, resp.Total)
	})

	t.Run("excludes soft-deleted contacts from IDs", func(t *testing.T) {
		// Create and delete a contact
		id := createContact("Soft Delete IDs Test " + ns)
		deleteContact(id)

		// Get IDs
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		// Deleted contact should NOT be in the list
		assert.NotContains(t, resp.IDs, id)
	})

	t.Run("sorts by cadence frequency descending (most frequent first)", func(t *testing.T) {
		// Create contacts with different cadences
		// We need to use the full API to set cadence, so use raw request
		createContactWithCadence := func(name, cadence string) string {
			body := fmt.Sprintf(`{"full_name": "%s", "cadence": "%s"}`, name, cadence)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code)

			var resp api.APIResponse
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			require.NoError(t, err)
			contactData := resp.Data.(map[string]interface{})
			return contactData["id"].(string)
		}

		// Create contacts with specific cadences (using unique prefix for isolation)
		idWeekly := createContactWithCadence("CadSort Weekly Test "+ns, "weekly")
		idMonthly := createContactWithCadence("CadSort Monthly Test "+ns, "monthly")
		idAnnual := createContactWithCadence("CadSort Annual Test "+ns, "annual")
		defer deleteContact(idWeekly)
		defer deleteContact(idMonthly)
		defer deleteContact(idAnnual)

		// Get IDs sorted by cadence descending (most frequent first)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=CadSort+"+ns+"&sort=cadence&order=desc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		// Find positions
		posWeekly, posMonthly, posAnnual := -1, -1, -1
		for i, id := range resp.IDs {
			if id == idWeekly {
				posWeekly = i
			}
			if id == idMonthly {
				posMonthly = i
			}
			if id == idAnnual {
				posAnnual = i
			}
		}

		// Weekly (most frequent) should come before Monthly, which should come before Annual
		assert.NotEqual(t, -1, posWeekly, "Weekly contact should be in results")
		assert.NotEqual(t, -1, posMonthly, "Monthly contact should be in results")
		assert.NotEqual(t, -1, posAnnual, "Annual contact should be in results")
		assert.Less(t, posWeekly, posMonthly, "Weekly should come before Monthly in desc order")
		assert.Less(t, posMonthly, posAnnual, "Monthly should come before Annual in desc order")
	})

	t.Run("sorts by cadence frequency ascending (least frequent first)", func(t *testing.T) {
		createContactWithCadence := func(name, cadence string) string {
			body := fmt.Sprintf(`{"full_name": "%s", "cadence": "%s"}`, name, cadence)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code)

			var resp api.APIResponse
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			require.NoError(t, err)
			contactData := resp.Data.(map[string]interface{})
			return contactData["id"].(string)
		}

		idWeekly := createContactWithCadence("CadAsc Weekly Test "+ns, "weekly")
		idAnnual := createContactWithCadence("CadAsc Annual Test "+ns, "annual")
		defer deleteContact(idWeekly)
		defer deleteContact(idAnnual)

		// Get IDs sorted by cadence ascending (least frequent first)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=CadAsc+"+ns+"&sort=cadence&order=asc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		posWeekly, posAnnual := -1, -1
		for i, id := range resp.IDs {
			if id == idWeekly {
				posWeekly = i
			}
			if id == idAnnual {
				posAnnual = i
			}
		}

		// Annual (least frequent) should come before Weekly in ascending order
		assert.NotEqual(t, -1, posWeekly, "Weekly contact should be in results")
		assert.NotEqual(t, -1, posAnnual, "Annual contact should be in results")
		assert.Less(t, posAnnual, posWeekly, "Annual should come before Weekly in asc order")
	})

	t.Run("null cadence appears after all cadences in descending order", func(t *testing.T) {
		createContactWithCadence := func(name, cadence string) string {
			var body string
			if cadence == "" {
				body = fmt.Sprintf(`{"full_name": "%s"}`, name)
			} else {
				body = fmt.Sprintf(`{"full_name": "%s", "cadence": "%s"}`, name, cadence)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code)

			var resp api.APIResponse
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			require.NoError(t, err)
			contactData := resp.Data.(map[string]interface{})
			return contactData["id"].(string)
		}

		idWeekly := createContactWithCadence("CadNull Weekly Test "+ns, "weekly")
		idNoCadence := createContactWithCadence("CadNull NoCadence Test "+ns, "")
		defer deleteContact(idWeekly)
		defer deleteContact(idNoCadence)

		// Get IDs sorted by cadence descending
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=CadNull+"+ns+"&sort=cadence&order=desc", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		posWeekly, posNoCadence := -1, -1
		for i, id := range resp.IDs {
			if id == idWeekly {
				posWeekly = i
			}
			if id == idNoCadence {
				posNoCadence = i
			}
		}

		// Weekly should come before null cadence in descending order
		assert.NotEqual(t, -1, posWeekly, "Weekly contact should be in results")
		assert.NotEqual(t, -1, posNoCadence, "No-cadence contact should be in results")
		assert.Less(t, posWeekly, posNoCadence, "Weekly should come before null cadence in desc order")
	})
}
