package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const apiTestChatID1 int64 = 60001
const apiTestChatID2 int64 = 60002

func setupTelegramChatRouter(t *testing.T) (*gin.Engine, *repository.TelegramChatConfigRepository, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	gin.SetMode(gin.TestMode)

	// Migrations are applied once by TestMain.

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

	// Phase 4 dependencies (nil for chat API tests — not exercised)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	externalContactRepo := repository.NewExternalContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)
	wireCadenceUpdaterForAPITest(t, database, contactService)

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
	tgRoutes := v1.Group("/telegram")
	tgChats := tgRoutes.Group("/chats")
	{
		tgChats.GET("", handler.ListChats)
		tgChats.PATCH("/:chat_id", handler.UpdateChatStatus)
	}

	cleanup := func() {
		_ = database.Queries.DeleteTelegramChatConfig(ctx, apiTestChatID1)
		_ = database.Queries.DeleteTelegramChatConfig(ctx, apiTestChatID2)
		database.Close()
	}

	// Clean before test
	_ = database.Queries.DeleteTelegramChatConfig(ctx, apiTestChatID1)
	_ = database.Queries.DeleteTelegramChatConfig(ctx, apiTestChatID2)

	return router, chatConfigRepo, cleanup
}

func TestChatAPI_ListChats(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, chatConfigRepo, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	ctx := context.Background()

	// Create a small group (tracked) and a large group (not tracked)
	mc5 := int32(5)
	title1 := "Small Group"
	_, err := chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: apiTestChatID1,
		ChatTitle:      &title1,
		ChatType:       "group",
		MemberCount:    &mc5,
		Status:         "auto",
	})
	require.NoError(t, err)

	mc50 := int32(50)
	title2 := "Large Group"
	_, err = chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: apiTestChatID2,
		ChatTitle:      &title2,
		ChatType:       "group",
		MemberCount:    &mc50,
		Status:         "auto",
	})
	require.NoError(t, err)

	req, _ := http.NewRequest("GET", "/api/v1/telegram/chats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []struct {
			TelegramChatID   int64  `json:"telegram_chat_id"`
			ChatTitle        string `json:"chat_title"`
			Status           string `json:"status"`
			EffectiveTracked bool   `json:"effective_tracked"`
		} `json:"data"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(resp.Data), 2)

	// Find our test chats
	var small, large bool
	for _, chat := range resp.Data {
		if chat.TelegramChatID == apiTestChatID1 {
			small = true
			assert.True(t, chat.EffectiveTracked, "small group should be tracked")
		}
		if chat.TelegramChatID == apiTestChatID2 {
			large = true
			assert.False(t, chat.EffectiveTracked, "large group should not be tracked")
		}
	}
	assert.True(t, small, "small group should be in response")
	assert.True(t, large, "large group should be in response")
}

func TestChatAPI_ListChats_ExcludesPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, chatConfigRepo, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	ctx := context.Background()

	_, err := chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: apiTestChatID1,
		ChatType:       "private",
		Status:         "auto",
	})
	require.NoError(t, err)

	req, _ := http.NewRequest("GET", "/api/v1/telegram/chats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []struct {
			TelegramChatID int64 `json:"telegram_chat_id"`
		} `json:"data"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	for _, chat := range resp.Data {
		assert.NotEqual(t, apiTestChatID1, chat.TelegramChatID, "private chat should not appear")
	}
}

func TestChatAPI_UpdateStatus_Ignored(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, chatConfigRepo, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	ctx := context.Background()

	mc5 := int32(5)
	_, err := chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: apiTestChatID1,
		ChatType:       "group",
		MemberCount:    &mc5,
		Status:         "auto",
	})
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]string{"status": "ignored"})
	req, _ := http.NewRequest("PATCH", "/api/v1/telegram/chats/60001", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data struct {
			Status           string `json:"status"`
			EffectiveTracked bool   `json:"effective_tracked"`
		} `json:"data"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ignored", resp.Data.Status)
	assert.False(t, resp.Data.EffectiveTracked)
}

func TestChatAPI_UpdateStatus_TrackedOverridesLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, chatConfigRepo, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	ctx := context.Background()

	mc50 := int32(50)
	_, err := chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: apiTestChatID1,
		ChatType:       "group",
		MemberCount:    &mc50,
		Status:         "auto",
	})
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]string{"status": "tracked"})
	req, _ := http.NewRequest("PATCH", "/api/v1/telegram/chats/60001", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data struct {
			Status           string `json:"status"`
			EffectiveTracked bool   `json:"effective_tracked"`
		} `json:"data"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "tracked", resp.Data.Status)
	assert.True(t, resp.Data.EffectiveTracked)
}

func TestChatAPI_UpdateStatus_InvalidStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, _, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"status": "invalid"})
	req, _ := http.NewRequest("PATCH", "/api/v1/telegram/chats/60001", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChatAPI_UpdateStatus_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	router, _, cleanup := setupTelegramChatRouter(t)
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"status": "ignored"})
	req, _ := http.NewRequest("PATCH", "/api/v1/telegram/chats/99999999", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
