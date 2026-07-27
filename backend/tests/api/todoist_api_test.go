//go:build integration_testdb

// Package api's todoist_api_test.go covers the TodoistHandler surface:
// GetSettings and UpdateSettings (hermetic, DB-only — they read/write only
// through oauthService.ListAccounts + syncRepo), plus the full
// ListProjects/ListLabels picker contract (SET-018).
//
// The pickers' no-account-connected 404 branch (SET-018.no-account-connected-404) runs entirely
// before either handler touches the live provider client (it returns as soon
// as len(accounts)==0), so it is hermetic. The live-provider clauses
// (SET-018.deleted-entries-filtered-out deleted-entry filtering, SET-018.token-provider-api-failure token/provider-failure 500)
// need no production seam: the handlers construct
// todoist.NewSyncClient(accessToken) inline, but the client reads the
// package-level todoist.SyncEndpoint var at request-build time, so pointing
// that var at an httptest server redirects the live-provider call — the same
// endpoint-override precedent as withTodoistEndpoints in
// backend/tests/todoist_oauth_token_integration_test.go. Tests that override
// the endpoint var must not run in parallel; see withTodoistSyncEndpoint.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTodoistAPITest builds a fresh, empty isolated-River DB clone, wires the
// production Todoist settings route surface over it, and returns two seeding
// closures that upsert a connected-account credential on demand: seedAccount
// stores dummy (undecryptable) token bytes for the hermetic settings surface,
// seedAccountWithToken stores accessToken encrypted with the real
// TokenEncryptor so handler paths that call oauthService.GetAccessToken can
// decrypt it. Every subtest MUST call this as its first line:
// TodoistOAuthService.ListAccounts lists ALL todoist credentials with no
// filter, so a shared clone would let one subtest's seeded account leak into
// a sibling "NoAccount" subtest, especially under -shuffle=on.
func newTodoistAPITest(t *testing.T) (router *gin.Engine, syncRepo *repository.SyncRepository, seedAccount func() string, seedAccountWithToken func(accessToken string) string) {
	t.Helper()
	ctx := context.Background()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	oauthRepo := repository.NewOAuthRepository(database.Queries)
	syncRepo = repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	oauthSvc, err := todoist.NewOAuthService(cfg, oauthRepo, syncRepo)
	require.NoError(t, err)

	handler := handlers.NewTodoistHandler(oauthSvc, syncRepo)
	router = gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterTodoistRoutes(v1, handler)

	seedAccount = func() string {
		t.Helper()
		accountID := "todoist-acct-" + uuid.NewString()[:8]
		// access_token_encrypted / encryption_nonce are NOT NULL columns,
		// but GetSettings/UpdateSettings never decrypt them — dummy non-nil
		// bytes satisfy the constraint without a real token. (The 4-byte
		// nonce also deterministically fails Decrypt's nonce-size check,
		// which the SET-018.token-provider-api-failure token-failure test relies on.)
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

	seedAccountWithToken = func(accessToken string) string {
		t.Helper()
		// Encrypt with the same key NewOAuthService derived its encryptor
		// from, so GetAccessToken decrypts back to accessToken (mirrors how
		// the oauth exchange round-trip test stores a real token).
		encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
		require.NoError(t, err)
		ciphertext, nonce, err := encryptor.Encrypt(accessToken)
		require.NoError(t, err)
		accountID := "todoist-acct-" + uuid.NewString()[:8]
		_, err = oauthRepo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:             todoist.ProviderName,
			AccountID:            accountID,
			AccessTokenEncrypted: ciphertext,
			EncryptionNonce:      nonce,
			TokenType:            "Bearer",
		})
		require.NoError(t, err)
		return accountID
	}

	return router, syncRepo, seedAccount, seedAccountWithToken
}

// withTodoistSyncEndpoint points the package-level todoist.SyncEndpoint var
// at url and restores the original in t.Cleanup. The picker handlers build
// todoist.NewSyncClient(accessToken) inline, but the client reads
// SyncEndpoint at request-build time, so overriding the var redirects the
// live-provider call to a fake server. The var is package-global with no
// per-instance setter (same as todoist.TokenEndpoint — see
// withTodoistEndpoints in todoist_oauth_token_integration_test.go), so tests
// using this helper must NOT call t.Parallel.
func withTodoistSyncEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := todoist.SyncEndpoint
	todoist.SyncEndpoint = url
	t.Cleanup(func() { todoist.SyncEndpoint = orig })
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
		router, _, seedAccount, _ := newTodoistAPITest(t)
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
		router, syncRepo, seedAccount, _ := newTodoistAPITest(t)
		accountID := seedAccount()

		wantProjectID := "proj-" + uuid.NewString()[:8]
		wantLabelID := "label-" + uuid.NewString()[:8]
		wantLabelName := "label-name-" + uuid.NewString()[:8]
		wantInstanceID := "instance-" + uuid.NewString()[:8]
		wantTimezone := "America/Chicago"

		_, err := syncRepo.CreateSyncState(context.Background(), repository.CreateSyncStateRequest{
			Source:    todoist.SourceName,
			AccountID: &accountID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyFetchAll,
			Metadata: map[string]any{
				todoist.MetadataKeyProjectID:           wantProjectID,
				todoist.MetadataKeyLabelID:             wantLabelID,
				todoist.MetadataKeyLabelName:           wantLabelName,
				todoist.MetadataKeyIntegrationInstance: wantInstanceID,
				todoist.MetadataKeyUserTimezone:        wantTimezone,
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
		require.NotNil(t, settings.IntegrationInstanceID)
		assert.Equal(t, wantInstanceID, *settings.IntegrationInstanceID)
		require.NotNil(t, settings.UserTimezone)
		assert.Equal(t, wantTimezone, *settings.UserTimezone)
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
		// Deliberately NO seeded account: getting a 400 (not the no-account
		// 404) proves body validation runs before the account check.
		router, _, _, _ := newTodoistAPITest(t)

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
		router, syncRepo, seedAccount, _ := newTodoistAPITest(t)
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
		router, syncRepo, seedAccount, _ := newTodoistAPITest(t)
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
		router, _, seedAccount, _ := newTodoistAPITest(t)
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
		router, syncRepo, seedAccount, _ := newTodoistAPITest(t)
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

// TestTodoistAPI_Pickers_NoAccount proves the ListProjects/ListLabels
// no-account guard: both routes check len(accounts)==0 and return 404
// BEFORE constructing a live provider client, so this is testable with no
// external Todoist access. Each subtest asserts the literal wire shape of
// the error envelope (raw map, not the DTO struct) so a renamed/mis-typed
// field is caught.
func TestTodoistAPI_Pickers_NoAccount(t *testing.T) {
	t.Parallel()

	t.Run("ListProjects_NoAccount_Returns404", func(t *testing.T) {
		// spec: SET-018.no-account-connected-404
		router, _, _, _ := newTodoistAPITest(t)

		w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/projects", nil)
		require.Equal(t, http.StatusNotFound, w.Code)

		var envelope map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		assert.Equal(t, false, envelope["success"])
		errObj, ok := envelope["error"].(map[string]interface{})
		require.True(t, ok, "error envelope must carry an 'error' object")
		assert.Equal(t, "NOT_FOUND", errObj["code"])
		assert.NotEmpty(t, errObj["message"])
		_, hasData := envelope["data"]
		assert.False(t, hasData, "a 404 error response must not carry a 'data' key")
	})

	t.Run("ListLabels_NoAccount_Returns404", func(t *testing.T) {
		// spec: SET-018.no-account-connected-404
		router, _, _, _ := newTodoistAPITest(t)

		w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/labels", nil)
		require.Equal(t, http.StatusNotFound, w.Code)

		var envelope map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		assert.Equal(t, false, envelope["success"])
		errObj, ok := envelope["error"].(map[string]interface{})
		require.True(t, ok, "error envelope must carry an 'error' object")
		assert.Equal(t, "NOT_FOUND", errObj["code"])
		assert.NotEmpty(t, errObj["message"])
		_, hasData := envelope["data"]
		assert.False(t, hasData, "a 404 error response must not carry a 'data' key")
	})
}

// decodeTodoistPickerData unwraps a picker success envelope into raw JSON
// maps so tests can assert the literal wire keys (not a DTO re-encode).
func decodeTodoistPickerData(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var envelope struct {
		Success bool                     `json:"success"`
		Data    []map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	return envelope.Data
}

// assertTodoistInternalErrorWire asserts the literal 500 error envelope both
// picker routes emit via api.RespondInternal: success=false, INTERNAL_ERROR
// code, generic message, no data key.
func assertTodoistInternalErrorWire(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var envelope map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	assert.Equal(t, false, envelope["success"])
	errObj, ok := envelope["error"].(map[string]interface{})
	require.True(t, ok, "error envelope must carry an 'error' object")
	assert.Equal(t, "INTERNAL_ERROR", errObj["code"])
	assert.Equal(t, "Internal server error", errObj["message"])
	_, hasData := envelope["data"]
	assert.False(t, hasData, "a 500 error response must not carry a 'data' key")
}

// TestTodoistAPI_Pickers_FilterDeletedEntries proves the live-provider half
// of the picker contract: the handler decrypts the stored token, queries the
// provider with it, and filters is_deleted entries out of both the projects
// and labels results. The provider is an httptest server reached by
// overriding todoist.SyncEndpoint, so this test must NOT be t.Parallel (see
// withTodoistSyncEndpoint). The fake returns a sync payload mixing live and
// is_deleted projects/labels; each route must surface ONLY the live entry,
// as literal wire keys id+name and nothing else.
func TestTodoistAPI_Pickers_FilterDeletedEntries(t *testing.T) {
	router, _, _, seedAccountWithToken := newTodoistAPITest(t)

	ns := uuid.NewString()[:8]
	accessToken := "live-todoist-token-" + ns
	seedAccountWithToken(accessToken)

	var mu sync.Mutex
	var gotAuth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sync_token": "fake-sync-token",
			"full_sync":  true,
			"projects": []map[string]interface{}{
				{"id": "proj-live-" + ns, "name": "live-project-" + ns, "color": "blue", "is_deleted": false},
				{"id": "proj-del-" + ns, "name": "deleted-project-" + ns, "color": "red", "is_deleted": true},
			},
			"labels": []map[string]interface{}{
				{"id": "label-live-" + ns, "name": "live-label-" + ns, "color": "green", "is_deleted": false},
				{"id": "label-del-" + ns, "name": "deleted-label-" + ns, "color": "yellow", "is_deleted": true},
			},
		})
	}))
	t.Cleanup(server.Close)
	withTodoistSyncEndpoint(t, server.URL)

	// spec: SET-018.deleted-entries-filtered-out
	w := doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/projects", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []map[string]interface{}{
		{"id": "proj-live-" + ns, "name": "live-project-" + ns},
	}, decodeTodoistPickerData(t, w), "projects must contain ONLY the live entry, as id+name")

	w = doTodoistRequest(router, http.MethodGet, "/api/v1/todoist/labels", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []map[string]interface{}{
		{"id": "label-live-" + ns, "name": "live-label-" + ns},
	}, decodeTodoistPickerData(t, w), "labels must contain ONLY the live entry, as id+name")

	// The provider must have been queried with the account's decrypted token.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotAuth, 2, "each picker route must query the live provider exactly once")
	for _, auth := range gotAuth {
		assert.Equal(t, "Bearer "+accessToken, auth, "provider query must carry the decrypted stored token")
	}
}

// TestTodoistAPI_Pickers_TokenOrProviderFailure500 proves the failure half of
// the picker contract on both routes and both failure classes:
//   - token failure: seedAccount's dummy 4-byte nonce makes GetAccessToken's
//     Decrypt fail on nonce size before any provider client is built, so this
//     half is hermetic (no endpoint override).
//   - provider-API failure: a valid decryptable token plus a fake provider
//     answering HTTP 500, reached by overriding todoist.SyncEndpoint — so
//     this test must NOT be t.Parallel (see withTodoistSyncEndpoint).
//
// Both halves assert the literal 500 error envelope on both routes.
func TestTodoistAPI_Pickers_TokenOrProviderFailure500(t *testing.T) {
	pickerPaths := []string{"/api/v1/todoist/projects", "/api/v1/todoist/labels"}

	t.Run("TokenDecryptFailure", func(t *testing.T) {
		router, _, seedAccount, _ := newTodoistAPITest(t)
		seedAccount()

		for _, path := range pickerPaths {
			// spec: SET-018.token-provider-api-failure
			w := doTodoistRequest(router, http.MethodGet, path, nil)
			assertTodoistInternalErrorWire(t, w)
		}
	})

	t.Run("ProviderAPIFailure", func(t *testing.T) {
		router, _, _, seedAccountWithToken := newTodoistAPITest(t)
		seedAccountWithToken("live-todoist-token-" + uuid.NewString()[:8])

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error_tag":"SERVER_ERROR","error_code":500,"http_code":500,"error":"provider exploded"}`))
		}))
		t.Cleanup(server.Close)
		withTodoistSyncEndpoint(t, server.URL)

		for _, path := range pickerPaths {
			// spec: SET-018.token-provider-api-failure
			w := doTodoistRequest(router, http.MethodGet, path, nil)
			assertTodoistInternalErrorWire(t, w)
		}
	})
}
