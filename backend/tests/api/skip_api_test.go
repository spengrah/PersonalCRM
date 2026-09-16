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
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setupSkipAPIRouter(t *testing.T) (*gin.Engine, *repository.ContactRepository, func()) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	database, err := db.NewDatabase(ctx, config.DatabaseConfig{
		URL: os.Getenv("DATABASE_URL"), MaxConns: 8, MinConns: 1,
		MaxConnIdleTime: config.DefaultDBMaxConnIdleTime, MaxConnLifetime: config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	})
	require.NoError(t, err)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	cfg := &config.Config{River: config.RiverConfig{WorkerConcurrency: 1}}
	manualHandler, contactService := mustBuildManualHandlerForTest(t, ctx, database, cfg)
	contactHandler := handlers.NewContactHandler(contactService)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	contacts := router.Group("/api/v1/contacts")
	contacts.POST("", contactHandler.CreateContact)
	contacts.GET("/:id", contactHandler.GetContact)
	contacts.POST("/:id/skip", contactHandler.SkipCycle)
	contacts.DELETE("/:id/skip", contactHandler.UndoSkip)
	_ = manualHandler
	return router, contactRepo, database.Close
}

func TestContactAPI_SkipAndUndo(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	router, contactRepo, cleanup := setupSkipAPIRouter(t)
	defer cleanup()

	cadenceName := "monthly"
	body, err := json.Marshal(handlers.CreateContactRequest{FullName: "Skip API Contact", Cadence: &cadenceName})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusCreated, resp.Code, resp.Body.String())
	var created api.APIResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &created))
	createdData := created.Data.(map[string]interface{})
	id, err := uuid.Parse(createdData["id"].(string))
	require.NoError(t, err)
	today := cadence.Today(accelerated.GetCurrentTime())
	seeded := time.Date(today.Year(), today.Month(), today.Day()-10, 0, 0, 0, 0, time.UTC)
	require.NoError(t, contactRepo.TestSeedContactCadenceFields(context.Background(), id, repository.TestCadenceSeed{ContactBy: &seeded}))

	t.Run("skip advances one cycle and reports undo", func(t *testing.T) {
		capturedAt := accelerated.GetCurrentTime()
		response := doSkipRequest(t, router, http.MethodPost, id)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		data := responseData(t, response)
		want := cadence.NextAfterSkip(&seeded, cadence.CadenceMonthly, capturedAt)
		require.Equal(t, cadence.CalendarDate(want), dateFromData(t, data["contact_by"]))
		require.Equal(t, cadence.CalendarDate(seeded), dateFromData(t, data["last_skipped_contact_by"]))
		require.True(t, data["undo_skip_available"].(bool))
		require.False(t, data["awaiting_reply"].(bool))
		require.NotNil(t, data["last_skipped_at"])
		gotAt, err := time.Parse(time.RFC3339Nano, data["last_skipped_at"].(string))
		require.NoError(t, err)
		delta := gotAt.Sub(capturedAt)
		if delta < 0 {
			delta = -delta
		}
		require.LessOrEqual(t, delta, time.Minute)

		getResp := doSkipRequest(t, router, http.MethodGet, id)
		require.Equal(t, http.StatusOK, getResp.Code)
		getData := responseData(t, getResp)
		for _, field := range []string{
			"contact_by",
			"awaiting_reply",
			"awaiting_reply_until",
			"last_skipped_at",
			"last_skipped_contact_by",
			"undo_skip_available",
		} {
			require.Equal(t, data[field], getData[field], "field %s", field)
		}
	})

	t.Run("undo restores the past date exactly", func(t *testing.T) {
		response := doSkipRequest(t, router, http.MethodDelete, id)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		data := responseData(t, response)
		require.Equal(t, cadence.CalendarDate(seeded), dateFromData(t, data["contact_by"]))
		require.False(t, data["undo_skip_available"].(bool))
		require.NotContains(t, data, "last_skipped_at")
		require.NotContains(t, data, "last_skipped_contact_by")
	})

	t.Run("undo without a skip conflicts", func(t *testing.T) {
		response := doSkipRequest(t, router, http.MethodDelete, id)
		require.Equal(t, http.StatusConflict, response.Code)
		var envelope api.APIResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		require.NotNil(t, envelope.Error)
		require.Equal(t, api.ErrCodeConflict, envelope.Error.Code)
	})

	t.Run("skip without a cadence conflicts", func(t *testing.T) {
		noCadenceBody, _ := json.Marshal(handlers.CreateContactRequest{FullName: "Skip API No Cadence"})
		create := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewReader(noCadenceBody))
		create.Header.Set("Content-Type", "application/json")
		createdResp := httptest.NewRecorder()
		router.ServeHTTP(createdResp, create)
		require.Equal(t, http.StatusCreated, createdResp.Code)
		var envelope api.APIResponse
		require.NoError(t, json.Unmarshal(createdResp.Body.Bytes(), &envelope))
		noCadenceID, err := uuid.Parse(envelope.Data.(map[string]interface{})["id"].(string))
		require.NoError(t, err)
		require.Equal(t, http.StatusConflict, doSkipRequest(t, router, http.MethodPost, noCadenceID).Code)
	})

	t.Run("unknown contact", func(t *testing.T) {
		unknown := uuid.New()
		require.Equal(t, http.StatusNotFound, doSkipRequest(t, router, http.MethodPost, unknown).Code)
		require.Equal(t, http.StatusNotFound, doSkipRequest(t, router, http.MethodDelete, unknown).Code)
	})
	t.Run("malformed id", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			req := httptest.NewRequest(method, "/api/v1/contacts/not-a-uuid/skip", nil)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)
		}
	})
}

func doSkipRequest(t *testing.T, router http.Handler, method string, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/contacts/"+id.String()+map[string]string{http.MethodPost: "/skip", http.MethodDelete: "/skip", http.MethodGet: ""}[method], nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func responseData(t *testing.T, response *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var envelope api.APIResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	return envelope.Data.(map[string]interface{})
}

func dateFromData(t *testing.T, value interface{}) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value.(string))
	require.NoError(t, err)
	return parsed
}
