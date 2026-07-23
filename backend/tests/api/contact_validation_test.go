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

// TestMain lives in testmain_integration_test.go (build-tagged) and clones a
// per-package template database via testdb.SetupPackage. The getMigrationsPath
// helper above is shared between the tagged bridge and the migration tests in
// this package, so it stays untagged here.

func setupContactValidationTestRouter() (*gin.Engine, func()) {
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
	cadenceUpdater := buildCadenceUpdaterForAPITest(nil, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(nil, database, nil)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil, cadenceUpdater, assertSvc, cache, nil)
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

	t.Parallel()
	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	t.Run("CreateContact_MissingRequiredField", func(t *testing.T) {
		// spec: CON-002[0], CON-002[5]
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
		// spec: CON-002[5]
		invalidEmails := []string{
			"not-an-email",
			"@domain.com",
			"user@",
		}

		for _, invalidEmail := range invalidEmails {
			requestBody := handlers.CreateContactRequest{
				FullName: "Test User",
				Methods: []handlers.ContactMethodRequest{
					{
						Type:  "email",
						Value: invalidEmail,
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
		}
	})

	t.Run("CreateContact_FullNameTooLong", func(t *testing.T) {
		// spec: CON-002[0], CON-002[5]
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
		// spec: CON-002[4], CON-002[5]
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
		// spec: CON-002[5]
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
		// spec: CON-002[0], CON-002[6]
		// Use unique email to avoid conflicts in CI
		uniqueEmail := strings.Repeat("a", 235) + uuid.New().String()[:10] + "@test.com" // Total ~255 chars

		requestBody := handlers.CreateContactRequest{
			FullName: strings.Repeat("a", 255), // Max 255
			Methods: []handlers.ContactMethodRequest{
				{
					Type:  "email",
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

	t.Parallel()
	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	// Per-test namespace so the CREATE + self-clean DELETE never collide with a
	// leftover/concurrent row on the contact_method email unique.
	ns := uuid.New().String()[:8]

	// Create a test contact first
	createReq := handlers.CreateContactRequest{
		FullName: "Update Test User " + ns,
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "updatetest+" + ns + "@example.com",
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

	// A `methods` key on the update payload is REJECTED, never ignored.
	//
	// Ignoring it is the failure this asserts against: a stale browser or a
	// naive client sends a method addition, Gin drops the unknown key, the
	// client receives 200 and believes the method landed. That is a
	// silent-success failure — the same class the operations endpoint exists to
	// eliminate, merely inverted from silent destruction.
	t.Run("UpdateContact_RejectsLegacyMethodsField", func(t *testing.T) {
		// spec: CON-064[0], CON-064[1], CON-064[2]
		originalName := "Update Test User " + ns

		cases := []struct {
			name    string
			methods any
		}{
			{"populated", []map[string]any{{"type": "email", "value": "legacy+" + ns + "@example.com"}}},
			// An empty array is what a client sends to mean "remove them all" —
			// exactly the destructive intent this endpoint must not accept.
			{"empty array", []map[string]any{}},
			// Load-bearing: this is the case a *json.RawMessage probe cannot
			// distinguish from an absent key, so it is what pins the non-pointer
			// requirement.
			{"null", nil},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{
					"full_name": "Should Not Be Applied",
					"methods":   tc.methods,
				})
				require.NoError(t, err)

				req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID, bytes.NewBuffer(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusBadRequest, w.Code)

				var response api.APIResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				assert.False(t, response.Success)
				assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
				assert.Contains(t, response.Error.Details, "POST /contacts/{id}/methods",
					"the rejection must name the endpoint that does accept methods")

				// Nothing was mutated: a rejected request must not have applied
				// the scalar fields it also carried.
				getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
				getW := httptest.NewRecorder()
				router.ServeHTTP(getW, getReq)
				require.Equal(t, http.StatusOK, getW.Code)

				var getResponse api.APIResponse
				require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &getResponse))
				current := getResponse.Data.(map[string]interface{})
				assert.Equal(t, originalName, current["full_name"])
				methods, _ := current["methods"].([]interface{})
				assert.Len(t, methods, 1, "the contact's methods must be untouched")
			})
		}
	})
}

// The create path still accepts methods, and must: creation is not a
// lost-update path, because there is nothing yet to lose. Only the UPDATE
// request lost its methods field (CON-064), and a DTO change that took the
// create field with it would silently stop persisting methods on every new
// contact while every request still returned 201.
func TestCreateContact_StillAcceptsMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	ns := uuid.New().String()[:8]
	email := "create-methods+" + ns + "@example.com"
	createReq := handlers.CreateContactRequest{
		FullName: "Create Methods User " + ns,
		Methods: []handlers.ContactMethodRequest{
			{Type: "email", Value: email, IsPrimary: true},
		},
	}
	body, err := json.Marshal(createReq)
	require.NoError(t, err)

	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var createResponse api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &createResponse))
	created := createResponse.Data.(map[string]interface{})
	contactID := created["id"].(string)
	defer func() {
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		router.ServeHTTP(httptest.NewRecorder(), deleteReq)
	}()

	// Asserted on the persisted contact, not the create response: a 201 alone
	// would still be returned by a build that accepted the field and dropped it.
	getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
	getW := httptest.NewRecorder()
	router.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)

	var getResponse api.APIResponse
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &getResponse))
	methods, _ := getResponse.Data.(map[string]interface{})["methods"].([]interface{})
	require.Len(t, methods, 1)
	stored := methods[0].(map[string]interface{})
	assert.Equal(t, "email", stored["type"])
	assert.Equal(t, email, stored["value"])
	assert.Equal(t, true, stored["is_primary"])
}

func TestContactAPI_QueryValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
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
		// spec: CON-017[1]
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

	t.Parallel()
	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	t.Run("GetContact_InvalidUUID", func(t *testing.T) {
		// spec: CON-005[2]
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

func TestContactAPI_DuplicateMethodValues(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupContactValidationTestRouter()
	defer cleanup()

	// Create contact with duplicate normalized values for the same type
	createReq := handlers.CreateContactRequest{
		FullName: "First User",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "dup@example.com",
			},
			{
				Type:  "email",
				Value: " Dup@Example.com ",
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
