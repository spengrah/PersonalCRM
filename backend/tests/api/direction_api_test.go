package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
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

func setupDirectionAPIRouter(t *testing.T) (*gin.Engine, *repository.ContactTaskRepository, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	gin.SetMode(gin.TestMode)

	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	ctx := context.Background()
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)
	contactHandler := handlers.NewContactHandler(contactService)
	interactionHandler := handlers.NewInteractionHandler(contactService, interactionRepo)

	// Create a minimal contact task service for the handler (no real Todoist)
	contactTaskService := service.NewContactTaskServiceForTest(contactTaskRepo, contactRepo, nil, "http://localhost:3000")
	contactTaskHandler := handlers.NewContactTaskHandler(contactTaskService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())

	v1 := router.Group("/api/v1")
	{
		contacts := v1.Group("/contacts")
		{
			contacts.GET("", contactHandler.ListContacts)
			contacts.POST("", contactHandler.CreateContact)
			contacts.GET("/:id", contactHandler.GetContact)
			contacts.GET("/:id/interactions", interactionHandler.ListContactInteractions)
			contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
			contacts.GET("/:id/tasks", contactTaskHandler.ListContactTasks)
		}
	}

	cleanup := func() {
		database.Close()
	}

	return router, contactTaskRepo, cleanup
}

func createDirectionTestContact(t *testing.T, router *gin.Engine, name string) string {
	t.Helper()
	body, _ := json.Marshal(handlers.CreateContactRequest{FullName: name})
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp.Data.(map[string]interface{})
	return data["id"].(string)
}

func TestInteractionAPI_DirectionInResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, _, cleanup := setupDirectionAPIRouter(t)
	defer cleanup()

	contactID := createDirectionTestContact(t, router, "Direction API Test")

	t.Run("CreateWithDirection", func(t *testing.T) {
		pastDate := accelerated.GetCurrentTime().Add(-3600_000_000_000).Format("2006-01-02T15:04:05Z07:00")
		body, _ := json.Marshal(map[string]string{
			"occurred_at": pastDate,
			"direction":   "outbound",
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		data := resp.Data.(map[string]interface{})
		assert.Equal(t, "outbound", data["direction"], "response should include direction")
	})

	t.Run("CreateWithoutDirection_DefaultsMutual", func(t *testing.T) {
		pastDate := accelerated.GetCurrentTime().Add(-7200_000_000_000).Format("2006-01-02T15:04:05Z07:00")
		body, _ := json.Marshal(map[string]string{
			"occurred_at": pastDate,
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		data := resp.Data.(map[string]interface{})
		assert.Equal(t, "mutual", data["direction"], "no direction should default to mutual")
	})

	t.Run("ListIncludesDirection", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/interactions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		items := resp.Data.([]interface{})
		require.Greater(t, len(items), 0)
		first := items[0].(map[string]interface{})
		assert.Contains(t, first, "direction", "list response should include direction field")
	})
}

func TestContactAPI_HasPendingFollowup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, contactTaskRepo, cleanup := setupDirectionAPIRouter(t)
	defer cleanup()
	ctx := context.Background()

	contactID := createDirectionTestContact(t, router, "Pending Followup API Test")

	t.Run("NoPendingFollowup", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		data := resp.Data.(map[string]interface{})
		assert.Equal(t, false, data["has_pending_followup"])
	})

	t.Run("WithPendingFollowup", func(t *testing.T) {
		// Create a managed follow-up task
		id, err := uuid.Parse(contactID)
		require.NoError(t, err)
		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      id,
			Provider:       "todoist",
			Kind:           "follow_up",
			ExternalTaskID: "test-followup-api-" + contactID,
			State:          "managed",
		})
		require.NoError(t, err)

		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		data := resp.Data.(map[string]interface{})
		assert.Equal(t, true, data["has_pending_followup"])
	})
}

func TestContactAPI_DirectionTimestamps(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, _, cleanup := setupDirectionAPIRouter(t)
	defer cleanup()

	contactID := createDirectionTestContact(t, router, "Direction Timestamps API Test")

	// Record a mutual interaction to populate timestamps
	pastDate := accelerated.GetCurrentTime().Add(-3600_000_000_000).Format("2006-01-02T15:04:05Z07:00")
	body, _ := json.Marshal(map[string]string{
		"occurred_at": pastDate,
		"direction":   "mutual",
	})
	req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Get contact and verify new fields are present
	req, _ = http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp.Data.(map[string]interface{})

	assert.Contains(t, data, "last_interaction_at", "response should include last_interaction_at")
	assert.Contains(t, data, "last_outreach_at", "response should include last_outreach_at")
	assert.Contains(t, data, "last_response_at", "response should include last_response_at")
	assert.Contains(t, data, "has_pending_followup", "response should include has_pending_followup")
}

func TestContactAPI_FollowupFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, contactTaskRepo, cleanup := setupDirectionAPIRouter(t)
	defer cleanup()
	ctx := context.Background()

	contactID := createDirectionTestContact(t, router, "Followup Filter API Test")
	id, _ := uuid.Parse(contactID)

	// Create a follow-up for this contact
	_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      id,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: "test-filter-api-" + contactID,
		State:          "managed",
	})
	require.NoError(t, err)

	t.Run("has_followup_includes_contact", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?followup_filter=has_followup", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		items := resp.Data.([]interface{})
		found := false
		for _, item := range items {
			c := item.(map[string]interface{})
			if c["id"] == contactID {
				found = true
				break
			}
		}
		assert.True(t, found, "contact with follow-up should appear in has_followup results")
	})

	t.Run("no_followup_excludes_contact", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts?followup_filter=no_followup", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		items := resp.Data.([]interface{})
		found := false
		for _, item := range items {
			c := item.(map[string]interface{})
			if c["id"] == contactID {
				found = true
				break
			}
		}
		assert.False(t, found, "contact with follow-up should NOT appear in no_followup results")
	})
}

func TestContactTaskAPI_FollowUpKindValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, contactTaskRepo, cleanup := setupDirectionAPIRouter(t)
	defer cleanup()
	ctx := context.Background()

	contactID := createDirectionTestContact(t, router, "Task Kind API Test")
	id, _ := uuid.Parse(contactID)

	// Create tasks of different kinds
	_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      id,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: "test-kind-followup-" + contactID,
		State:          "managed",
		Metadata:       map[string]any{"content": "Follow up: Test", "due_date": "2026-04-10"},
	})
	require.NoError(t, err)

	_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      id,
		Provider:       "todoist",
		Kind:           "action",
		ExternalTaskID: "test-kind-action-" + contactID,
		State:          "managed",
		Metadata:       map[string]any{"content": "Action task"},
	})
	require.NoError(t, err)

	t.Run("FilterByFollowUp", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/tasks?kind=follow_up", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		items := resp.Data.([]interface{})
		for _, item := range items {
			task := item.(map[string]interface{})
			assert.Equal(t, "follow_up", task["kind"], "should only return follow_up tasks")
		}
	})

	t.Run("FilterByAction_ExcludesFollowUp", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/tasks?kind=action", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		items := resp.Data.([]interface{})
		for _, item := range items {
			task := item.(map[string]interface{})
			assert.Equal(t, "action", task["kind"], "should only return action tasks")
		}
	})

	t.Run("InvalidKindRejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID+"/tasks?kind=invalid", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
