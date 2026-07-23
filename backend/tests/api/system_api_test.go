package api

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSystemTestDatabase opens a DB handle exactly like production wiring does
// (pool config mirrors config.TestConfig() 8/1, same as the sibling
// setupContactValidationTestRouter). Migrations are applied once by TestMain.
func newSystemTestDatabase(t *testing.T) *db.Database {
	t.Helper()

	dbConfig := config.DatabaseConfig{
		URL:               os.Getenv("DATABASE_URL"),
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(context.Background(), dbConfig)
	require.NoError(t, err, "failed to connect to test database")
	return database
}

// setupSystemExportTestRouter wires SystemHandler the way production does
// (RegisterDataExchangeRoutes on the /api/v1 group, a concrete
// *repository.ContactRepository), against the given database handle.
func setupSystemExportTestRouter(database *db.Database) *gin.Engine {
	contactRepo := repository.NewContactRepository(database.Queries)
	systemHandler := handlers.NewSystemHandler(contactRepo, config.RuntimeConfig{CRMEnvironment: "test"})

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	corsConfig := config.CORSConfig{AllowAll: true}
	router.Use(api.CORSMiddleware(corsConfig))

	v1 := router.Group("/api/v1")
	handlers.RegisterDataExchangeRoutes(v1, systemHandler)

	return router
}

// collectKeys recursively gathers every object key present anywhere in a
// decoded JSON value, so absence assertions cover the whole envelope rather
// than one level of it.
func collectKeys(value interface{}, into map[string]bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, child := range v {
			into[key] = true
			collectKeys(child, into)
		}
	case []interface{}:
		for _, child := range v {
			collectKeys(child, into)
		}
	}
}

func TestSystemAPI_ExportData(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	database := newSystemTestDatabase(t)
	defer database.Close()
	router := setupSystemExportTestRouter(database)

	ctx := context.Background()
	contactRepo := repository.NewContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)

	// Namespaced fixture: a contact that ALSO has an interaction and a note,
	// so the contacts-only assertion is exercised against a dataset where
	// interactions/notes genuinely exist and could leak into the export.
	ns := uuid.New().String()[:8]
	contactName := "System Export " + ns
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: contactName,
	})
	require.NoError(t, err)
	defer func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	}()

	sourceRef := "system-export-test-" + ns
	description := "export fixture interaction " + ns
	_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:   contact.ID,
		Source:      repository.InteractionSourceManual,
		SourceRef:   &sourceRef,
		OccurredAt:  accelerated.GetCurrentTime(),
		Description: &description,
		Direction:   repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)

	_, err = noteRepo.CreateNotepad(ctx, contact.ID, "export fixture note "+ns)
	require.NoError(t, err)

	exportBody := func(t *testing.T) (*httptest.ResponseRecorder, map[string]interface{}) {
		t.Helper()
		req, _ := http.NewRequest("POST", "/api/v1/export", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return w, body
	}

	t.Run("Export_IsJSONAttachmentWrappingEnvelope", func(t *testing.T) {
		// spec: SET-030[0]
		w, body := exportBody(t)

		assert.Equal(t, "attachment; filename=crm_export.json", w.Header().Get("Content-Disposition"))
		assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

		// The download wraps an export envelope: literal wire keys on the
		// decoded body, not a DTO round-trip.
		assert.Equal(t, true, body["success"])
		envelope, ok := body["data"].(map[string]interface{})
		require.True(t, ok, "body.data must be the export envelope object, got: %T", body["data"])
		_, hasDataset := envelope["data"].(map[string]interface{})
		assert.True(t, hasDataset, "the envelope must wrap a data object")
	})

	t.Run("Export_EnvelopeCarriesVersionAndTimestamp", func(t *testing.T) {
		// spec: SET-030[1]
		_, body := exportBody(t)

		envelope, ok := body["data"].(map[string]interface{})
		require.True(t, ok)

		assert.Equal(t, "1.0", envelope["version"], "envelope must carry the literal version tag")

		exportedAt, ok := envelope["exported_at"].(string)
		require.True(t, ok, "envelope must carry exported_at as a string, got: %T", envelope["exported_at"])
		assert.NotEmpty(t, exportedAt)
	})

	t.Run("Export_ContactsOnlyNoInteractionsOrNotes", func(t *testing.T) {
		// spec: SET-030[2]
		_, body := exportBody(t)

		envelope, ok := body["data"].(map[string]interface{})
		require.True(t, ok)
		dataset, ok := envelope["data"].(map[string]interface{})
		require.True(t, ok)

		contacts, ok := dataset["contacts"].([]interface{})
		require.True(t, ok, "dataset must carry a contacts array, got: %T", dataset["contacts"])

		var found bool
		for _, row := range contacts {
			c, ok := row.(map[string]interface{})
			if !ok {
				continue
			}
			if c["id"] == contact.ID.String() {
				found = true
				assert.Equal(t, contactName, c["full_name"])
			}
		}
		assert.True(t, found, fmt.Sprintf("seeded contact %s must appear in the exported contacts", contact.ID))

		// The fixture contact HAS an interaction and a note in the DB; neither
		// may surface anywhere in the export envelope — assert over the full
		// recursive key set of the decoded body.
		keys := map[string]bool{}
		collectKeys(body, keys)
		assert.False(t, keys["interactions"], "export must not contain an interactions key anywhere")
		assert.False(t, keys["notes"], "export must not contain a notes key anywhere")
	})

	t.Run("Export_ContactsReadFailureReturns500", func(t *testing.T) {
		// spec: SET-030[3]
		// The repository has no injection seam, so force a genuine read
		// failure: a handler whose repository sits on an already-closed pool.
		failingDB := newSystemTestDatabase(t)
		failingRouter := setupSystemExportTestRouter(failingDB)
		failingDB.Close()

		req, _ := http.NewRequest("POST", "/api/v1/export", nil)
		w := httptest.NewRecorder()
		failingRouter.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())

		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, false, body["success"])
		errObj, ok := body["error"].(map[string]interface{})
		require.True(t, ok, "error must be an object, got: %T", body["error"])
		assert.Equal(t, "DATABASE_ERROR", errObj["code"])
		assert.Equal(t, "Failed to fetch contacts", errObj["message"])
	})
}
