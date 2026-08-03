//go:build integration_testdb

// OAuth route-registration boundary pin against the PRODUCTION wiring.
//
// The registrar-level tests in internal/api/handlers/oauth_routes_test.go
// reconstruct the middleware ordering and provider gates themselves, so they
// cannot catch a regression in THIS package's composition root (e.g.
// RegisterOAuthCallbackRoutes moved behind the API-key middleware in
// routes.go, or the GoogleEnabled derivation broken). These tests drive the
// real wire chain (buildWireChainForGolden's exact order, minus buildTelegram
// per the telegram-skip rule and minus riverClient.Start / the HTTP server)
// through the real registerRoutes per config shape, then probe the resulting
// router. They are the citation holders for SET-001 and SET-005.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oauthWiringTestAPIKey = "oauth-wiring-boundary-test-key-91d4"

// oauthWiringConfig returns a TestConfig over a fresh ephemeral clone DB with
// the API key set so auth.APIKeyMiddleware enforces. TestConfig ships with
// both providers' client credentials configured; shapes mutate from there.
func oauthWiringConfig(t *testing.T) *config.Config {
	t.Helper()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	cfg.Database.MigrationsPath = migrationsPathForTest()
	cfg.External.APIKey = oauthWiringTestAPIKey
	return cfg
}

// buildRouterForOAuthWiring drives the wire chain in run()'s exact order
// (minus buildTelegram — telegram-skip rule — and minus riverClient.Start /
// the HTTP server), then registers the production route tree via the real
// registerRoutes with deps drawn exactly the way run() draws them. The
// returned router is therefore the production route tree for cfg's shape.
func buildRouterForOAuthWiring(t *testing.T, cfg *config.Config) *gin.Engine {
	t.Helper()
	ctx := context.Background()

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	core := buildCoreRepos(database.Queries)

	riverWorkers := river.NewWorkers()
	reg := newRiverRegistrar(riverWorkers)
	addWorker(reg, &noopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:                     riverWorkers,
		ErrorHandler:                events.NewRiverErrorHandler(logger.Get()),
		Logger:                      logger.NewSlogLogger(logger.Get()),
		DiscardedJobRetentionPeriod: riverDiscardedJobRetention,
	})
	require.NoError(t, err)
	reg.periodic = riverClient.PeriodicJobs()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)

	ingest := buildIngestRepos(database.Queries)
	graph := buildGraphCore(database, eventBus)
	messaging := buildMessagingFoundation(database.Queries, ingest.MessagesMessage, ingest.CalendarEvent)
	consumers := buildDomainConsumers(cfg, database, core, graph, eventBus, riverClient)
	contactService := buildContactService(cfg, database, core, graph, consumers, eventBus, riverClient)
	interactionRecorder := buildInteractionRecorder(contactService, messaging, ingest, consumers, eventBus)
	ingestStk := buildIngestStack(database, core, contactService, ingest, messaging, consumers, eventBus, riverClient)
	registerCoreConsumerWorkers(reg, database, core, contactService, interactionRecorder, messaging, consumers, eventBus)
	pubBus, manualHandler := resolveInteractionMode(cfg, database, interactionRecorder, eventBus)
	registerModeWorkers(reg, cfg, database, core, consumers, eventBus, riverClient)
	domain := buildDomainServices(database, core, graph, ingest, consumers, ingestStk, eventBus)
	registerRematchDispatcher(reg, graph, database, eventBus)

	// The external-sync feature gate is exercised through the SAME production
	// seam run() uses — the gate logic itself lives in
	// buildExternalSyncIfEnabled (wire_sync.go), never re-implemented here.
	syncStk := buildExternalSyncIfEnabled(ctx, cfg, database, core, contactService, graph, ingest, messaging, consumers, domain, eventBus, riverClient, pubBus)

	// WhatsApp IS driven here, unlike Telegram: its Start() cannot open a
	// connection (the readiness gate refuses without an ingestor and a drainer),
	// so running the real gate is both safe and the only way to prove the route
	// tree reflects ENABLE_WHATSAPP_SYNC rather than a hand-built handler.
	var whatsappStk whatsappStack
	if cfg.Features.EnableWhatsAppSync {
		whatsappStk = buildWhatsApp(ctx, cfg, database, whatsappPrereqs{})
		if whatsappStk.Manager != nil {
			t.Cleanup(whatsappStk.Manager.Stop)
		}
	}

	// Telegram is intentionally SKIPPED (Start must not run); a nil
	// telegramManager is exactly a telegram-disabled boot for aggregation.
	agg := buildAggregationEngines(database, core, contactService, graph, ingest, messaging, consumers, eventBus, riverClient, nil, syncStk.GChatProvider, syncStk.GChatSyncStates)
	registerMessagingWorkers(reg, ingest, messaging, agg, riverClient)

	handlersCore := buildCoreHandlers(database, core, contactService, graph, cfg, domain.NoteService, manualHandler, eventBus)
	machost := buildMacHost(reg, database, core, ingest)
	staleness := buildStaleness(reg, cfg, database, machost)
	registerAssertionRollover(reg, graph)
	registerSyncScheduler(reg, cfg, syncStk, riverClient)
	registerJobSampleWorkers(reg, repository.NewJobSampleRepository(database.Queries), cfg)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	router.Use(api.ErrorHandlerMiddleware())

	registerRoutes(routeDeps{
		Router:                   router,
		Cfg:                      cfg,
		Database:                 database,
		StalenessService:         staleness.Service,
		MacHostRepo:              machost.Repo,
		MacHostHandler:           machost.Handler,
		IngestHandler:            ingestStk.IngestHandler,
		MeetingNoteHandler:       ingestStk.MeetingNoteHandler,
		ContactHandler:           handlersCore.Contact,
		InteractionHandler:       handlersCore.Interaction,
		NoteHandler:              handlersCore.Note,
		ContactMethodHandler:     handlersCore.ContactMethod,
		RematchHandler:           handlersCore.Rematch,
		StalenessHandler:         staleness.Handler,
		SystemHandler:            handlersCore.System,
		OAuthHandler:             syncStk.OAuthHandler,
		GoogleOAuthService:       syncStk.GoogleOAuthService,
		TodoistHandler:           syncStk.TodoistHandler,
		TelegramHandler:          nil,
		WhatsAppHandler:          whatsappStk.Handler,
		SyncHandler:              syncStk.SyncHandler,
		IdentityHandler:          syncStk.IdentityHandler,
		ContactTaskHandler:       syncStk.ContactTaskHandler,
		CalendarHandler:          syncStk.CalendarHandler,
		ImportHandler:            syncStk.ImportHandler,
		AnarlogDiscoveryHandler:  syncStk.AnarlogDiscoveryHandler,
		SuggestionHandler:        syncStk.SuggestionHandler,
		ExternalContactRepo:      syncStk.ExternalContactRepo,
		ContactService:           contactService,
		MeetingNoteRepoForIngest: ingest.MeetingNote,
	})
	return router
}

func serveOAuthWiringRequest(router *gin.Engine, method, path string, withKey bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if withKey {
		req.Header.Set("X-API-Key", oauthWiringTestAPIKey)
	}
	router.ServeHTTP(w, req)
	return w
}

// oauthWiringRouteSet returns "METHOD path" for every registered route under
// /api/v1/auth.
func oauthWiringRouteSet(router *gin.Engine) []string {
	var routes []string
	for _, route := range router.Routes() {
		if len(route.Path) >= len("/api/v1/auth") && route.Path[:len("/api/v1/auth")] == "/api/v1/auth" {
			routes = append(routes, route.Method+" "+route.Path)
		}
	}
	return routes
}

var wiringGoogleOAuthRouteSet = []string{
	"GET /api/v1/auth/google/callback",
	"GET /api/v1/auth/google",
	"GET /api/v1/auth/google/accounts",
	"GET /api/v1/auth/google/accounts/:id/status",
	"POST /api/v1/auth/google/accounts/:id/revoke",
}

var wiringTodoistOAuthRouteSet = []string{
	"GET /api/v1/auth/todoist/callback",
	"GET /api/v1/auth/todoist",
	"GET /api/v1/auth/todoist/accounts",
	"GET /api/v1/auth/todoist/accounts/:id/status",
	"POST /api/v1/auth/todoist/accounts/:id/revoke",
}

// TestOAuthRouteWiring_AuthBoundary probes the PRODUCTION auth boundary: the
// provider callbacks must sit OUTSIDE the API-key middleware (a keyless
// request reaches the handler), while every auth-URL and account-management
// route sits inside it (keyless is 401, keyed proceeds).
func TestOAuthRouteWiring_AuthBoundary(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	cfg := oauthWiringConfig(t)
	cfg.Features.EnableExternalSync = true
	router := buildRouterForOAuthWiring(t, cfg)

	t.Run("callback routes are reachable without the API key", func(t *testing.T) {
		// spec: SET-001
		// A keyless request to each provider callback must get PAST the auth
		// middleware and into the handler. With no query params the handler
		// deterministically 302-redirects to the settings surface with an
		// invalid_state outcome — proof it executed.
		for provider, path := range map[string]string{
			"google":  "/api/v1/auth/google/callback",
			"todoist": "/api/v1/auth/todoist/callback",
		} {
			w := serveOAuthWiringRequest(router, http.MethodGet, path, false)
			assert.NotEqual(t, http.StatusUnauthorized, w.Code,
				"%s callback must not be rejected by the API-key middleware", provider)
			require.Equal(t, http.StatusFound, w.Code,
				"%s callback handler must run and redirect", provider)
			location := w.Header().Get("Location")
			assert.Contains(t, location, "/settings?auth=error", "provider %s", provider)
			assert.Contains(t, location, "provider="+provider)
		}
	})

	t.Run("auth-URL and account routes without a key are rejected 401", func(t *testing.T) {
		// spec: SET-001
		protected := []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/v1/auth/google"},
			{http.MethodGet, "/api/v1/auth/google/accounts"},
			{http.MethodGet, fmt.Sprintf("/api/v1/auth/google/accounts/%s/status", uuid.New())},
			{http.MethodPost, fmt.Sprintf("/api/v1/auth/google/accounts/%s/revoke", uuid.New())},
			{http.MethodGet, "/api/v1/auth/todoist"},
			{http.MethodGet, "/api/v1/auth/todoist/accounts"},
			{http.MethodGet, fmt.Sprintf("/api/v1/auth/todoist/accounts/%s/status", uuid.New())},
			{http.MethodPost, fmt.Sprintf("/api/v1/auth/todoist/accounts/%s/revoke", uuid.New())},
		}
		for _, route := range protected {
			w := serveOAuthWiringRequest(router, route.method, route.path, false)
			name := route.method + " " + route.path
			require.Equal(t, http.StatusUnauthorized, w.Code,
				"%s without an API key must be rejected by the auth middleware", name)
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "route %s", name)
			assert.Equal(t, false, body["success"], "route %s", name)
			errObj, ok := body["error"].(map[string]interface{})
			require.True(t, ok, "route %s: 401 body must carry an error object", name)
			assert.Equal(t, "MISSING_API_KEY", errObj["code"], "route %s", name)
		}
	})

	t.Run("auth-URL routes with the key reach their handlers", func(t *testing.T) {
		// spec: SET-001
		// Acceptance side of the boundary: the same protected routes proceed
		// once the key is supplied (the real OAuth services build the
		// provider auth URLs locally — no network involved).
		wGoogle := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/auth/google", true)
		require.Equal(t, http.StatusOK, wGoogle.Code)
		assert.Contains(t, wGoogle.Body.String(), "accounts.google.com")

		wTodoist := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/auth/todoist", true)
		require.Equal(t, http.StatusOK, wTodoist.Code)
		assert.Contains(t, wTodoist.Body.String(), "todoist.com")
	})
}

// TestOAuthRouteWiring_ProviderGating pins the PRODUCTION provider gates: the
// wire chain must derive nil OAuth services from external-sync/credential
// config, and registerRoutes must translate those nils into absent routes.
func TestOAuthRouteWiring_ProviderGating(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Run("external sync enabled with both providers configured registers both full route sets", func(t *testing.T) {
		// spec: SET-005
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		router := buildRouterForOAuthWiring(t, cfg)

		assert.ElementsMatch(t,
			append(append([]string{}, wiringGoogleOAuthRouteSet...), wiringTodoistOAuthRouteSet...),
			oauthWiringRouteSet(router))
	})

	t.Run("external sync disabled registers no oauth routes for either provider", func(t *testing.T) {
		// spec: SET-005
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = false
		router := buildRouterForOAuthWiring(t, cfg)

		assert.Empty(t, oauthWiringRouteSet(router),
			"with external sync disabled no /api/v1/auth route may exist")
		for _, path := range []string{"/api/v1/auth/google/callback", "/api/v1/auth/todoist/callback"} {
			w := serveOAuthWiringRequest(router, http.MethodGet, path, false)
			assert.Equal(t, http.StatusNotFound, w.Code,
				"disabled provider callback %s must 404", path)
		}
	})

	t.Run("google credentials unconfigured derives a nil google service and absent google routes", func(t *testing.T) {
		// spec: SET-005
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		cfg.Google.ClientID = ""
		cfg.Google.ClientSecret = ""
		router := buildRouterForOAuthWiring(t, cfg)

		assert.ElementsMatch(t, wiringTodoistOAuthRouteSet, oauthWiringRouteSet(router))
		w := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/auth/google/callback", false)
		assert.Equal(t, http.StatusNotFound, w.Code,
			"an unconfigured provider's callback URL must return 404")
	})

	t.Run("todoist credentials unconfigured derives a nil todoist service and absent todoist routes", func(t *testing.T) {
		// spec: SET-005
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		cfg.Todoist.ClientID = ""
		cfg.Todoist.ClientSecret = ""
		router := buildRouterForOAuthWiring(t, cfg)

		assert.ElementsMatch(t, wiringGoogleOAuthRouteSet, oauthWiringRouteSet(router))
		w := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/auth/todoist/callback", false)
		assert.Equal(t, http.StatusNotFound, w.Code,
			"an unconfigured provider's callback URL must return 404")
	})

	// Partially-configured shapes: the derivation requires BOTH credential
	// fields, so a provider with only one populated must expose no routes. An
	// erroneous OR in the credential check passes the all-empty shapes above
	// but fails these.
	partialShapes := []struct {
		name  string
		setup func(cfg *config.Config)
		want  []string
		probe string
	}{
		{"google client id only", func(cfg *config.Config) { cfg.Google.ClientSecret = "" }, wiringTodoistOAuthRouteSet, "/api/v1/auth/google/callback"},
		{"google client secret only", func(cfg *config.Config) { cfg.Google.ClientID = "" }, wiringTodoistOAuthRouteSet, "/api/v1/auth/google/callback"},
		{"todoist client id only", func(cfg *config.Config) { cfg.Todoist.ClientSecret = "" }, wiringGoogleOAuthRouteSet, "/api/v1/auth/todoist/callback"},
		{"todoist client secret only", func(cfg *config.Config) { cfg.Todoist.ClientID = "" }, wiringGoogleOAuthRouteSet, "/api/v1/auth/todoist/callback"},
	}
	for _, shape := range partialShapes {
		t.Run("partially configured: "+shape.name+" exposes no routes for that provider", func(t *testing.T) {
			// spec: SET-005
			t.Parallel()
			cfg := oauthWiringConfig(t)
			cfg.Features.EnableExternalSync = true
			shape.setup(cfg)
			router := buildRouterForOAuthWiring(t, cfg)

			assert.ElementsMatch(t, shape.want, oauthWiringRouteSet(router))
			w := serveOAuthWiringRequest(router, http.MethodGet, shape.probe, false)
			assert.Equal(t, http.StatusNotFound, w.Code,
				"a partially-configured provider's callback URL must return 404")
		})
	}
}
