package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
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

func setupInteractionTestRouter() (*gin.Engine, func()) {
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, repository.NewContactTaskRepository(database.Queries))
	contactHandler := handlers.NewContactHandler(contactService, nil)
	interactionHandler := handlers.NewInteractionHandler(contactService, interactionRepo, nil)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	{
		contacts := v1.Group("/contacts")
		{
			contacts.POST("", contactHandler.CreateContact)
			contacts.GET("/:id", contactHandler.GetContact)
			contacts.DELETE("/:id", contactHandler.DeleteContact)
			contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
			contacts.GET("/:id/interactions", interactionHandler.ListContactInteractions)
			contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
		}
		interactions := v1.Group("/interactions")
		{
			interactions.DELETE("/:id", interactionHandler.DeleteInteraction)
		}
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

func createInteractionTestContact(t *testing.T, router *gin.Engine, name string) string {
	t.Helper()
	createReq := handlers.CreateContactRequest{
		FullName: name,
	}
	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var response api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	data := response.Data.(map[string]interface{})
	return data["id"].(string)
}

func deleteInteractionTestContact(t *testing.T, router *gin.Engine, contactID string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
}

func TestInteraction_CreateAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Interaction Test User")
	defer deleteInteractionTestContact(t, router, contactID)

	t.Run("CreateManualInteraction", func(t *testing.T) {
		pastDate := accelerated.GetCurrentTime().Add(-24 * time.Hour).Format(time.RFC3339)
		desc := "Had coffee together"
		body, _ := json.Marshal(map[string]string{
			"occurred_at": pastDate,
			"description": desc,
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		data := response.Data.(map[string]interface{})
		assert.Equal(t, "manual", data["source"])
		assert.Equal(t, "Had coffee together", data["description"])
		assert.NotEmpty(t, data["id"])
	})

	t.Run("CreateInteractionWithoutBody", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		data := response.Data.(map[string]interface{})
		assert.Equal(t, "manual", data["source"])
	})

	t.Run("ListContactInteractions", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/interactions", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		data := response.Data.([]interface{})
		assert.GreaterOrEqual(t, len(data), 2) // At least 2 from the creates above

		// Check pagination meta
		assert.NotNil(t, response.Meta)
		assert.NotNil(t, response.Meta.Pagination)
		assert.GreaterOrEqual(t, response.Meta.Pagination.Total, int64(2))
	})
}

func TestInteraction_ManualDeduplication(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Dedup Test User")
	defer deleteInteractionTestContact(t, router, contactID)

	t.Run("DuplicateWithin30MinWindowReturnsExisting", func(t *testing.T) {
		// Create first interaction
		body, _ := json.Marshal(map[string]string{})
		req1, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req1.Header.Set("Content-Type", "application/json")

		w1 := httptest.NewRecorder()
		router.ServeHTTP(w1, req1)
		require.Equal(t, http.StatusCreated, w1.Code)

		var resp1 api.APIResponse
		err := json.Unmarshal(w1.Body.Bytes(), &resp1)
		require.NoError(t, err)
		data1 := resp1.Data.(map[string]interface{})
		firstID := data1["id"].(string)

		// Create second interaction immediately (within 30-min window)
		body2, _ := json.Marshal(map[string]string{})
		req2, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body2))
		req2.Header.Set("Content-Type", "application/json")

		w2 := httptest.NewRecorder()
		router.ServeHTTP(w2, req2)
		require.Equal(t, http.StatusCreated, w2.Code)

		var resp2 api.APIResponse
		err = json.Unmarshal(w2.Body.Bytes(), &resp2)
		require.NoError(t, err)
		data2 := resp2.Data.(map[string]interface{})
		secondID := data2["id"].(string)

		// Should return the same interaction (deduped)
		assert.Equal(t, firstID, secondID, "duplicate interaction within 30-min window should return existing")
	})
}

func TestInteraction_SoftDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Delete Interaction Test User")
	defer deleteInteractionTestContact(t, router, contactID)

	// Create an interaction
	pastDate := accelerated.GetCurrentTime().Add(-48 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]string{
		"occurred_at": pastDate,
	})
	req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var createResp api.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &createResp)
	require.NoError(t, err)
	interactionData := createResp.Data.(map[string]interface{})
	interactionID := interactionData["id"].(string)

	t.Run("DeleteInteraction", func(t *testing.T) {
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/interactions/"+interactionID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)

		assert.Equal(t, http.StatusNoContent, deleteW.Code)
	})

	t.Run("DeletedInteractionNotInList", func(t *testing.T) {
		listReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/interactions", nil)
		listW := httptest.NewRecorder()
		router.ServeHTTP(listW, listReq)

		var listResp api.APIResponse
		err := json.Unmarshal(listW.Body.Bytes(), &listResp)
		require.NoError(t, err)

		// Verify the deleted interaction is not in the list
		data := listResp.Data.([]interface{})
		for _, item := range data {
			interaction := item.(map[string]interface{})
			assert.NotEqual(t, interactionID, interaction["id"], "soft-deleted interaction should not appear in list")
		}
	})

	t.Run("DeleteNonExistentInteraction", func(t *testing.T) {
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/interactions/00000000-0000-0000-0000-000000000000", nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)

		assert.Equal(t, http.StatusNotFound, deleteW.Code)
	})
}

func TestInteraction_UpdatesLastContacted(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Last Contacted Update Test")
	defer deleteInteractionTestContact(t, router, contactID)

	t.Run("ManualInteractionUpdatesLastContacted", func(t *testing.T) {
		// Create interaction with specific past date
		pastDate := "2025-06-15T14:00:00Z"
		body, _ := json.Marshal(map[string]string{
			"occurred_at": pastDate,
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		// Verify last_contacted was updated on the contact
		getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)

		var getResp api.APIResponse
		err := json.Unmarshal(getW.Body.Bytes(), &getResp)
		require.NoError(t, err)
		contactData := getResp.Data.(map[string]interface{})
		lastContacted := contactData["last_contacted"].(string)
		assert.Contains(t, lastContacted, "2025-06-15")
	})
}

func TestInteraction_NonExistentContact(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	t.Run("CreateInteractionForNonExistentContact", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/00000000-0000-0000-0000-000000000000/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("ListInteractionsForNonExistentContact", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/00000000-0000-0000-0000-000000000000/interactions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// Should return 200 with empty list (not 404)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("InvalidContactID", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/api/v1/contacts/not-a-uuid/interactions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestInteraction_FutureDateRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupInteractionTestRouter()
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Future Date Test User")
	defer deleteInteractionTestContact(t, router, contactID)

	t.Run("FutureDateRejected", func(t *testing.T) {
		futureDate := accelerated.GetCurrentTime().Add(48 * time.Hour).Format(time.RFC3339)
		body, _ := json.Marshal(map[string]string{
			"occurred_at": futureDate,
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
