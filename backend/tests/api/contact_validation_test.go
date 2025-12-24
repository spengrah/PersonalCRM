package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func setupContactValidationTestRouter() (*gin.Engine, func()) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	if err != nil {
		panic("Failed to connect to test database: " + err.Error())
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	contactHandler := handlers.NewContactHandler(contactRepo)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.CORSMiddleware())

	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("", contactHandler.ListContacts)
		contacts.GET("/:id", contactHandler.GetContact)
		contacts.PUT("/:id", contactHandler.UpdateContact)
		contacts.DELETE("/:id", contactHandler.DeleteContact)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

func TestContactAPI_ValidationErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	t.Run("CreateContact_MissingRequiredField", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: "", // Required field empty
			Email:    stringPtr("test@example.com"),
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
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("CreateContact_InvalidEmailFormat", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: "Test User",
			Email:    stringPtr("not-an-email"), // Invalid email format
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
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("CreateContact_FullNameTooLong", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: strings.Repeat("a", 256), // Exceeds max 255
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
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("CreateContact_InvalidCadence", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName: "Test User",
			Cadence:  stringPtr("daily"), // Invalid cadence value
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
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("CreateContact_InvalidProfilePhotoURL", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName:     "Test User",
			ProfilePhoto: stringPtr("not-a-url"), // Invalid URL
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
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("CreateContact_MalformedJSON", func(t *testing.T) {
		malformedJSON := []byte(`{"full_name": "Test", invalid json}`)

		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(malformedJSON))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.NotNil(t, response.Error)
	})

	t.Run("CreateContact_AllFieldsMaxLength", func(t *testing.T) {
		requestBody := handlers.CreateContactRequest{
			FullName:     strings.Repeat("a", 255),                         // Max 255
			Email:        stringPtr(strings.Repeat("a", 245) + "@test.com"), // Max 255
			Phone:        stringPtr(strings.Repeat("1", 50)),                // Max 50
			Location:     stringPtr(strings.Repeat("a", 255)),               // Max 255
			HowMet:       stringPtr(strings.Repeat("a", 500)),               // Max 500
			ProfilePhoto: stringPtr("https://example.com/" + strings.Repeat("a", 470) + ".jpg"), // Max 500
		}

		jsonBody, _ := json.Marshal(requestBody)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// This should succeed - all at max valid length
		assert.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)

		// Cleanup
		contactData := response.Data.(map[string]interface{})
		contactID := contactData["id"].(string)
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	})
}

func TestContactAPI_UpdateValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	// Create a test contact first
	createReq := handlers.CreateContactRequest{
		FullName: "Update Test User",
		Email:    stringPtr("updatetest@example.com"),
	}
	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var createResponse api.APIResponse
	json.Unmarshal(w.Body.Bytes(), &createResponse)
	contactData := createResponse.Data.(map[string]interface{})
	contactID := contactData["id"].(string)

	defer func() {
		// Cleanup
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	}()

	t.Run("UpdateContact_InvalidContactID", func(t *testing.T) {
		updateReq := handlers.UpdateContactRequest{
			FullName: "Updated Name",
		}

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/invalid-uuid", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("UpdateContact_InvalidEmail", func(t *testing.T) {
		updateReq := handlers.UpdateContactRequest{
			FullName: "Updated Name",
			Email:    stringPtr("invalid-email"),
		}

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID, bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})
}

func TestContactAPI_QueryValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	t.Run("ListContacts_InvalidPage", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?page=-1", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ListContacts_LimitTooHigh", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?limit=1001", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ListContacts_InvalidSortField", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?sort=invalid_field", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ListContacts_InvalidOrder", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?order=invalid", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ListContacts_ValidQueryParams", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?page=1&limit=20&sort=name&order=asc", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
	})
}

func TestContactAPI_GetContactValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	t.Run("GetContact_InvalidUUID", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/not-a-uuid", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("GetContact_ValidUUID_NotFound", func(t *testing.T) {
		nonExistentID := uuid.New().String()
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+nonExistentID, nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})
}

func TestContactAPI_EmailUniqueness(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	// Create first contact
	email := "uniqueness" + uuid.New().String()[:8] + "@example.com"
	createReq := handlers.CreateContactRequest{
		FullName: "First User",
		Email:    stringPtr(email),
	}

	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var createResponse api.APIResponse
	json.Unmarshal(w.Body.Bytes(), &createResponse)
	contactData := createResponse.Data.(map[string]interface{})
	contactID := contactData["id"].(string)

	defer func() {
		// Cleanup
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	}()

	// Try to create second contact with same email
	t.Run("CreateContact_DuplicateEmail", func(t *testing.T) {
		duplicateReq := handlers.CreateContactRequest{
			FullName: "Second User",
			Email:    stringPtr(email), // Same email
		}

		jsonBody, _ := json.Marshal(duplicateReq)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusConflict, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "CONFLICT", response.Error.Code)
	})
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
