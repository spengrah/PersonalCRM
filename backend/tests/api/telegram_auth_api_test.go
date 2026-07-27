//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTelegramAuthRouter builds a TelegramHandler over a real TelegramManager
// wired to a per-test isolated DB clone (newIsolatedRiverTestDB), with the
// production route surface (handlers.RegisterTelegramRoutes). The clone means
// the singleton telegram_session row is private to this test, so seeding /
// deleting it cannot race sibling tests or the package-level session tests.
//
// None of the auth endpoints exercised here ever spawn an MTProto client:
// validation rejects before the manager, the already-connected 409 is decided
// from the DB session row before the client is created, invalid-token paths
// hit only the in-memory AuthSessionManager, and Disconnect/GetStatus resolve
// entirely from DB + in-memory state.
func setupTelegramAuthRouter(t *testing.T, ctx context.Context) (*gin.Engine, *repository.TelegramSessionRepository) {
	t.Helper()

	database, _ := newIsolatedRiverTestDB(t, ctx)

	sessionRepo := repository.NewTelegramSessionRepository(database.Queries)
	updateStateRepo := repository.NewTelegramUpdateStateRepository(database.Queries)
	chatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)

	// 32 bytes = 64 hex chars
	encryptor, err := crypto.NewTokenEncryptor("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, err)

	telegramCfg := &config.TelegramConfig{
		GroupMaxMembers:      10,
		BackfillSince:        "2026-01-01",
		BurstWindowHours:     2,
		ReplyBridgeHours:     48,
		DiscoveryMinMessages: 3,
	}

	// Phase 4 dependencies (not exercised by the auth endpoints)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	externalContactRepo := repository.NewExternalContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil, cadenceUpdater, assertSvc, cache, nil)

	manager := tgpkg.NewTelegramManager(
		sessionRepo,
		updateStateRepo,
		chatConfigRepo,
		messageRepo,
		syncRepo,
		encryptor,
		12345,
		"testhash",
		telegramCfg,
		identityService,
		externalContactRepo,
		nil,
		interactionRepo,
		contactService,
		contactService,
		nil,
		nil, // pool: non-tx publish fallback (test mode)
		nil, // enqueuer: stale-claim recovery disabled
	)

	handler := handlers.NewTelegramHandler(manager)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	v1 := router.Group("/api/v1")
	handlers.RegisterTelegramRoutes(v1, handler)

	return router, sessionRepo
}

// seedConnectedTelegramSession upserts the singleton session row in the
// connected state via the real repository (never raw SQL).
func seedConnectedTelegramSession(t *testing.T, ctx context.Context, repo *repository.TelegramSessionRepository, username, phone string) {
	t.Helper()
	userID := int64(987654)
	_, err := repo.UpsertSession(ctx, repository.UpsertTelegramSessionParams{
		SessionDataEncrypted: []byte("test-session-data"),
		EncryptionNonce:      []byte("test-nonce"),
		PhoneNumber:          &phone,
		TelegramUserID:       &userID,
		Username:             &username,
		AuthState:            "connected",
	})
	require.NoError(t, err)
}

// doTelegramJSON serves method+path with payload against the router. A string
// payload is sent verbatim (for malformed-body cases); anything else is
// JSON-marshalled. nil sends no body.
func doTelegramJSON(t *testing.T, router *gin.Engine, method, path string, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch v := payload.(type) {
	case nil:
		reader = nil
	case string:
		reader = strings.NewReader(v)
	default:
		b, err := json.Marshal(v)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// decodeTelegramWire decodes the raw response body into a generic map so
// assertions run against LITERAL wire keys, never production DTO tags.
func decodeTelegramWire(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body: %s", w.Body.String())
	return body
}

// telegramWireErrorCode extracts error.code from the wire envelope.
func telegramWireErrorCode(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]interface{})
	require.True(t, ok, "expected error object in envelope, got: %v", body)
	code, ok := errObj["code"].(string)
	require.True(t, ok, "expected error.code string, got: %v", errObj)
	return code
}

// telegramWireData extracts the data object from the wire envelope.
func telegramWireData(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, ok := body["data"].(map[string]interface{})
	require.True(t, ok, "expected data object in envelope, got: %v", body)
	return data
}

// TestTelegramAuthAPI_ValidationErrors proves every listed input-validation
// branch of the auth endpoints returns 400 VALIDATION_ERROR, and that phone
// numbers on BOTH valid boundary edges (7 and 15 digits) pass validation —
// proven by reaching the manager's already-connected conflict (409) instead of
// a 400, which requires no MTProto client.
func TestTelegramAuthAPI_ValidationErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	router, _ := setupTelegramAuthRouter(t, ctx)

	t.Run("StartAuth_MissingOrEmptyPhone", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		for _, payload := range []interface{}{
			map[string]string{},                      // field absent
			map[string]string{"phone_number": ""},    // empty
			map[string]string{"phone_number": "   "}, // whitespace-only (trimmed)
		} {
			w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/start", payload)
			assert.Equal(t, http.StatusBadRequest, w.Code, "payload %v", payload)
			assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)), "payload %v", payload)
		}
	})

	t.Run("StartAuth_MalformedPhone", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		for _, phone := range []string{
			"1234567",           // no leading +
			"+123456",           // 6 digits — below the 7-digit minimum
			"+1234567890123456", // 16 digits — above the 15-digit maximum
			"+12345ab",          // letters
			"++1234567",         // double plus
		} {
			w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/start", map[string]string{"phone_number": phone})
			assert.Equal(t, http.StatusBadRequest, w.Code, "phone %q", phone)
			assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)), "phone %q", phone)
		}
	})

	t.Run("StartAuth_BoundaryValidPhonesPassValidation", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		// A dedicated harness whose session row is seeded connected: a phone
		// that PASSES validation reaches the manager and is answered with the
		// already-connected 409 (decided from the DB row, before any MTProto
		// client is created). A validation regression would answer 400 instead.
		bctx := context.Background()
		brouter, bsessionRepo := setupTelegramAuthRouter(t, bctx)
		ns := uuid.NewString()[:8]
		seedConnectedTelegramSession(t, bctx, bsessionRepo, "tg-boundary-"+ns, "+15550000001")

		for _, phone := range []string{
			"+1234567",         // 7 digits — minimum valid length
			"+123456789012345", // 15 digits — maximum valid length
		} {
			w := doTelegramJSON(t, brouter, http.MethodPost, "/api/v1/telegram/auth/start", map[string]string{"phone_number": phone})
			assert.Equal(t, http.StatusConflict, w.Code, "phone %q should pass validation and reach the manager", phone)
			assert.Equal(t, api.ErrCodeConflict, telegramWireErrorCode(t, decodeTelegramWire(t, w)), "phone %q", phone)
		}
	})

	t.Run("StartAuth_MalformedBody", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/start", "{not-json")
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyCode_MissingToken", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-code", map[string]string{"code": "12345"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyCode_MissingCode", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-code", map[string]string{"auth_token": "deadbeef"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyCode_MalformedBody", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-code", "{not-json")
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyPassword_MissingToken", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-password", map[string]string{"password": "hunter2"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyPassword_MissingPassword", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-password", map[string]string{"auth_token": "deadbeef"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})

	t.Run("VerifyPassword_MalformedBody", func(t *testing.T) {
		// spec: TGM-007.empty-malformed-phone-number
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/verify-password", "{not-json")
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, api.ErrCodeValidation, telegramWireErrorCode(t, decodeTelegramWire(t, w)))
	})
}

// TestTelegramAuthAPI_StartConflictWhenAlreadyConnected is GROUNDWORK for
// TGM-007.in-progress-or-connected-409 (deliberately uncited): it proves the already-connected half of
// the then-item (409 CONFLICT decided from the stored session). The
// auth-in-progress half is unreachable without a live MTProto client — the
// AuthSessionManager's session field is unexported and set only inside a real
// StartAuth flow — so the whole item cannot be cited yet.
func TestTelegramAuthAPI_StartConflictWhenAlreadyConnected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	router, sessionRepo := setupTelegramAuthRouter(t, ctx)
	ns := uuid.NewString()[:8]
	seedConnectedTelegramSession(t, ctx, sessionRepo, "tg-conflict-"+ns, "+15550000002")

	w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/start", map[string]string{"phone_number": "+15551234567"})
	assert.Equal(t, http.StatusConflict, w.Code)
	body := decodeTelegramWire(t, w)
	assert.Equal(t, false, body["success"])
	assert.Equal(t, api.ErrCodeConflict, telegramWireErrorCode(t, body))
	errObj := body["error"].(map[string]interface{})
	assert.Contains(t, errObj["message"], "Already connected")
}

// TestTelegramAuthAPI_InvalidTokenBadRequest is GROUNDWORK for TGM-007.invalid-token-client-error
// (deliberately uncited): it proves the invalid-token half (400 BAD_REQUEST,
// distinct from the 400 VALIDATION_ERROR of missing fields) for both
// verify-code and verify-password. The expired half (410) is unreachable
// deterministically: cleanup() closes the session's Done channel and nils the
// session pointer atomically under one mutex, so a post-TTL request takes the
// invalid-token path; the 410 branch only fires in the race window between the
// token check and the Done check of a LIVE MTProto-backed session, and the TTL
// timer is a raw time.AfterFunc the accelerated clock cannot advance.
func TestTelegramAuthAPI_InvalidTokenBadRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	router, _ := setupTelegramAuthRouter(t, ctx)

	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]string
	}{
		{"VerifyCode", "/api/v1/telegram/auth/verify-code", map[string]string{"auth_token": "no-such-token", "code": "12345"}},
		{"VerifyPassword", "/api/v1/telegram/auth/verify-password", map[string]string{"auth_token": "no-such-token", "password": "hunter2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doTelegramJSON(t, router, http.MethodPost, tc.path, tc.payload)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			body := decodeTelegramWire(t, w)
			assert.Equal(t, api.ErrCodeBadRequest, telegramWireErrorCode(t, body))
			errObj := body["error"].(map[string]interface{})
			assert.Equal(t, "Invalid auth token", errObj["message"])
		})
	}
}

// TestTelegramAuthAPI_CancelAlwaysSucceeds is GROUNDWORK for TGM-007.cancel-always-succeeds-200
// (deliberately uncited): it proves the cancel-always-succeeds half — 200 with
// {"status":"cancelled"} even with no flow in progress, and idempotently on
// repeat. The successful-step half (a StartAuth/VerifyCode/VerifyPassword
// success returning 200 with the flow status) requires a completing MTProto
// auth flow, for which no stub seam exists.
func TestTelegramAuthAPI_CancelAlwaysSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	router, _ := setupTelegramAuthRouter(t, ctx)

	for i := 0; i < 2; i++ { // idempotent: "always succeeds"
		w := doTelegramJSON(t, router, http.MethodPost, "/api/v1/telegram/auth/cancel", nil)
		assert.Equal(t, http.StatusOK, w.Code, "cancel call %d", i+1)
		body := decodeTelegramWire(t, w)
		assert.Equal(t, true, body["success"], "cancel call %d", i+1)
		data := telegramWireData(t, body)
		assert.Equal(t, "cancelled", data["status"], "cancel call %d", i+1)
	}
}

// TestTelegramAuthAPI_Disconnect proves both halves of TGM-007.disconnect-200-unless-delete-fails: the happy
// path returns 200 with {"status":"disconnected"} and clears the session row;
// a failing session-row delete (forced by serving the request with an
// already-cancelled context, so the repository DELETE fails without any
// production seam) is a 500 that leaves the row intact for retry.
func TestTelegramAuthAPI_Disconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	t.Run("SucceedsAndClearsSession", func(t *testing.T) {
		// spec: TGM-007.disconnect-200-unless-delete-fails
		ctx := context.Background()
		router, sessionRepo := setupTelegramAuthRouter(t, ctx)
		ns := uuid.NewString()[:8]
		seedConnectedTelegramSession(t, ctx, sessionRepo, "tg-disc-"+ns, "+15550000003")

		w := doTelegramJSON(t, router, http.MethodDelete, "/api/v1/telegram/auth", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		body := decodeTelegramWire(t, w)
		assert.Equal(t, true, body["success"])
		data := telegramWireData(t, body)
		assert.Equal(t, "disconnected", data["status"])

		_, err := sessionRepo.GetSession(ctx)
		assert.ErrorIs(t, err, db.ErrNotFound, "session row should be deleted")
	})

	t.Run("SessionDeleteFailureIsServerError", func(t *testing.T) {
		// spec: TGM-007.disconnect-200-unless-delete-fails
		ctx := context.Background()
		router, sessionRepo := setupTelegramAuthRouter(t, ctx)
		ns := uuid.NewString()[:8]
		seedConnectedTelegramSession(t, ctx, sessionRepo, "tg-discfail-"+ns, "+15550000004")

		// A pre-cancelled request context makes the session-row DELETE fail
		// inside TelegramManager.Disconnect without touching production code.
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		req, err := http.NewRequestWithContext(cancelledCtx, http.MethodDelete, "/api/v1/telegram/auth", nil)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		body := decodeTelegramWire(t, w)
		assert.Equal(t, false, body["success"])
		assert.Equal(t, api.ErrCodeInternal, telegramWireErrorCode(t, body))

		// The delete failed, so the row must survive for a retry...
		sess, err := sessionRepo.GetSession(ctx)
		require.NoError(t, err, "session row should survive the failed disconnect")
		assert.Equal(t, "connected", sess.AuthState)

		// ...and the retry with a healthy context succeeds.
		w = doTelegramJSON(t, router, http.MethodDelete, "/api/v1/telegram/auth", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		_, err = sessionRepo.GetSession(ctx)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}

// TestTelegramAuthAPI_Status proves TGM-007.status-always-returns-200: status always answers 200 and
// reflects the current connection state, asserting the LITERAL wire keys of
// the payload in both the no-session and stored-connected-session states.
func TestTelegramAuthAPI_Status(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	t.Run("DisconnectedWithoutSession", func(t *testing.T) {
		// spec: TGM-007.status-always-returns-200
		ctx := context.Background()
		router, _ := setupTelegramAuthRouter(t, ctx)

		w := doTelegramJSON(t, router, http.MethodGet, "/api/v1/telegram/auth/status", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		body := decodeTelegramWire(t, w)
		assert.Equal(t, true, body["success"])
		data := telegramWireData(t, body)
		assert.Equal(t, false, data["connected"])
		assert.Equal(t, "disconnected", data["status"])
		_, hasUsername := data["username"]
		assert.False(t, hasUsername, "username must be absent when no session exists")
		_, hasPhone := data["phone_number"]
		assert.False(t, hasPhone, "phone_number must be absent when no session exists")
	})

	t.Run("ConnectedFromStoredSession", func(t *testing.T) {
		// spec: TGM-007.status-always-returns-200
		ctx := context.Background()
		router, sessionRepo := setupTelegramAuthRouter(t, ctx)
		ns := uuid.NewString()[:8]
		username := "tg-status-" + ns
		phone := "+15550000005"
		seedConnectedTelegramSession(t, ctx, sessionRepo, username, phone)

		w := doTelegramJSON(t, router, http.MethodGet, "/api/v1/telegram/auth/status", nil)
		assert.Equal(t, http.StatusOK, w.Code)
		body := decodeTelegramWire(t, w)
		assert.Equal(t, true, body["success"])
		data := telegramWireData(t, body)
		assert.Equal(t, true, data["connected"])
		assert.Equal(t, "connected", data["status"])
		assert.Equal(t, username, data["username"])
		assert.Equal(t, phone, data["phone_number"])
	})
}
