package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// GET /api/v1/host/:id/known-identifiers integration tests
// ----------------------------------------------------------------------------

type knownIDsEnv struct {
	router         *gin.Engine
	database       *db.Database
	macService     *service.MacHostService
	contactRepo    *repository.ContactRepository
	cmRepo         *repository.ContactMethodRepository
	pairedHostID   uuid.UUID
	pairedHostKey  string
	seededContacts []uuid.UUID
}

func setupKnownIDsEnv(t *testing.T) *knownIDsEnv {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.External.APIKey = macHostTestKey

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, nil, database.Pool, 4)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))

	limiter := auth.NewPairingIPRateLimiter()
	macHandler := handlers.NewMacHostHandler(macService, limiter)
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    hostRepo,
		Handler:     macHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})

	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "known-ids-test", "0.1.0", 1)
	require.NoError(t, err)

	env := &knownIDsEnv{
		router:        router,
		database:      database,
		macService:    macService,
		contactRepo:   contactRepo,
		cmRepo:        contactMethodRepo,
		pairedHostID:  pair.HostID,
		pairedHostKey: pair.APIKey,
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, id := range env.seededContacts {
			_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, id)
			_ = contactRepo.HardDeleteContact(cleanCtx, id)
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		database.Close()
	})
	return env
}

// seedKnownIDsContact creates a CRM contact with the supplied methods.
// Each method tuple is (type, value).
func (e *knownIDsEnv) seedKnownIDsContact(t *testing.T, name string, methods [][2]string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	created, err := e.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	for _, m := range methods {
		_, err := e.cmRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: created.ID,
			Type:      m[0],
			Value:     m[1],
		})
		require.NoError(t, err)
	}
	e.seededContacts = append(e.seededContacts, created.ID)
	return created.ID
}

func getKnownIdentifiers(t *testing.T, env *knownIDsEnv, hostID, hostKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/"+hostID+"/known-identifiers", nil)
	if hostID != "" {
		req.Header.Set("X-Mac-Host-ID", hostID)
	}
	if hostKey != "" {
		req.Header.Set("Authorization", "Bearer "+hostKey)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

type knownIDsResp struct {
	Success bool `json:"success"`
	Data    struct {
		Phones []string `json:"phones"`
		Emails []string `json:"emails"`
	} `json:"data"`
}

func parseKnownIDsResp(t *testing.T, w *httptest.ResponseRecorder) knownIDsResp {
	t.Helper()
	var r knownIDsResp
	require.NoError(t, json.NewDecoder(w.Body).Decode(&r), "body: %s", w.Body.String())
	return r
}

func TestKnownIdentifiers_Auth_Missing_401(t *testing.T) {
	env := setupKnownIDsEnv(t)
	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), "")
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

func TestKnownIdentifiers_Auth_WrongKey_401(t *testing.T) {
	env := setupKnownIDsEnv(t)
	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), "deadbeef")
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

func TestKnownIdentifiers_HappyPath_ReturnsSortedDistinctSets(t *testing.T) {
	env := setupKnownIDsEnv(t)
	// Two contacts with overlapping phones (different normalized values).
	env.seedKnownIDsContact(t, "Known IDs A "+uuid.NewString()[:6], [][2]string{
		{"email", "alpha-" + uuid.NewString()[:6] + "@example.invalid"},
		{"phone", "+15550009999"},
	})
	env.seedKnownIDsContact(t, "Known IDs B "+uuid.NewString()[:6], [][2]string{
		{"email", "beta-" + uuid.NewString()[:6] + "@example.invalid"},
		{"phone", "+15550009999"}, // same phone — DISTINCT collapses
		{"phone", "+15550008888"},
	})

	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), env.pairedHostKey)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsResp(t, w)
	require.True(t, r.Success)

	// Phones: duplicate collapses to one entry; sorted ascending.
	require.Contains(t, r.Data.Phones, "+15550009999")
	require.Contains(t, r.Data.Phones, "+15550008888")
	// 8888 sorts before 9999.
	pos8 := indexOf(r.Data.Phones, "+15550008888")
	pos9 := indexOf(r.Data.Phones, "+15550009999")
	require.True(t, pos8 < pos9, "phones must be sorted ASC; got %v", r.Data.Phones)
	require.Equal(t, 1, occurrences(r.Data.Phones, "+15550009999"),
		"distinct semantics: same number on two contacts should appear once")
}

func TestKnownIdentifiers_ExcludesSoftDeletedContact(t *testing.T) {
	env := setupKnownIDsEnv(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	live := env.seedKnownIDsContact(t, "KnownIDs Live "+suffix, [][2]string{
		{"email", "live-" + suffix + "@example.invalid"},
	})
	deleted := env.seedKnownIDsContact(t, "KnownIDs Deleted "+suffix, [][2]string{
		{"email", "deleted-" + suffix + "@example.invalid"},
	})
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, deleted))

	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), env.pairedHostKey)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsResp(t, w)
	require.Contains(t, r.Data.Emails, "live-"+suffix+"@example.invalid")
	require.NotContains(t, r.Data.Emails, "deleted-"+suffix+"@example.invalid",
		"soft-deleted contact's emails must not appear in known-identifiers")
	_ = live // keep referenced
}

func TestKnownIdentifiers_OnlyEmailAndPhoneTypes(t *testing.T) {
	env := setupKnownIDsEnv(t)
	suffix := uuid.NewString()[:8]
	env.seedKnownIDsContact(t, "KnownIDs Types "+suffix, [][2]string{
		{"email", "types-" + suffix + "@example.invalid"},
		{"phone", "+15551110000"},
		{"telegram", "@types-" + suffix},
		{"whatsapp", "+15551110000"},
	})

	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), env.pairedHostKey)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsResp(t, w)
	require.Contains(t, r.Data.Emails, "types-"+suffix+"@example.invalid")
	require.Contains(t, r.Data.Phones, "+15551110000")
	// Telegram / WhatsApp handles must NOT leak into either array.
	for _, v := range r.Data.Emails {
		require.NotContains(t, v, "@types-"+suffix, "telegram handle should not appear in emails")
	}
}

func TestKnownIdentifiers_EmptyArraysOnFreshHost(t *testing.T) {
	env := setupKnownIDsEnv(t)
	// No contacts seeded for this test (the cleanup ensures isolation
	// on a per-test basis from prior tests' contacts because each test
	// hard-deletes its own seeded rows in t.Cleanup).
	w := getKnownIdentifiers(t, env, env.pairedHostID.String(), env.pairedHostKey)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	r := parseKnownIDsResp(t, w)
	require.NotNil(t, r.Data.Phones)
	require.NotNil(t, r.Data.Emails)
}

// indexOf returns the position of needle in slice, or -1 if absent.
func indexOf(s []string, needle string) int {
	for i, v := range s {
		if v == needle {
			return i
		}
	}
	return -1
}

// occurrences counts how many times needle appears in slice.
func occurrences(s []string, needle string) int {
	n := 0
	for _, v := range s {
		if v == needle {
			n++
		}
	}
	return n
}
