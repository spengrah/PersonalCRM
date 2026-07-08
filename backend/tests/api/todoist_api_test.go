//go:build integration_testdb

// Package api's todoist_api_test.go covers the hermetic (DB-only) surface of
// TodoistHandler: GetSettings and UpdateSettings, which read/write only
// through oauthService.ListAccounts + syncRepo — no external HTTP.
//
// ListProjects/ListLabels (SET-018) are deliberately NOT covered here: they
// construct todoist.NewSyncClient(accessToken) inline with no injection
// seam, so their live-provider contract can't be exercised without a real
// Todoist account or a handler client seam. See the arc's tracked
// follow-up: inject the existing todoist.ClientFactory into TodoistHandler.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTodoistAPITest builds a fresh, empty isolated-River DB clone (DT3),
// wires the production Todoist settings route surface over it, and returns a
// seedAccount closure that upserts a connected-account credential on demand.
// Every subtest MUST call this as its first line: TodoistOAuthService.
// ListAccounts lists ALL todoist credentials with no filter, so a shared
// clone would let one subtest's seeded account leak into a sibling
// "NoAccount" subtest, especially under -shuffle=on.
func newTodoistAPITest(t *testing.T) (router *gin.Engine, syncRepo *repository.SyncRepository, oauthSvc *todoist.OAuthService, seedAccount func() string) {
	t.Helper()
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	oauthRepo := repository.NewOAuthRepository(database.Queries)
	syncRepo = repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	var err error
	oauthSvc, err = todoist.NewOAuthService(cfg, oauthRepo, syncRepo)
	require.NoError(t, err)

	handler := handlers.NewTodoistHandler(oauthSvc, syncRepo)
	router = gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterTodoistRoutes(v1, handler)

	seedAccount = func() string {
		t.Helper()
		accountID := "todoist-acct-" + uuid.NewString()[:8]
		// access_token_encrypted / encryption_nonce are NOT NULL columns,
		// but GetSettings/UpdateSettings never decrypt them (DT2) — dummy
		// non-nil bytes satisfy the constraint without a real token.
		_, err := oauthRepo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             todoist.ProviderName,
			AccountID:            accountID,
			AccessTokenEncrypted: []byte("test"),
			EncryptionNonce:      []byte("test"),
			TokenType:            "Bearer",
		})
		require.NoError(t, err)
		return accountID
	}

	return router, syncRepo, oauthSvc, seedAccount
}

// todoistSettingsResponse mirrors handlers.TodoistSettingsResponse for JSON
// unwrapping.
type todoistSettingsResponse struct {
	ProjectID             *string `json:"project_id,omitempty"`
	LabelID               *string `json:"label_id,omitempty"`
	LabelName             *string `json:"label_name,omitempty"`
	IntegrationInstanceID *string `json:"integration_instance_id,omitempty"`
	UserTimezone          *string `json:"user_timezone,omitempty"`
}

// doTodoistRequest serves an HTTP request against the router, JSON-encoding
// body when non-nil, and returns the recorder.
func doTodoistRequest(router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	router.ServeHTTP(w, req)
	return w
}

// decodeTodoistSettings unwraps api.APIResponse and returns the settings
// payload.
func decodeTodoistSettings(t *testing.T, w *httptest.ResponseRecorder) todoistSettingsResponse {
	t.Helper()
	var envelope struct {
		Success bool                    `json:"success"`
		Data    todoistSettingsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	return envelope.Data
}

// TestTodoistAPI_GetSettings proves the read side of the hermetic Todoist
// settings surface: no account is not-found, an account with no sync state
// yet returns empty settings, and a sync state's stored metadata is
// surfaced.
// spec: SET-015
func TestTodoistAPI_GetSettings(t *testing.T) {
	t.Parallel()

	t.Run("NoAccount_Returns404", func(t *testing.T) {
		router, _, _, _ := newTodoistAPITest(t)

		w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/settings", nil)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("AccountNoSyncState_ReturnsEmpty200", func(t *testing.T) {
		router, _, _, seedAccount := newTodoistAPITest(t)
		seedAccount()

		w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/settings", nil)
		require.Equal(t, http.StatusOK, w.Code)

		settings := decodeTodoistSettings(t, w)
		assert.Nil(t, settings.ProjectID)
		assert.Nil(t, settings.LabelID)
		assert.Nil(t, settings.LabelName)
		assert.Nil(t, settings.IntegrationInstanceID)
	})

	t.Run("WithSyncState_ReturnsStoredSettings", func(t *testing.T) {
		router, syncRepo, _, seedAccount := newTodoistAPITest(t)
		accountID := seedAccount()

		wantProjectID := "proj-" + uuid.NewString()[:8]
		wantLabelID := "label-" + uuid.NewString()[:8]
		wantLabelName := "label-name-" + uuid.NewString()[:8]

		_, err := syncRepo.CreateSyncState(context.Background(), repository.CreateSyncStateRequest{
			Source:    todoist.SourceName,
			AccountID: &accountID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyFetchAll,
			Metadata: map[string]any{
				todoist.MetadataKeyProjectID: wantProjectID,
				todoist.MetadataKeyLabelID:   wantLabelID,
				todoist.MetadataKeyLabelName: wantLabelName,
			},
		})
		require.NoError(t, err)

		w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/settings", nil)
		require.Equal(t, http.StatusOK, w.Code)

		settings := decodeTodoistSettings(t, w)
		require.NotNil(t, settings.ProjectID)
		assert.Equal(t, wantProjectID, *settings.ProjectID)
		require.NotNil(t, settings.LabelID)
		assert.Equal(t, wantLabelID, *settings.LabelID)
		require.NotNil(t, settings.LabelName)
		assert.Equal(t, wantLabelName, *settings.LabelName)
	})
}

// TestTodoistAPI_UpdateSettings proves the write side of the hermetic
// Todoist settings surface: malformed-body validation runs before the
// account check, missing accounts 404, the sync state is lazily created,
// updates merge into (not replace) existing metadata, and
// integration_instance_id is generated once and then held stable.
// spec: SET-016
func TestTodoistAPI_UpdateSettings(t *testing.T) {
	t.Parallel()

	t.Run("MalformedBody_Returns400", func(t *testing.T) {
		router, _, _, seedAccount := newTodoistAPITest(t)
		seedAccount()

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/todoist/settings", bytes.NewReader([]byte("{not-json")))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAccount_Returns404", func(t *testing.T) {
		router, _, _, _ := newTodoistAPITest(t)

		w := doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"project_id": "proj-" + uuid.NewString()[:8],
		})
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("CreatesSyncStateIfAbsent", func(t *testing.T) {
		router, syncRepo, _, seedAccount := newTodoistAPITest(t)
		accountID := seedAccount()

		projectID := "proj-" + uuid.NewString()[:8]
		w := doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"project_id": projectID,
		})
		require.Equal(t, http.StatusOK, w.Code)

		state, err := syncRepo.GetSyncStateBySource(context.Background(), todoist.SourceName, &accountID)
		require.NoError(t, err, "sync state must be lazily created")
		assert.True(t, state.Enabled)
		assert.Equal(t, repository.SyncStrategyFetchAll, state.Strategy)
		assert.Equal(t, projectID, state.Metadata[todoist.MetadataKeyProjectID])
	})

	t.Run("MergesMetadataNotReplace", func(t *testing.T) {
		router, syncRepo, _, seedAccount := newTodoistAPITest(t)
		accountID := seedAccount()

		unrelatedValue := "unrelated-" + uuid.NewString()[:8]
		_, err := syncRepo.CreateSyncState(context.Background(), repository.CreateSyncStateRequest{
			Source:    todoist.SourceName,
			AccountID: &accountID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyFetchAll,
			Metadata: map[string]any{
				todoist.MetadataKeyUserTimezone: unrelatedValue,
			},
		})
		require.NoError(t, err)

		labelID := "label-" + uuid.NewString()[:8]
		w := doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"label_id": labelID,
		})
		require.Equal(t, http.StatusOK, w.Code)

		state, err := syncRepo.GetSyncStateBySource(context.Background(), todoist.SourceName, &accountID)
		require.NoError(t, err)
		assert.Equal(t, unrelatedValue, state.Metadata[todoist.MetadataKeyUserTimezone], "unrelated metadata key must survive the merge")
		assert.Equal(t, labelID, state.Metadata[todoist.MetadataKeyLabelID])
	})

	t.Run("IntegrationInstanceIDStableOnce", func(t *testing.T) {
		router, _, _, seedAccount := newTodoistAPITest(t)
		seedAccount()

		w := doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"project_id": "proj-" + uuid.NewString()[:8],
		})
		require.Equal(t, http.StatusOK, w.Code)
		first := decodeTodoistSettings(t, w)
		require.NotNil(t, first.IntegrationInstanceID)
		require.NotEmpty(t, *first.IntegrationInstanceID)

		w = doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"project_id": "proj-" + uuid.NewString()[:8],
		})
		require.Equal(t, http.StatusOK, w.Code)
		second := decodeTodoistSettings(t, w)
		require.NotNil(t, second.IntegrationInstanceID)
		assert.Equal(t, *first.IntegrationInstanceID, *second.IntegrationInstanceID, "integration_instance_id must be generated once and then left stable")
	})

	t.Run("LabelChangeClearsCachedName", func(t *testing.T) {
		// spec: SET-017
		router, syncRepo, _, seedAccount := newTodoistAPITest(t)
		accountID := seedAccount()

		oldLabelID := "label-" + uuid.NewString()[:8]
		cachedLabelName := "cached-name-" + uuid.NewString()[:8]
		_, err := syncRepo.CreateSyncState(context.Background(), repository.CreateSyncStateRequest{
			Source:    todoist.SourceName,
			AccountID: &accountID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyFetchAll,
			Metadata: map[string]any{
				todoist.MetadataKeyLabelID:   oldLabelID,
				todoist.MetadataKeyLabelName: cachedLabelName,
			},
		})
		require.NoError(t, err)

		newLabelID := "label-" + uuid.NewString()[:8]
		w := doTodoistRequest(router, http.MethodPatch, "/api/v1/todoist/settings", map[string]any{
			"label_id": newLabelID,
		})
		require.Equal(t, http.StatusOK, w.Code)

		state, err := syncRepo.GetSyncStateBySource(context.Background(), todoist.SourceName, &accountID)
		require.NoError(t, err)
		assert.Equal(t, newLabelID, state.Metadata[todoist.MetadataKeyLabelID])
		_, hasLabelName := state.Metadata[todoist.MetadataKeyLabelName]
		assert.False(t, hasLabelName, "label_name must be cleared from stored metadata when the label changes")
	})
}
