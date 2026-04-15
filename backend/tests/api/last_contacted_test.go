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

func setupLastContactedTestRouter() (*gin.Engine, func()) {
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
	contactHandler := handlers.NewContactHandler(contactService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", contactHandler.CreateContact)
		contacts.GET("/:id", contactHandler.GetContact)
		contacts.DELETE("/:id", contactHandler.DeleteContact)
		contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

func TestUpdateLastContacted_WithPastDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupLastContactedTestRouter()
	defer cleanup()

	// Create a test contact
	createReq := handlers.CreateContactRequest{
		FullName: "Last Contacted Test User",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "lastcontacted@example.com",
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

	t.Run("UpdateLastContacted_WithPastDate_Success", func(t *testing.T) {
		pastDate := "2024-01-15"
		updateReq := handlers.UpdateLastContactedRequest{
			LastContacted: &handlers.DateOnly{},
		}
		// Parse and set the date
		parsedTime, _ := time.Parse("2006-01-02", pastDate)
		updateReq.LastContacted.Time = &parsedTime

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		// Verify the last_contacted was updated
		data := response.Data.(map[string]interface{})
		lastContacted := data["last_contacted"].(string)
		assert.Contains(t, lastContacted, "2024-01-15")
	})

	t.Run("UpdateLastContacted_WithTodayDate_Success", func(t *testing.T) {
		today := accelerated.GetCurrentTime().Format("2006-01-02")
		updateReq := handlers.UpdateLastContactedRequest{
			LastContacted: &handlers.DateOnly{},
		}
		parsedTime, _ := time.Parse("2006-01-02", today)
		updateReq.LastContacted.Time = &parsedTime

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})
}

func TestUpdateLastContacted_WithFutureDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupLastContactedTestRouter()
	defer cleanup()

	// Create a test contact
	createReq := handlers.CreateContactRequest{
		FullName: "Future Date Test User",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "futuredate@example.com",
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
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	}()

	t.Run("UpdateLastContacted_WithFutureDate_Fails", func(t *testing.T) {
		futureDate := accelerated.GetCurrentTime().AddDate(0, 0, 7).Format("2006-01-02")
		updateReq := handlers.UpdateLastContactedRequest{
			LastContacted: &handlers.DateOnly{},
		}
		parsedTime, _ := time.Parse("2006-01-02", futureDate)
		updateReq.LastContacted.Time = &parsedTime

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.False(t, response.Success)
		assert.Equal(t, "VALIDATION_ERROR", response.Error.Code)
		assert.Contains(t, response.Error.Details, "future")
	})
}

func TestUpdateLastContacted_WithoutDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupLastContactedTestRouter()
	defer cleanup()

	// Create a test contact
	createReq := handlers.CreateContactRequest{
		FullName: "No Date Test User",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "nodate@example.com",
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
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	}()

	t.Run("UpdateLastContacted_EmptyBody_UsesCurrentTime", func(t *testing.T) {
		// Send empty body - should use current time
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", nil)
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		// Verify the last_contacted was updated to approximately now
		data := response.Data.(map[string]interface{})
		lastContacted := data["last_contacted"].(string)
		parsedTime, _ := time.Parse(time.RFC3339, lastContacted)
		now := accelerated.GetCurrentTime()
		// Should be within 1 minute of now
		assert.WithinDuration(t, now, parsedTime, time.Minute)
	})

	t.Run("UpdateLastContacted_EmptyJSON_UsesCurrentTime", func(t *testing.T) {
		// Send empty JSON object - should use current time
		jsonBody, _ := json.Marshal(map[string]interface{}{})
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)
	})
}

func TestUpdateLastContacted_NonExistentContact(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupLastContactedTestRouter()
	defer cleanup()

	t.Run("UpdateLastContacted_NonExistentContact_Returns404", func(t *testing.T) {
		nonExistentID := "00000000-0000-0000-0000-000000000000"
		pastDate := "2024-01-15"
		updateReq := handlers.UpdateLastContactedRequest{
			LastContacted: &handlers.DateOnly{},
		}
		parsedTime, _ := time.Parse("2006-01-02", pastDate)
		updateReq.LastContacted.Time = &parsedTime

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+nonExistentID+"/last-contacted", bytes.NewBuffer(jsonBody))
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

	t.Run("UpdateLastContacted_InvalidUUID_Returns400", func(t *testing.T) {
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/not-a-uuid/last-contacted", nil)
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

func TestUpdateLastContacted_PreservesOtherContactData(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	router, cleanup := setupLastContactedTestRouter()
	defer cleanup()

	// Create a test contact with various fields
	location := "San Francisco"
	cadence := "monthly"
	createReq := handlers.CreateContactRequest{
		FullName: "Data Preservation Test",
		Methods: []handlers.ContactMethodRequest{
			{
				Type:  "email",
				Value: "preservation@example.com",
			},
		},
		Location: &location,
		Cadence:  &cadence,
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
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	}()

	t.Run("UpdateLastContacted_PreservesOtherFields", func(t *testing.T) {
		pastDate := "2024-06-15"
		updateReq := handlers.UpdateLastContactedRequest{
			LastContacted: &handlers.DateOnly{},
		}
		parsedTime, _ := time.Parse("2006-01-02", pastDate)
		updateReq.LastContacted.Time = &parsedTime

		jsonBody, _ := json.Marshal(updateReq)
		req, _ := http.NewRequest("PATCH", "/api/v1/contacts/"+contactID+"/last-contacted", bytes.NewBuffer(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response api.APIResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.True(t, response.Success)

		// Verify all fields are preserved
		data := response.Data.(map[string]interface{})
		assert.Equal(t, "Data Preservation Test", data["full_name"])
		assert.Equal(t, "San Francisco", data["location"])
		// Note: notes are now stored in a separate note table, not on the contact
		assert.Equal(t, "monthly", data["cadence"])
		assert.Contains(t, data["last_contacted"].(string), "2024-06-15")
	})
}
