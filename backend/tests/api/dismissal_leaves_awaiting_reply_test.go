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
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupAwaitingReplyAPIRouter(t *testing.T) (*gin.Engine, *repository.ContactRepository, *repository.ContactTaskRepository, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	database, err := db.NewDatabase(ctx, config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	})
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contentService := service.NewInteractionContentService(
		interactionRepo,
		repository.NewCommsMessageRepository(database.Queries),
		repository.NewTelegramMessageRepository(database.Queries),
		repository.NewMessagesMessageRepository(database.Queries),
		repository.NewMeetingNoteRepository(database.Queries),
		repository.NewCalendarEventRepository(database.Queries),
		repository.NewPhoneCallRepository(database.Queries),
		repository.NewContactRepository(database.Queries),
	)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	cfg := &config.Config{River: config.RiverConfig{WorkerConcurrency: 1}}
	manualHandler, contactService := mustBuildManualHandlerForTest(t, ctx, database, cfg)
	contactHandler := handlers.NewContactHandler(contactService)
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, manualHandler, contentService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	contacts := router.Group("/api/v1/contacts")
	contacts.POST("", contactHandler.CreateContact)
	contacts.GET("/overdue", contactHandler.ListOverdueContacts)
	contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)

	return router, contactRepo, contactTaskRepo, database.Close
}

// spec: CAD-023.each-entry-carries-pending-followup
func TestOverdueAPI_DismissedFollowUpLeavesContactAwaitingReply(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, contactRepo, contactTaskRepo, cleanup := setupAwaitingReplyAPIRouter(t)
	defer cleanup()

	cadenceName := "monthly"
	name := "Awaiting Reply API Dismissal " + uuid.NewString()
	body, err := json.Marshal(handlers.CreateContactRequest{FullName: name, Cadence: &cadenceName})
	require.NoError(t, err)
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewReader(body))
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, createRequest)
	require.Equal(t, http.StatusCreated, createResponse.Code)

	var created api.APIResponse
	require.NoError(t, json.Unmarshal(createResponse.Body.Bytes(), &created))
	contactData := created.Data.(map[string]interface{})
	contactID, err := uuid.Parse(contactData["id"].(string))
	require.NoError(t, err)

	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	contactBy := now.AddDate(0, 0, -10)
	require.NoError(t, contactRepo.TestSeedContactCadenceFields(context.Background(), contactID, repository.TestCadenceSeed{
		LastContacted:     &old,
		LastInteractionAt: &old,
		LastResponseAt:    &old,
		ContactBy:         &contactBy,
	}))

	interactionBody, err := json.Marshal(map[string]string{
		"direction":   "outbound",
		"occurred_at": now.Add(-time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	interactionRequest := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/"+contactID.String()+"/interactions", bytes.NewReader(interactionBody))
	interactionRequest.Header.Set("Content-Type", "application/json")
	interactionResponse := httptest.NewRecorder()
	router.ServeHTTP(interactionResponse, interactionRequest)
	require.Equal(t, http.StatusCreated, interactionResponse.Code)

	task, err := contactTaskRepo.CreateContactTask(context.Background(), repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       "todoist",
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
		ExternalTaskID: "dismissed-awaiting-" + contactID.String(),
		State:          string(repository.ContactTaskStateManaged),
	})
	require.NoError(t, err)
	_, err = contactTaskRepo.UpdateContactTaskState(context.Background(), task.ID, repository.ContactTaskStateDismissed)
	require.NoError(t, err)

	overdueRequest := httptest.NewRequest(http.MethodGet, "/api/v1/contacts/overdue", nil)
	overdueResponse := httptest.NewRecorder()
	router.ServeHTTP(overdueResponse, overdueRequest)
	require.Equal(t, http.StatusOK, overdueResponse.Code)

	var overdue api.APIResponse
	require.NoError(t, json.Unmarshal(overdueResponse.Body.Bytes(), &overdue))
	entries := overdue.Data.([]interface{})
	var entry map[string]interface{}
	for _, candidate := range entries {
		item := candidate.(map[string]interface{})
		if item["id"] == contactID.String() {
			entry = item
			break
		}
	}
	require.NotNil(t, entry, "overdue response includes the contact")
	value, ok := entry["awaiting_reply"]
	if !ok {
		value, ok = entry["has_pending_followup"]
	}
	require.True(t, ok, "overdue entry carries neither awaiting_reply nor has_pending_followup")
	assert.Equal(t, true, value)
}
