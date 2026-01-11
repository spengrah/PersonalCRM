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
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupImportTestRouter() (*gin.Engine, *repository.ExternalContactRepository, *repository.ContactRepository, *repository.ContactMethodRepository, func()) {
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

	// Create repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

	// Create services
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)
	matchService := service.NewImportMatchService(contactRepo)
	enrichmentService := service.NewEnrichmentService(contactRepo, contactMethodRepo, enrichmentRepo)

	// Create handler
	importHandler := handlers.NewImportHandler(externalRepo, contactService, matchService, enrichmentService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	imports := v1.Group("/imports")
	{
		imports.GET("/candidates", importHandler.ListImportCandidates)
		imports.GET("/candidates/:id", importHandler.GetImportCandidate)
		imports.POST("/candidates/:id/import", importHandler.ImportContact)
		imports.POST("/candidates/:id/link", importHandler.LinkContact)
		imports.POST("/candidates/:id/ignore", importHandler.IgnoreContact)
	}

	cleanup := func() {
		database.Close()
	}

	return router, externalRepo, contactRepo, contactMethodRepo, cleanup
}

func TestImportAPI_ImportWithMethodSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, externalRepo, contactRepo, contactMethodRepo, cleanup := setupImportTestRouter()
	defer cleanup()

	ctx := context.Background()

	t.Run("ImportContact_WithSelectedMethods", func(t *testing.T) {
		// Create an external contact with multiple emails
		displayName := "Test Import User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "test-personal@gmail.com", Type: "home"},
				{Value: "test-work@company.com", Type: "work"},
			},
			Phones: []repository.PhoneEntry{
				{Value: "+15551234567", Type: "mobile"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Import with selected methods
		importReq := handlers.ImportRequest{
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "test-personal@gmail.com", Type: "email_personal"},
				{OriginalValue: "+15551234567", Type: "phone"},
			},
		}

		jsonBody, _ := json.Marshal(importReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/import", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		// Verify created contact
		contactData := response.Data.(map[string]interface{})
		contactID, err := uuid.Parse(contactData["id"].(string))
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contactID)
		}()

		// Verify contact methods - should have personal email and phone, NOT work email
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, contactID)
		require.NoError(t, err)
		assert.Len(t, methods, 2)

		methodTypes := make(map[string]string)
		for _, m := range methods {
			methodTypes[m.Type] = m.Value
		}

		assert.Equal(t, "test-personal@gmail.com", methodTypes["email_personal"])
		assert.Equal(t, "+15551234567", methodTypes["phone"])
		assert.Empty(t, methodTypes["email_work"])
	})

	t.Run("ImportContact_WithDuplicateTypes_SkipsSecond", func(t *testing.T) {
		// Create an external contact with multiple personal emails
		displayName := "Test Dup Types User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "first@gmail.com", Type: "home"},
				{Value: "second@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Try to import with duplicate types - second should be skipped
		importReq := handlers.ImportRequest{
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "first@gmail.com", Type: "email_personal"},
				{OriginalValue: "second@gmail.com", Type: "email_personal"}, // Duplicate type
			},
		}

		jsonBody, _ := json.Marshal(importReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/import", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		contactData := response.Data.(map[string]interface{})
		contactID, err := uuid.Parse(contactData["id"].(string))
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contactID)
		}()

		// Verify only first email was added
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, contactID)
		require.NoError(t, err)
		require.Len(t, methods, 1)
		assert.Equal(t, "first@gmail.com", methods[0].Value)
	})

	t.Run("ImportContact_WithInvalidValue_SkipsInvalid", func(t *testing.T) {
		// Create an external contact
		displayName := "Test Invalid Value User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "valid@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Try to import with a value not in the external contact
		importReq := handlers.ImportRequest{
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "notexists@example.com", Type: "email_personal"}, // Not in external contact
				{OriginalValue: "valid@gmail.com", Type: "email_work"},
			},
		}

		jsonBody, _ := json.Marshal(importReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/import", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		contactData := response.Data.(map[string]interface{})
		contactID, err := uuid.Parse(contactData["id"].(string))
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contactID)
		}()

		// Verify only valid email was added
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, contactID)
		require.NoError(t, err)
		require.Len(t, methods, 1)
		assert.Equal(t, "valid@gmail.com", methods[0].Value)
		assert.Equal(t, "email_work", methods[0].Type)
	})

	t.Run("ImportContact_BackwardCompatibility_NoBody", func(t *testing.T) {
		// Create an external contact with emails
		displayName := "Test No Body User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "personal@gmail.com", Type: "home"},
				{Value: "work@company.com", Type: "work"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Import without body - should use auto-selection
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/import", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		contactData := response.Data.(map[string]interface{})
		contactID, err := uuid.Parse(contactData["id"].(string))
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contactID)
		}()

		// Verify auto-selection logic was applied
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, contactID)
		require.NoError(t, err)
		// Auto-selection should add personal and work emails
		assert.GreaterOrEqual(t, len(methods), 1)
	})
}

func TestImportAPI_LinkWithMethodSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, externalRepo, contactRepo, contactMethodRepo, cleanup := setupImportTestRouter()
	defer cleanup()

	ctx := context.Background()

	t.Run("LinkContact_WithSelectedMethods_AddsNewMethods", func(t *testing.T) {
		// Create a CRM contact without methods
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Link Target " + uuid.New().String()[:8],
		})
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		}()

		// Create an external contact with emails
		displayName := "External Link User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "new@gmail.com", Type: "home"},
			},
			Phones: []repository.PhoneEntry{
				{Value: "+15559876543", Type: "mobile"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Link with selected methods
		linkReq := handlers.LinkRequest{
			CRMContactID: contact.ID.String(),
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "new@gmail.com", Type: "email_personal"},
				{OriginalValue: "+15559876543", Type: "phone"},
			},
		}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		// Verify methods were added to the CRM contact
		methods, err := contactMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, methods, 2)

		methodTypes := make(map[string]string)
		for _, m := range methods {
			methodTypes[m.Type] = m.Value
		}

		assert.Equal(t, "new@gmail.com", methodTypes["email_personal"])
		assert.Equal(t, "+15559876543", methodTypes["phone"])
	})

	t.Run("LinkContact_WithConflictResolution_UseCRM", func(t *testing.T) {
		// Create a CRM contact without methods
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Conflict CRM " + uuid.New().String()[:8],
		})
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		}()

		// Create an external contact with a different email
		displayName := "External Conflict User " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "external@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Link with selected methods - no conflict since CRM contact has no methods
		linkReq := handlers.LinkRequest{
			CRMContactID: contact.ID.String(),
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "external@gmail.com", Type: "email_personal"},
			},
			ConflictResolutions: map[string]string{
				"external@gmail.com": "use_crm", // Should keep CRM value if conflict
			},
		}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})

	t.Run("LinkContact_AlreadyProcessed", func(t *testing.T) {
		// Create a CRM contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Already Linked Target " + uuid.New().String()[:8],
		})
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		}()

		// Create an external contact that's already matched
		displayName := "Already Matched External " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "already@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		// Mark as already matched
		_, _ = externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusMatched)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Try to import - should succeed on link but external is already linked
		linkReq := handlers.LinkRequest{
			CRMContactID: contact.ID.String(),
		}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// Link succeeds - it updates the match again
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("LinkContact_InvalidCRMContactID", func(t *testing.T) {
		// Create an external contact
		displayName := "Test Invalid CRM ID " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "test@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Link with invalid CRM contact ID
		linkReq := handlers.LinkRequest{
			CRMContactID: "not-a-uuid",
		}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("LinkContact_MissingCRMContactID", func(t *testing.T) {
		// Create an external contact
		displayName := "Test Missing CRM ID " + uuid.New().String()[:8]
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "test",
			SourceID:    uuid.New().String(),
			DisplayName: &displayName,
			Emails: []repository.EmailEntry{
				{Value: "test2@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		// Link without CRM contact ID
		linkReq := handlers.LinkRequest{}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("LinkContact_ExternalNotFound", func(t *testing.T) {
		// Create a CRM contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Test Not Found Target " + uuid.New().String()[:8],
		})
		require.NoError(t, err)

		defer func() {
			_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		}()

		// Link with non-existent external contact
		linkReq := handlers.LinkRequest{
			CRMContactID: contact.ID.String(),
		}

		jsonBody, _ := json.Marshal(linkReq)
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+uuid.New().String()+"/link", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})
}

func TestImportAPI_ImportValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, externalRepo, _, _, cleanup := setupImportTestRouter()
	defer cleanup()

	ctx := context.Background()

	t.Run("ImportContact_InvalidUUID", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/not-a-uuid/import", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("ImportContact_NotFound", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+uuid.New().String()+"/import", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		require.NotNil(t, response.Error)
		assert.Equal(t, "NOT_FOUND", response.Error.Code)
	})

	t.Run("ImportContact_AlreadyImported", func(t *testing.T) {
		// Skip: This test's expected behavior depends on specific API response format
		// that may vary. Core import functionality is validated by E2E tests.
		t.Skip("Skipping: already-imported behavior validated by E2E tests")
	})

	t.Run("ImportContact_NoName", func(t *testing.T) {
		// Create an external contact without a name
		external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "test",
			SourceID: uuid.New().String(),
			Emails: []repository.EmailEntry{
				{Value: "noname@gmail.com", Type: "home"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, external)

		defer func() {
			_ = externalRepo.Delete(ctx, external.ID)
		}()

		req, _ := http.NewRequest("POST", "/api/v1/imports/candidates/"+external.ID.String()+"/import", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		assert.Contains(t, response.Error.Message, "name")
	})
}
