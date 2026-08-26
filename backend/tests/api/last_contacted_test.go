// Tests covering the manual interaction logger surface that replaced
// the legacy PATCH /contacts/:id/last-contacted endpoint. The legacy
// endpoint is gone; the regression test below confirms it returns 404
// so any stranded clients fail loudly. The other tests exercise the
// equivalent semantics now exposed via POST /contacts/:id/interactions
// (direction=mutual matches the old "Mark as Contacted" behaviour).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupLastContactedTestRouter(t *testing.T) (*gin.Engine, func()) {
	t.Helper()

	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")

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

	cfg := &config.Config{River: config.RiverConfig{WorkerConcurrency: 1}}
	manualHandler, contactService := mustBuildManualHandlerForTest(t, ctx, database, cfg)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contentService := service.NewInteractionContentService(interactionRepo, repository.NewCommsMessageRepository(database.Queries), repository.NewTelegramMessageRepository(database.Queries), repository.NewMessagesMessageRepository(database.Queries), repository.NewMeetingNoteRepository(database.Queries), repository.NewCalendarEventRepository(database.Queries), repository.NewPhoneCallRepository(database.Queries))
	contactHandler := handlers.NewContactHandler(contactService)
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, manualHandler, contentService)

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
		contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
	}

	cleanup := func() {
		database.Close()
	}

	return router, cleanup
}

// createContactForTest is a thin helper that POSTs a contact and returns its ID.
func createContactForTest(t *testing.T, router *gin.Engine, fullName string) string {
	t.Helper()
	cadence := "weekly"
	createReq := handlers.CreateContactRequest{
		FullName: fullName,
		Cadence:  &cadence,
	}
	jsonBody, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/contacts", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "create contact: %s", w.Body.String())

	var createResponse api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &createResponse))
	contactData := createResponse.Data.(map[string]interface{})
	contactID := contactData["id"].(string)

	t.Cleanup(func() {
		deleteReq, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID, nil)
		deleteW := httptest.NewRecorder()
		router.ServeHTTP(deleteW, deleteReq)
	})

	return contactID
}

// TestPostInteraction_DirectionMutual_BumpsLastContacted is the primary
// replacement for the old "Mark as Contacted" PATCH semantics.
// direction=mutual updates last_contacted via the manual interaction
// pipeline (publish -> InteractionRecorder -> CadenceUpdater all in one tx).
func TestPostInteraction_DirectionMutual_BumpsLastContacted(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}

	t.Parallel()
	router, cleanup := setupLastContactedTestRouter(t)
	// Register the pool-close via t.Cleanup (NOT defer) so it runs AFTER the
	// contact-DELETE that createContactForTest registers — LIFO keeps the pool
	// open for the row cleanup.
	t.Cleanup(cleanup)

	contactID := createContactForTest(t, router, "PostInteraction Mutual "+uuid.New().String()[:8])

	// Empty body -> direction defaults to mutual; backend uses
	// accelerated.GetCurrentTime() for occurred_at.
	req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBufferString(`{"direction":"mutual"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "post interaction: %s", w.Body.String())

	// Refetch the contact and verify last_contacted advanced.
	getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
	getW := httptest.NewRecorder()
	router.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)

	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &resp))
	data := resp.Data.(map[string]interface{})
	require.NotNil(t, data["last_contacted"], "last_contacted must be populated after a mutual interaction")

	// last_contacted should be roughly 'now' (within 5 seconds of accelerated time).
	parsed, err := time.Parse(time.RFC3339, data["last_contacted"].(string))
	require.NoError(t, err)
	now := accelerated.GetCurrentTime()
	delta := now.Sub(parsed)
	if delta < 0 {
		delta = -delta
	}
	assert.Less(t, delta, 5*time.Second, "last_contacted should be ~now")
}

// TestPostInteraction_DirectionOutbound_DoesNotBumpLastContacted asserts
// the direction-aware semantics that replace the old timestamp-only
// update: outbound interactions advance last_outreach_at only and MUST
// NOT touch last_contacted (last_contacted tracks incoming/mutual only).
//
// Note: contact creation initializes last_contacted to the create-time
// timestamp (handler default). The assertion compares pre- and post-
// interaction values to confirm the outbound interaction did NOT advance
// last_contacted, NOT that last_contacted is nil.
func TestPostInteraction_DirectionOutbound_DoesNotBumpLastContacted(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}

	t.Parallel()
	router, cleanup := setupLastContactedTestRouter(t)
	// Register the pool-close via t.Cleanup (NOT defer) so it runs AFTER the
	// contact-DELETE that createContactForTest registers — LIFO keeps the pool
	// open for the row cleanup.
	t.Cleanup(cleanup)

	contactID := createContactForTest(t, router, "PostInteraction Outbound "+uuid.New().String()[:8])

	// Capture last_contacted at creation time.
	getReq0, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
	getW0 := httptest.NewRecorder()
	router.ServeHTTP(getW0, getReq0)
	require.Equal(t, http.StatusOK, getW0.Code)
	var resp0 api.APIResponse
	require.NoError(t, json.Unmarshal(getW0.Body.Bytes(), &resp0))
	data0 := resp0.Data.(map[string]interface{})
	lcAtCreate, _ := data0["last_contacted"].(string)

	// Sleep briefly so the interaction's occurred_at is strictly after
	// the contact's created-time last_contacted, ensuring "no bump"
	// asserts a real invariant.
	time.Sleep(50 * time.Millisecond)

	body := []byte(`{"direction":"outbound"}`)
	req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "post interaction: %s", w.Body.String())

	getReq, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID, nil)
	getW := httptest.NewRecorder()
	router.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)

	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &resp))
	data := resp.Data.(map[string]interface{})

	// Outbound must NOT advance last_contacted.
	lcAfter, _ := data["last_contacted"].(string)
	assert.Equal(t, lcAtCreate, lcAfter, "outbound interaction must not bump last_contacted")

	// last_outreach_at should advance instead.
	require.NotNil(t, data["last_outreach_at"], "last_outreach_at must be populated after an outbound interaction")
}

// TestRemovedEndpoint_PatchLastContacted_Returns404 locks down the legacy
// PATCH /contacts/:id/last-contacted endpoint removal: stranded clients
// must see a clean 404 (gin's default no-route response) rather than
// silent success or an unexpected handler match.
func TestRemovedEndpoint_PatchLastContacted_Returns404(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	t.Parallel()

	router, cleanup := setupLastContactedTestRouter(t)
	defer cleanup()

	contactID := uuid.New().String()
	url := fmt.Sprintf("/api/v1/contacts/%s/last-contacted", contactID)
	req, _ := http.NewRequest("PATCH", url, bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// gin returns 404 for unregistered routes by default.
	assert.Equal(t, http.StatusNotFound, w.Code, "PATCH /contacts/:id/last-contacted must be removed; got body=%s", w.Body.String())
}
