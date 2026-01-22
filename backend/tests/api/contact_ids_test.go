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

func setupContactIDsTestRouter() (*gin.Engine, *repository.ContactRepository, func()) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

	// Run migrations before connecting to database
	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
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
	reminderRepo := repository.NewReminderRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)
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
		id1 := createContact("IDs Test Contact Alpha")
		id2 := createContact("IDs Test Contact Beta")
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
		id1 := createContact("IDs Sort Zebra")
		id2 := createContact("IDs Sort Alpha")
		defer deleteContact(id1)
		defer deleteContact(id2)

		// Get IDs sorted by name ascending (limit to our test contacts)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=IDs+Sort&sort=name&order=asc", nil)
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
		uniqueSuffix := fmt.Sprintf("%d", os.Getpid())
		id1 := createContact("Searchable Unique " + uniqueSuffix)
		id2 := createContact("Other Contact " + uniqueSuffix)
		defer deleteContact(id1)
		defer deleteContact(id2)

		// Search for the unique contact
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=Searchable+Unique", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		// Should find the searchable contact
		assert.Contains(t, resp.IDs, id1)
	})

	t.Run("returns empty array when no contacts match search", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?ids_only=true&search=NonexistentContactXYZ123456", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		resp := parseIDsResponse(w.Body.Bytes())

		assert.Equal(t, 0, len(resp.IDs))
		assert.Equal(t, 0, resp.Total)
	})

	t.Run("excludes soft-deleted contacts from IDs", func(t *testing.T) {
		// Create and delete a contact
		id := createContact("Soft Delete IDs Test")
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
}
