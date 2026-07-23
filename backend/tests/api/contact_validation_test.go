package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		// spec: CON-002[5]
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
		// spec: CON-002[5]
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
		// spec: CON-002[5]
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

	t.Run("CreateContact_FullNameSingleCharAccepted", func(t *testing.T) {
		// spec: CON-002[0]
		ns := uuid.New().String()[:8]
		requestBody := handlers.CreateContactRequest{
			FullName: "A", // Lower bound of the 1-255 range
			Methods: []handlers.ContactMethodRequest{
				{
					Type:  "email",
					Value: "single-char+" + ns + "@example.com",
				},
			},
		}

		jsonBody, _ := json.Marshal(requestBody)
		req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		contactData := response.Data.(map[string]interface{})
		assert.Equal(t, "A", contactData["full_name"])

		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactData["id"].(string), nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	})

	t.Run("CreateContact_LocationTooLong", func(t *testing.T) {
		// spec: CON-002[1]
		requestBody := handlers.CreateContactRequest{
			FullName: "Test User",
			Location: stringPtr(strings.Repeat("a", 256)), // Exceeds max 255
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

	t.Run("CreateContact_HowMetTooLong", func(t *testing.T) {
		// spec: CON-002[1]
		requestBody := handlers.CreateContactRequest{
			FullName: "Test User",
			HowMet:   stringPtr(strings.Repeat("a", 501)), // Exceeds max 500
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

	t.Run("CreateContact_ProfilePhotoTooLong", func(t *testing.T) {
		// spec: CON-002[2]
		// Pin the 500-character cap exactly: a syntactically valid URL of
		// exactly 500 characters is accepted, and one of exactly 501 is
		// rejected, so a validator capped anywhere else fails one side.
		okURL := "https://example.com/" + strings.Repeat("a", 476) + ".jpg"
		require.Len(t, okURL, 500)

		okBody, _ := json.Marshal(handlers.CreateContactRequest{
			FullName:     "Test User PhotoCap",
			ProfilePhoto: stringPtr(okURL),
		})
		okReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(okBody))
		okReq.Header.Set("Content-Type", "application/json")
		okW := httptest.NewRecorder()
		router.ServeHTTP(okW, okReq)
		require.Equal(t, http.StatusCreated, okW.Code, "a 500-char profile_photo URL must be accepted: %s", okW.Body.String())

		var okResp api.APIResponse
		require.NoError(t, json.Unmarshal(okW.Body.Bytes(), &okResp))
		createdID := okResp.Data.(map[string]interface{})["id"].(string)
		defer func() {
			delReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+createdID, nil)
			router.ServeHTTP(httptest.NewRecorder(), delReq)
		}()

		longURL := "https://example.com/" + strings.Repeat("a", 477) + ".jpg"
		require.Len(t, longURL, 501)

		requestBody := handlers.CreateContactRequest{
			FullName:     "Test User",
			ProfilePhoto: stringPtr(longURL),
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

	t.Run("CreateContact_MalformedBirthday", func(t *testing.T) {
		// spec: CON-002[3]
		requestBody := map[string]interface{}{
			"full_name": "Test User",
			"birthday":  "not-a-date",
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

	t.Run("CreateContact_AllCadenceValuesAccepted", func(t *testing.T) {
		// spec: CON-002[4]
		validCadences := []string{"weekly", "biweekly", "monthly", "quarterly", "biannual", "annual"}

		for _, cadence := range validCadences {
			cadence := cadence
			t.Run(cadence, func(t *testing.T) {
				ns := uuid.New().String()[:8]
				requestBody := handlers.CreateContactRequest{
					FullName: "Cadence Test " + ns,
					Cadence:  stringPtr(cadence),
				}

				jsonBody, _ := json.Marshal(requestBody)
				req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
				req.Header.Set("Content-Type", "application/json")

				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

				var response api.APIResponse
				err := json.Unmarshal(w.Body.Bytes(), &response)
				require.NoError(t, err)

				assert.True(t, response.Success)
				contactData := response.Data.(map[string]interface{})
				assert.Equal(t, cadence, contactData["cadence"])

				deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactData["id"].(string), nil)
				deleteW := httptest.NewRecorder()
				router.ServeHTTP(deleteW, deleteReq)
			})
		}
	})

	t.Run("CreateContact_AllFieldsMaxLength", func(t *testing.T) {
		// spec: CON-002[6]
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

	t.Run("UpdateContact_BlankFullNameRejected", func(t *testing.T) {
		// spec: CON-047[0]
		updateReq := handlers.UpdateContactRequest{
			FullName: "",
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

	t.Run("UpdateContact_FullNameTooLongRejected", func(t *testing.T) {
		// spec: CON-047[0]
		updateReq := handlers.UpdateContactRequest{
			FullName: strings.Repeat("a", 256), // Exceeds max 255
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

	t.Run("UpdateContact_InvalidCadenceRejected", func(t *testing.T) {
		// spec: CON-047[0]
		// spec: CON-047[1]
		updateReq := handlers.UpdateContactRequest{
			FullName: "Update Test User " + ns,
			Cadence:  stringPtr("daily"), // Not in the closed set
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
		assert.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("UpdateContact_UnknownContactNotFound", func(t *testing.T) {
		// spec: CON-047[2]
		unknownID := uuid.New().String()
		updateReq := handlers.UpdateContactRequest{
			FullName: "Updated Name",
		}

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+unknownID, bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("UpdateContact_SoftDeletedContactNotFound", func(t *testing.T) {
		// spec: CON-047[2]
		createReq := handlers.CreateContactRequest{
			FullName: "Update SoftDeleted " + ns,
		}
		jsonBody, _ := json.Marshal(createReq)
		createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		createHTTPReq.Header.Set("Content-Type", "application/json")
		createW := httptest.NewRecorder()
		router.ServeHTTP(createW, createHTTPReq)
		require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

		var createResp api.APIResponse
		require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))
		softDeletedID := createResp.Data.(map[string]interface{})["id"].(string)

		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+softDeletedID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
		require.Equal(t, http.StatusNoContent, deleteW.Code)

		updateReq := handlers.UpdateContactRequest{
			FullName: "Should Not Apply",
		}
		updateBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+softDeletedID, bytes.NewBuffer(updateBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("UpdateContact_ValidRequestReturnsUpdatedContact", func(t *testing.T) {
		// spec: CON-047[3]
		updatedName := "Updated Name " + ns
		updateReq := handlers.UpdateContactRequest{
			FullName: updatedName,
			Cadence:  stringPtr("quarterly"),
		}

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID, bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		updated := response.Data.(map[string]interface{})
		assert.Equal(t, updatedName, updated["full_name"])
		assert.Equal(t, "quarterly", updated["cadence"])
		assert.Equal(t, contactID, updated["id"])

		// Persisted, not just echoed back on the update response.
		getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)
		require.Equal(t, http.StatusOK, getW.Code)

		var getResponse api.APIResponse
		require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &getResponse))
		current := getResponse.Data.(map[string]interface{})
		assert.Equal(t, updatedName, current["full_name"])
		assert.Equal(t, "quarterly", current["cadence"])
	})
}

// TestContactAPI_DeleteValidation covers CON-048: malformed-id rejection,
// not-found for unknown/already-deleted contacts, the no-content shape of a
// successful delete, and the absence of any hard-delete route.
func TestContactAPI_DeleteValidation(t *testing.T) {
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

	ns := uuid.New().String()[:8]

	t.Run("DeleteContact_MalformedIDRejected", func(t *testing.T) {
		// spec: CON-048[0]
		req, _ := http.NewRequest("DELETE", "/api/v1/contacts/not-a-uuid", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("DeleteContact_UnknownContactNotFound", func(t *testing.T) {
		// spec: CON-048[1]
		unknownID := uuid.New().String()
		req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+unknownID, nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("DeleteContact_AlreadyDeletedContactNotFound", func(t *testing.T) {
		// spec: CON-048[1]
		createReq := handlers.CreateContactRequest{
			FullName: "Delete Twice " + ns,
		}
		jsonBody, _ := json.Marshal(createReq)
		createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		createHTTPReq.Header.Set("Content-Type", "application/json")
		createW := httptest.NewRecorder()
		router.ServeHTTP(createW, createHTTPReq)
		require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

		var createResp api.APIResponse
		require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))
		contactID := createResp.Data.(map[string]interface{})["id"].(string)

		firstDeleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		firstDeleteW := httptest.NewRecorder()
		router.ServeHTTP(firstDeleteW, firstDeleteReq)
		require.Equal(t, http.StatusNoContent, firstDeleteW.Code)

		secondDeleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, secondDeleteReq)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("DeleteContact_SuccessfulDeleteReturnsEmptyNoContent", func(t *testing.T) {
		// spec: CON-048[2]
		createReq := handlers.CreateContactRequest{
			FullName: "Delete Success " + ns,
		}
		jsonBody, _ := json.Marshal(createReq)
		createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		createHTTPReq.Header.Set("Content-Type", "application/json")
		createW := httptest.NewRecorder()
		router.ServeHTTP(createW, createHTTPReq)
		require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

		var createResp api.APIResponse
		require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResp))
		contactID := createResp.Data.(map[string]interface{})["id"].(string)

		req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Empty(t, w.Body.Bytes(), "a 204 response must have an empty body")
	})

	t.Run("DeleteContact_NoHardDeleteRouteExposed", func(t *testing.T) {
		// spec: CON-048[3]
		// Registers the contact-resource registrar (RegisterContactRoutes) and
		// enumerates every DELETE route it serves under /contacts. Other
		// registrars mount subresources under /contacts/:id/..., but the
		// contact row's own lifecycle routes are registered here, so a
		// hard-delete variant on the contact resource would surface in this
		// enumeration.
		routesRouter := gin.New()
		routesV1 := routesRouter.Group("/api/v1")
		handlers.RegisterContactRoutes(routesV1, handlers.ContactRouteDeps{
			Contact:       &handlers.ContactHandler{},
			Interaction:   &handlers.InteractionHandler{},
			Note:          &handlers.NoteHandler{},
			ContactMethod: handlers.NewContactMethodHandler(stubApplier{err: errors.New("unused")}),
		})

		var contactDeleteRoutes []string
		for _, route := range routesRouter.Routes() {
			if route.Method != http.MethodDelete {
				continue
			}
			if !strings.HasPrefix(route.Path, "/api/v1/contacts") {
				continue
			}
			contactDeleteRoutes = append(contactDeleteRoutes, route.Path)
		}

		assert.Equal(t, []string{"/api/v1/contacts/:id"}, contactDeleteRoutes,
			"the only DELETE route under /contacts must be the soft-delete by id; no hard/purge/force variant may be registered")
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

	t.Run("ListContacts_DefaultPagination", func(t *testing.T) {
		// spec: CON-017[0]
		// Seed more than the default page size so the un-paginated request is
		// guaranteed to have a 20-row page to return regardless of what other
		// tests have left in the shared DB.
		ns := uuid.New().String()[:8]
		var ids []string
		for i := 0; i < 25; i++ {
			createReq := handlers.CreateContactRequest{
				FullName: fmt.Sprintf("DefaultPage %s %02d", ns, i),
			}
			jsonBody, _ := json.Marshal(createReq)
			createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
			createHTTPReq.Header.Set("Content-Type", "application/json")
			createW := httptest.NewRecorder()
			router.ServeHTTP(createW, createHTTPReq)
			require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

			var created api.APIResponse
			require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &created))
			ids = append(ids, created.Data.(map[string]interface{})["id"].(string))
		}
		defer func() {
			for _, id := range ids {
				delReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+id, nil)
				router.ServeHTTP(httptest.NewRecorder(), delReq)
			}
		}()

		req, _ := http.NewRequest("GET", "/api/v1/contacts", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.True(t, response.Success)
		require.NotNil(t, response.Meta)
		require.NotNil(t, response.Meta.Pagination)
		assert.Equal(t, 1, response.Meta.Pagination.Page)
		assert.Equal(t, 20, response.Meta.Pagination.Limit)

		rows, ok := response.Data.([]interface{})
		require.True(t, ok)
		assert.Len(t, rows, 20)
	})

	t.Run("ListContacts_LimitCapAccepted", func(t *testing.T) {
		// spec: CON-017[1]
		req, _ := http.NewRequest("GET", "/api/v1/contacts?limit=1000", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.True(t, response.Success)
		require.NotNil(t, response.Meta)
		require.NotNil(t, response.Meta.Pagination)
		assert.Equal(t, 1000, response.Meta.Pagination.Limit)
	})

	t.Run("ListContacts_PaginationMetaAccurate", func(t *testing.T) {
		// spec: CON-017[2]
		ns := uuid.New().String()[:8]
		const seeded = 7
		var ids []string
		for i := 0; i < seeded; i++ {
			createReq := handlers.CreateContactRequest{
				FullName: fmt.Sprintf("MetaCount %s %02d", ns, i),
			}
			jsonBody, _ := json.Marshal(createReq)
			createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
			createHTTPReq.Header.Set("Content-Type", "application/json")
			createW := httptest.NewRecorder()
			router.ServeHTTP(createW, createHTTPReq)
			require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

			var created api.APIResponse
			require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &created))
			ids = append(ids, created.Data.(map[string]interface{})["id"].(string))
		}
		defer func() {
			for _, id := range ids {
				delReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+id, nil)
				router.ServeHTTP(httptest.NewRecorder(), delReq)
			}
		}()

		req, _ := http.NewRequest("GET", "/api/v1/contacts?search=MetaCount+"+ns+"&limit=3", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.True(t, response.Success)
		require.NotNil(t, response.Meta)
		require.NotNil(t, response.Meta.Pagination)
		assert.EqualValues(t, seeded, response.Meta.Pagination.Total)
		assert.Equal(t, 3, response.Meta.Pagination.Pages, "ceil(7/3) = 3")
		assert.Equal(t, 1, response.Meta.Pagination.Page)
		assert.Equal(t, 3, response.Meta.Pagination.Limit)

		rows, ok := response.Data.([]interface{})
		require.True(t, ok)
		assert.Len(t, rows, 3)
	})

	t.Run("ListContacts_CadenceFilter", func(t *testing.T) {
		// spec: CON-018[2]
		ns := uuid.New().String()[:8]
		var withCadenceIDs, withoutCadenceIDs []string

		for i := 0; i < 3; i++ {
			createReq := handlers.CreateContactRequest{
				FullName: fmt.Sprintf("CadenceFilter %s WithCadence %02d", ns, i),
				Cadence:  stringPtr("monthly"),
			}
			jsonBody, _ := json.Marshal(createReq)
			createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
			createHTTPReq.Header.Set("Content-Type", "application/json")
			createW := httptest.NewRecorder()
			router.ServeHTTP(createW, createHTTPReq)
			require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

			var created api.APIResponse
			require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &created))
			withCadenceIDs = append(withCadenceIDs, created.Data.(map[string]interface{})["id"].(string))
		}
		for i := 0; i < 3; i++ {
			createReq := handlers.CreateContactRequest{
				FullName: fmt.Sprintf("CadenceFilter %s NoCadence %02d", ns, i),
			}
			jsonBody, _ := json.Marshal(createReq)
			createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
			createHTTPReq.Header.Set("Content-Type", "application/json")
			createW := httptest.NewRecorder()
			router.ServeHTTP(createW, createHTTPReq)
			require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

			var created api.APIResponse
			require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &created))
			withoutCadenceIDs = append(withoutCadenceIDs, created.Data.(map[string]interface{})["id"].(string))
		}
		defer func() {
			for _, id := range append(withCadenceIDs, withoutCadenceIDs...) {
				delReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+id, nil)
				router.ServeHTTP(httptest.NewRecorder(), delReq)
			}
		}()

		req, _ := http.NewRequest("GET", "/api/v1/contacts?search=CadenceFilter+"+ns+"&cadence_filter=has_cadence&limit=100", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var hasResp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &hasResp))
		hasRows, ok := hasResp.Data.([]interface{})
		require.True(t, ok)
		require.Len(t, hasRows, 3)
		for _, row := range hasRows {
			r := row.(map[string]interface{})
			assert.Equal(t, "monthly", r["cadence"])
		}

		req, _ = http.NewRequest("GET", "/api/v1/contacts?search=CadenceFilter+"+ns+"&cadence_filter=no_cadence&limit=100", nil)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var noResp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &noResp))
		noRows, ok := noResp.Data.([]interface{})
		require.True(t, ok)
		require.Len(t, noRows, 3)
		for _, row := range noRows {
			r := row.(map[string]interface{})
			_, hasCadenceKey := r["cadence"]
			assert.False(t, hasCadenceKey, "no_cadence rows must not carry a cadence value")
		}
	})

	t.Run("ListContacts_InvalidCadenceFilter", func(t *testing.T) {
		// spec: CON-018[4]
		req, _ := http.NewRequest("GET", "/api/v1/contacts?cadence_filter=bogus", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ListContacts_InvalidFollowupFilter", func(t *testing.T) {
		// spec: CON-018[4]
		req, _ := http.NewRequest("GET", "/api/v1/contacts?followup_filter=bogus", nil)

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

	t.Run("GetContact_SoftDeleted_NotFound", func(t *testing.T) {
		// spec: CON-005[1]
		ns := uuid.New().String()[:8]
		createReq := handlers.CreateContactRequest{
			FullName: "SoftDeleted Test " + ns,
		}
		jsonBody, _ := json.Marshal(createReq)
		createHTTPReq, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
		createHTTPReq.Header.Set("Content-Type", "application/json")
		createW := httptest.NewRecorder()
		router.ServeHTTP(createW, createHTTPReq)
		require.Equal(t, http.StatusCreated, createW.Code, createW.Body.String())

		var createResponse api.APIResponse
		require.NoError(t, json.Unmarshal(createW.Body.Bytes(), &createResponse))
		contactID := createResponse.Data.(map[string]interface{})["id"].(string)

		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
		require.Equal(t, http.StatusNoContent, deleteW.Code)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
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
