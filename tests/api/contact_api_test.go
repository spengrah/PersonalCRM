package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestRouter() (*gin.Engine, *handlers.ContactHandler, func()) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Connect to test database
	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	if err != nil {
		panic("Failed to connect to test database: " + err.Error())
	}

	// Create repositories and handlers
	contactRepo := repository.NewContactRepository(database.Queries)
	contactHandler := handlers.NewContactHandler(contactRepo)

	// Setup router
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.CORSMiddleware())

	// Add routes
	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("", contactHandler.ListContacts)
		contacts.GET("/:id", contactHandler.GetContact)
		contacts.PUT("/:id", contactHandler.UpdateContact)
		contacts.DELETE("/:id", contactHandler.DeleteContact)
		contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
	}

	// Return cleanup function
	cleanup := func() {
		database.Close()
	}

	return router, contactHandler, cleanup
}

func TestContactAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Check if DATABASE_URL is set for integration testing
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, _, cleanup := setupTestRouter()
	defer cleanup()

	t.Run("CreateContact_Success", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: "API Test User",
			Email:    stringPtr("apitest@example.com"),
			Phone:    stringPtr("+1234567890"),
			Location: stringPtr("Test City"),
			Cadence:  stringPtr("monthly"),
		}

		jsonBody, _ := json.Marshal(requestBody)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		assert.NotNil(t, response.Data)

		// Extract contact data
		contactData, ok := response.Data.(map[string]interface{})
		require.True(t, ok)

		assert.Equal(t, "API Test User", contactData["full_name"])
		assert.Equal(t, "apitest@example.com", contactData["email"])
		assert.NotEmpty(t, contactData["id"])

		// Clean up - delete the test contact
		contactID := contactData["id"].(string)
		req, _ = http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("CreateContact_ValidationError", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: "",                         // Empty name should fail validation
			Email:    stringPtr("invalid-email"), // Invalid email
		}

		jsonBody, _ := json.Marshal(requestBody)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.NotNil(t, response.Error)
		assert.Equal(t, api.ErrCodeValidation, response.Error.Code)
	})

	t.Run("GetContact_NotFound", func(t *testing.T) {
		randomID := uuid.New().String()
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+randomID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.NotNil(t, response.Error)
		assert.Equal(t, api.ErrCodeNotFound, response.Error.Code)
	})

	t.Run("GetContact_InvalidID", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/invalid-uuid", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.NotNil(t, response.Error)
		assert.Equal(t, api.ErrCodeValidation, response.Error.Code)
	})
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
