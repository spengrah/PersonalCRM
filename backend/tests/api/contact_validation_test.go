package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

// getMigrationsPath returns the absolute path to the migrations directory
func getMigrationsPath() string {
	// If MIGRATIONS_PATH is set as absolute path, use it
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}

	// Otherwise, compute path relative to this test file
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename)
	return filepath.Join(testDir, "..", "..", "migrations")
}

func setupContactValidationTestRouter() (*gin.Engine, func()) {
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
			Methods: []handlers.ContactMethodRequest{
				{
					Type:  "email_personal",
					Value: "not-an-email",
				},
			},
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
		// Use unique email to avoid conflicts in CI
		uniqueEmail := strings.Repeat("a", 235) + uuid.New().String()[:10] + "@test.com" // Total ~255 chars

		requestBody := handlers.CreateContactRequest{
			FullName: strings.Repeat("a", 255), // Max 255
			Methods: []handlers.ContactMethodRequest{
				{
					Type:  "email_personal",
					Value: uniqueEmail,
				},
				{
					Type:  "phone",
					Value: strings.Repeat("1", 50),
				},
			},
			Location:     stringPtr(strings.Repeat("a", 255)),                                   // Max 255
			HowMet:       stringPtr(strings.Repeat("a", 500)),                                   // Max 500
			ProfilePhoto: stringPtr("https://example.com/" + strings.Repeat("a", 470) + ".jpg"), // Max 500
		}

		jsonBody, _ := json.Marshal(requestBody)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// This should succeed - all at max valid length
		if !assert.Equal(t, http.StatusCreated, w.Code) {
			// Log response body for debugging
			t.Logf("Response body: %s", w.Body.String())
		}

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)

		// Cleanup only if successful
		if response.Success && response.Data != nil {
			contactData := response.Data.(map[string]interface{})
			contactID := contactData["id"].(string)
			deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
			deleteW := httptest.NewRecorder()
			router.ServeHTTP(deleteW, deleteReq)
		}
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
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email_personal",
				Value: "updatetest@example.com",
			},
		},
	}
	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var createResponse api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &createResponse)
	require.NoError(t, err)
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
			Methods: []handlers.ContactMethodRequest{
				{
					Type:  "email_personal",
					Value: "invalid-email",
				},
			},
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

func TestContactAPI_DuplicateMethodTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	// Create contact with a duplicate method type
	createReq := handlers.CreateContactRequest{
		FullName: "First User",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email_personal",
				Value: "dup1@example.com",
			},
			{
				Type:  "email_personal",
				Value: "dup2@example.com",
			},
		},
	}

	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var response api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.False(t, response.Success)
	assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
}

// Helper function to create string pointers
func stringPtr(s string) *string {
	return &s
}
