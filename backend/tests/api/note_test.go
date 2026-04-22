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
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupNoteTestRouter() (*gin.Engine, func()) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

	// Migrations are applied once by TestMain.
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

	// Set up repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)

	// Set up services
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries), nil, nil)
	wireCadenceUpdaterForAPITest(nil, database, contactService)
	noteService := service.NewNoteService(noteRepo, contactRepo)

	// Set up handlers
	contactHandler := handlers.NewContactHandler(contactService, nil)
	noteHandler := handlers.NewNoteHandler(noteService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")

	// Contact routes (for creating test contacts)
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("/:id", contactHandler.GetContact)
		contacts.DELETE("/:id", contactHandler.DeleteContact)
		contacts.GET("/:id/notes", noteHandler.GetContactNotepad)
		contacts.PUT("/:id/notes", noteHandler.SaveContactNotepad)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

// createTestContact creates a contact for testing and returns its ID
func createTestContact(t *testing.T, router *gin.Engine, name string) string {
	t.Helper()

	createReq := handlers.CreateContactRequest{
		FullName: name,
	}

	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "Failed to create test contact: %s", w.Body.String())

	var response api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	contactData := response.Data.(map[string]interface{})
	return contactData["id"].(string)
}

// deleteTestContact deletes a test contact
func deleteTestContact(t *testing.T, router *gin.Engine, contactID string) {
	t.Helper()

	req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
}

func TestNoteAPI_GetContactNotepad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupNoteTestRouter()
	defer cleanup()

	t.Run("GetNote_NonExistent_Returns204", func(t *testing.T) {
		// Create a contact without notes
		contactID := createTestContact(t, router, "Note Test User")
		defer deleteTestContact(t, router, contactID)

		// Get notes - should return 204 No Content since no note exists
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Empty(t, w.Body.String())
	})

	t.Run("GetNote_ExistingNote_Returns200", func(t *testing.T) {
		contactID := createTestContact(t, router, "Note Test User With Note")
		defer deleteTestContact(t, router, contactID)

		// First create a note
		saveReq := handlers.SaveNoteRequest{Body: "Test note content"}
		jsonBody, _ := json.Marshal(saveReq)
		putReq, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		router.ServeHTTP(putW, putReq)
		require.Equal(t, http.StatusOK, putW.Code)

		// Now get the note
		getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)

		assert.Equal(t, http.StatusOK, getW.Code)

		var response api.APIResponse
		err := json.Unmarshal(getW.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		noteData := response.Data.(map[string]interface{})
		assert.Equal(t, "Test note content", noteData["body"])
		assert.Equal(t, contactID, noteData["contact_id"])
		assert.NotEmpty(t, noteData["id"])
		assert.NotEmpty(t, noteData["created_at"])
		assert.NotEmpty(t, noteData["updated_at"])
	})

	t.Run("GetNote_InvalidContactID_Returns400", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/invalid-uuid/notes", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
	})

	t.Run("GetNote_ContactNotFound_Returns404", func(t *testing.T) {
		nonExistentID := uuid.New().String()
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+nonExistentID+"/notes", nil)
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

func TestNoteAPI_SaveContactNotepad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupNoteTestRouter()
	defer cleanup()

	t.Run("SaveNote_CreateNew_Returns200", func(t *testing.T) {
		contactID := createTestContact(t, router, "Save Note Test User")
		defer deleteTestContact(t, router, contactID)

		saveReq := handlers.SaveNoteRequest{Body: "New note content"}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		noteData := response.Data.(map[string]interface{})
		assert.Equal(t, "New note content", noteData["body"])
		assert.Equal(t, contactID, noteData["contact_id"])
	})

	t.Run("SaveNote_UpdateExisting_Returns200", func(t *testing.T) {
		contactID := createTestContact(t, router, "Update Note Test User")
		defer deleteTestContact(t, router, contactID)

		// Create initial note
		saveReq1 := handlers.SaveNoteRequest{Body: "Initial content"}
		jsonBody1, _ := json.Marshal(saveReq1)
		req1, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody1))
		req1.Header.Set("Content-Type", "application/json")
		w1 := httptest.NewRecorder()
		router.ServeHTTP(w1, req1)
		require.Equal(t, http.StatusOK, w1.Code)

		// Get the note ID
		var response1 api.APIResponse
		err := json.Unmarshal(w1.Body.Bytes(), &response1)
		require.NoError(t, err)
		noteData1 := response1.Data.(map[string]interface{})
		originalNoteID := noteData1["id"].(string)

		// Update the note
		saveReq2 := handlers.SaveNoteRequest{Body: "Updated content"}
		jsonBody2, _ := json.Marshal(saveReq2)
		req2, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody2))
		req2.Header.Set("Content-Type", "application/json")
		w2 := httptest.NewRecorder()
		router.ServeHTTP(w2, req2)

		assert.Equal(t, http.StatusOK, w2.Code)

		var response2 api.APIResponse
		err = json.Unmarshal(w2.Body.Bytes(), &response2)
		require.NoError(t, err)

		noteData2 := response2.Data.(map[string]interface{})
		assert.Equal(t, "Updated content", noteData2["body"])
		// Note ID should be the same (upsert)
		assert.Equal(t, originalNoteID, noteData2["id"])
	})

	t.Run("SaveNote_EmptyBody_DeletesNote_Returns204", func(t *testing.T) {
		contactID := createTestContact(t, router, "Delete Note Test User")
		defer deleteTestContact(t, router, contactID)

		// Create a note first
		saveReq1 := handlers.SaveNoteRequest{Body: "Content to delete"}
		jsonBody1, _ := json.Marshal(saveReq1)
		req1, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody1))
		req1.Header.Set("Content-Type", "application/json")
		w1 := httptest.NewRecorder()
		router.ServeHTTP(w1, req1)
		require.Equal(t, http.StatusOK, w1.Code)

		// Delete by sending empty body
		saveReq2 := handlers.SaveNoteRequest{Body: ""}
		jsonBody2, _ := json.Marshal(saveReq2)
		req2, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody2))
		req2.Header.Set("Content-Type", "application/json")
		w2 := httptest.NewRecorder()
		router.ServeHTTP(w2, req2)

		assert.Equal(t, http.StatusNoContent, w2.Code)
		assert.Empty(t, w2.Body.String())

		// Verify note is actually deleted
		getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)
		assert.Equal(t, http.StatusNoContent, getW.Code)
	})

	t.Run("SaveNote_WhitespaceOnlyBody_DeletesNote_Returns204", func(t *testing.T) {
		contactID := createTestContact(t, router, "Whitespace Note Test User")
		defer deleteTestContact(t, router, contactID)

		// Create a note first
		saveReq1 := handlers.SaveNoteRequest{Body: "Content to delete"}
		jsonBody1, _ := json.Marshal(saveReq1)
		req1, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody1))
		req1.Header.Set("Content-Type", "application/json")
		w1 := httptest.NewRecorder()
		router.ServeHTTP(w1, req1)
		require.Equal(t, http.StatusOK, w1.Code)

		// Delete by sending whitespace-only body
		saveReq2 := handlers.SaveNoteRequest{Body: "   \n\t   "}
		jsonBody2, _ := json.Marshal(saveReq2)
		req2, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody2))
		req2.Header.Set("Content-Type", "application/json")
		w2 := httptest.NewRecorder()
		router.ServeHTTP(w2, req2)

		assert.Equal(t, http.StatusNoContent, w2.Code)

		// Verify note is actually deleted
		getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)
		assert.Equal(t, http.StatusNoContent, getW.Code)
	})

	t.Run("SaveNote_TrimsWhitespace", func(t *testing.T) {
		contactID := createTestContact(t, router, "Trim Whitespace Test User")
		defer deleteTestContact(t, router, contactID)

		saveReq := handlers.SaveNoteRequest{Body: "  trimmed content  "}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		noteData := response.Data.(map[string]interface{})
		assert.Equal(t, "trimmed content", noteData["body"])
	})

	t.Run("SaveNote_InvalidContactID_Returns400", func(t *testing.T) {
		saveReq := handlers.SaveNoteRequest{Body: "Some content"}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/invalid-uuid/notes", bytes.NewBuffer(jsonBody))
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

	t.Run("SaveNote_ContactNotFound_Returns404", func(t *testing.T) {
		nonExistentID := uuid.New().String()
		saveReq := handlers.SaveNoteRequest{Body: "Some content"}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+nonExistentID+"/notes", bytes.NewBuffer(jsonBody))
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

	t.Run("SaveNote_BodyTooLong_Returns400", func(t *testing.T) {
		contactID := createTestContact(t, router, "Body Too Long Test User")
		defer deleteTestContact(t, router, contactID)

		// Create body exceeding 50,000 chars
		longBody := strings.Repeat("a", 50001)
		saveReq := handlers.SaveNoteRequest{Body: longBody}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody))
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

	t.Run("SaveNote_BodyAtMaxLength_Returns200", func(t *testing.T) {
		contactID := createTestContact(t, router, "Max Length Test User")
		defer deleteTestContact(t, router, contactID)

		// Create body at exactly 50,000 chars
		maxBody := strings.Repeat("a", 50000)
		saveReq := handlers.SaveNoteRequest{Body: maxBody}
		jsonBody, _ := json.Marshal(saveReq)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response.Success)
		noteData := response.Data.(map[string]interface{})
		assert.Equal(t, 50000, len(noteData["body"].(string)))
	})

	t.Run("SaveNote_MalformedJSON_Returns400", func(t *testing.T) {
		contactID := createTestContact(t, router, "Malformed JSON Test User")
		defer deleteTestContact(t, router, contactID)

		malformedJSON := []byte(`{"body": invalid}`)
		req, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(malformedJSON))
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

func TestNoteAPI_LazyCreationPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupNoteTestRouter()
	defer cleanup()

	t.Run("LazyCreation_NoRecordUntilContent", func(t *testing.T) {
		contactID := createTestContact(t, router, "Lazy Creation Test User")
		defer deleteTestContact(t, router, contactID)

		// Initially no note exists
		getReq1, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW1 := httptest.NewRecorder()
		router.ServeHTTP(getW1, getReq1)
		assert.Equal(t, http.StatusNoContent, getW1.Code, "Note should not exist initially")

		// Save empty body - still no record
		saveReq1 := handlers.SaveNoteRequest{Body: ""}
		jsonBody1, _ := json.Marshal(saveReq1)
		putReq1, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody1))
		putReq1.Header.Set("Content-Type", "application/json")
		putW1 := httptest.NewRecorder()
		router.ServeHTTP(putW1, putReq1)
		assert.Equal(t, http.StatusNoContent, putW1.Code, "Empty body should return 204")

		// Still no note
		getReq2, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW2 := httptest.NewRecorder()
		router.ServeHTTP(getW2, getReq2)
		assert.Equal(t, http.StatusNoContent, getW2.Code, "Note should still not exist after empty save")

		// Now save actual content
		saveReq2 := handlers.SaveNoteRequest{Body: "Actual content"}
		jsonBody2, _ := json.Marshal(saveReq2)
		putReq2, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody2))
		putReq2.Header.Set("Content-Type", "application/json")
		putW2 := httptest.NewRecorder()
		router.ServeHTTP(putW2, putReq2)
		assert.Equal(t, http.StatusOK, putW2.Code, "Non-empty body should return 200")

		// Now note exists
		getReq3, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW3 := httptest.NewRecorder()
		router.ServeHTTP(getW3, getReq3)
		assert.Equal(t, http.StatusOK, getW3.Code, "Note should exist after content save")

		// Delete with empty body
		saveReq3 := handlers.SaveNoteRequest{Body: ""}
		jsonBody3, _ := json.Marshal(saveReq3)
		putReq3, _ := http.NewRequest("PUT", "/api/v1/contacts/"+contactID+"/notes", bytes.NewBuffer(jsonBody3))
		putReq3.Header.Set("Content-Type", "application/json")
		putW3 := httptest.NewRecorder()
		router.ServeHTTP(putW3, putReq3)
		assert.Equal(t, http.StatusNoContent, putW3.Code, "Empty body should delete and return 204")

		// Note is deleted
		getReq4, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/notes", nil)
		getW4 := httptest.NewRecorder()
		router.ServeHTTP(getW4, getReq4)
		assert.Equal(t, http.StatusNoContent, getW4.Code, "Note should be deleted")
	})
}
