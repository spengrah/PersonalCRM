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
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupInteractionTestRouter(t *testing.T) (*gin.Engine, func()) {
	t.Helper()

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

	cfg := &config.Config{River: config.RiverConfig{WorkerConcurrency: 1}}
	manualHandler, contactService := mustBuildManualHandlerForTest(t, ctx, database, cfg)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contentService := service.NewInteractionContentService(interactionRepo, repository.NewCommsMessageRepository(database.Queries), repository.NewTelegramMessageRepository(database.Queries), repository.NewMessagesMessageRepository(database.Queries), repository.NewMeetingNoteRepository(database.Queries), repository.NewCalendarEventRepository(database.Queries), repository.NewPhoneCallRepository(database.Queries), repository.NewContactRepository(database.Queries))
	contactHandler := handlers.NewContactHandler(contactService)
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, manualHandler, contentService)

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
			contacts.GET("/:id/interactions", interactionHandler.ListContactInteractions)
			contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
		}
		interactions := v1.Group("/interactions")
		{
			interactions.GET("/:id/content", interactionHandler.GetInteractionContent)
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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Interaction Test User "+t.Name()+"-"+uuid.NewString()[:8])
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

		data := response.Data.(map[string]interface{})
		items := data["items"].([]interface{})
		assert.GreaterOrEqual(t, len(items), 2) // At least 2 from the creates above

		// LIST rows carry the same wire shape as the create echo —
		// description, direction, and source. Today both paths share
		// interactionToResponse, so this pins the contract against the
		// mapping ever forking.
		var coffeeRow map[string]interface{}
		for _, item := range items {
			row := item.(map[string]interface{})
			assert.NotEmpty(t, row["source"], "list row should carry source")
			assert.Contains(t, []interface{}{"outbound", "inbound", "mutual"}, row["direction"],
				"list row should carry a valid direction")
			if row["description"] == "Had coffee together" {
				coffeeRow = row
			}
		}
		require.NotNil(t, coffeeRow, "created interaction should appear in the list with its description")
		assert.Equal(t, "manual", coffeeRow["source"])
		assert.Equal(t, "mutual", coffeeRow["direction"]) // create passed no direction; manual default is mutual

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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Dedup Test User "+t.Name()+"-"+uuid.NewString()[:8])
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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Delete Interaction Test User "+t.Name()+"-"+uuid.NewString()[:8])
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
		data := listResp.Data.(map[string]interface{})
		items := data["items"].([]interface{})
		for _, item := range items {
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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Last Contacted Update Test "+t.Name()+"-"+uuid.NewString()[:8])
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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	// Use a non-nil UUID that definitely won't exist. The zero UUID
	// (00000000...) would trip the consumer's "contact_id unresolved"
	// publisher-bug guard (plan Decision 4) before reaching the DB check;
	// we specifically want the DB-level "not found" path here.
	nonExistentID := "550e8400-e29b-41d4-a716-446655440099"

	t.Run("CreateInteractionForNonExistentContact", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+nonExistentID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("ListInteractionsForNonExistentContact", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+nonExistentID+"/interactions", nil)
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

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	contactID := createInteractionTestContact(t, router, "Future Date Test User "+t.Name()+"-"+uuid.NewString()[:8])
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

// TestInteraction_ListPaginatesAWholeHistory is FIRST-ORDER product coverage of
// the interaction list's pagination read path: a history long enough to span
// several pages, walked page by page.
//
// It is NOT the fixture test that used to assert the `long-history` adversarial
// edge produced 48 rows — that one checked the SEED and was deleted. This one
// checks the API: whether the handler reports a truthful total and page count and
// serves every row exactly once. Nothing else covers it. TestInteraction_CreateAndList
// asserts only `Total >= 2` on an unpaginated read, which is a one-sided bound —
// it keeps passing if later pages vanish or the total is wrong.
func TestInteraction_ListPaginatesAWholeHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()

	// The subject's identifiers come from the synthetic factory rather than a
	// hand-rolled literal, and its namespace is unique to this test so the shared
	// package DB cannot collide.
	gen := factory.NewGenerator(factory.DefaultSeed, "i"+uuid.NewString()[:8])
	contactID := createInteractionTestContact(t, router, gen.Contact().FullName)
	defer deleteInteractionTestContact(t, router, contactID)

	const (
		history   = 48
		pageSize  = 20
		wantPages = 3 // 48 rows at 20 per page
	)

	// Manual interactions dedupe inside a 30-minute window, so the rows are spaced
	// an hour apart: 48 DISTINCT interactions rather than one created 48 times.
	base := accelerated.GetCurrentTime()
	for i := 1; i <= history; i++ {
		body, _ := json.Marshal(map[string]string{
			"occurred_at": base.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
			"description": fmt.Sprintf("history row %02d", i),
		})
		req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, "seeding row %d: %s", i, w.Body.String())
	}

	// Page 1 is FULL and the metadata describes the whole history, not the page.
	first := listInteractionPage(t, router, contactID, pageSize, 1)
	assert.Len(t, first.rows, pageSize, "page 1 must be full")
	assert.Equal(t, int64(history), first.total, "the read must report the WHOLE history, not the page")
	assert.Equal(t, wantPages, first.pages, "%d rows at %d per page is %d pages", history, pageSize, wantPages)

	// And walking every page really does yield the whole history exactly once — a
	// dropped last page or a row served on two pages both fail here.
	seen := map[string]bool{}
	for page := 1; page <= first.pages; page++ {
		got := listInteractionPage(t, router, contactID, pageSize, page)
		assert.NotEmpty(t, got.rows, "page %d must not be empty", page)
		for _, id := range got.rows {
			assert.False(t, seen[id], "interaction %s appears on more than one page", id)
			seen[id] = true
		}
	}
	assert.Len(t, seen, history, "walking the pages must yield every interaction exactly once")
}

// interactionPage is one page of the interaction list plus its paging metadata.
type interactionPage struct {
	rows  []string
	total int64
	pages int
}

func listInteractionPage(t *testing.T, router *gin.Engine, contactID string, limit, page int) interactionPage {
	t.Helper()
	path := fmt.Sprintf("/api/v1/contacts/%s/interactions?limit=%d&page=%d", contactID, limit, page)
	req, _ := http.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "GET %s: %s", path, w.Body.String())

	var response api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotNil(t, response.Meta, "the interaction list must carry meta")
	require.NotNil(t, response.Meta.Pagination, "the interaction list must carry pagination meta")

	data, ok := response.Data.(map[string]interface{})
	require.True(t, ok, "data must be an object: %s", w.Body.String())
	rows, ok := data["items"].([]interface{})
	require.True(t, ok, "data.items must be an array: %s", w.Body.String())
	ids := make([]string, 0, len(rows))
	for _, item := range rows {
		row, isObject := item.(map[string]interface{})
		require.True(t, isObject)
		id, hasID := row["id"].(string)
		require.True(t, hasID, "every interaction row must carry an id")
		ids = append(ids, id)
	}
	return interactionPage{
		rows:  ids,
		total: response.Meta.Pagination.Total,
		pages: response.Meta.Pagination.Pages,
	}
}
