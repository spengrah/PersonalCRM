package main

import (
	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// routeDeps carries everything registerRoutes needs to register the gin
// route tree. Conditional gates (feature flags, nil-handler checks,
// CRM_ENV) live at their call sites inside registerRoutes exactly as they
// did inline in run().
type routeDeps struct {
	Router   *gin.Engine
	Cfg      *config.Config
	Database *db.Database

	// Health
	StalenessService *service.StalenessService

	// Mac-daemon host management
	MacHostRepo    *repository.MacHostRepository
	MacHostHandler *handlers.MacHostHandler

	// Ingest
	IngestHandler      *handlers.IngestHandler
	MeetingNoteHandler *handlers.MeetingNoteHandler

	// Core (always-on)
	ContactHandler       *handlers.ContactHandler
	InteractionHandler   *handlers.InteractionHandler
	NoteHandler          *handlers.NoteHandler
	ContactMethodHandler *handlers.ContactMethodHandler
	RematchHandler       *handlers.RematchHandler
	StalenessHandler     *handlers.StalenessHandler
	SystemHandler        *handlers.SystemHandler

	// OAuth / external sync (feature-flagged / nil when unconfigured)
	OAuthHandler            *handlers.OAuthHandler
	GoogleOAuthService      *google.OAuthService
	TodoistHandler          *handlers.TodoistHandler
	TelegramHandler         *handlers.TelegramHandler
	SyncHandler             *handlers.SyncHandler
	IdentityHandler         *handlers.IdentityHandler
	ContactTaskHandler      *handlers.ContactTaskHandler
	CalendarHandler         *handlers.CalendarHandler
	ImportHandler           *handlers.ImportHandler
	AnarlogDiscoveryHandler *handlers.AnarlogDiscoveryHandler
	SuggestionHandler       *handlers.SuggestionHandler

	// Test seeding (CRM_ENV=testing/test)
	ExternalContactRepo      *repository.ExternalContactRepository
	ContactService           *service.ContactService
	MeetingNoteRepoForIngest *repository.MeetingNoteRepository
}

// registerRoutes registers the health endpoint, OAuth callbacks, mac-host
// routes, the ingest endpoint, the /api/v1 authenticated group (with all
// conditional sub-routes), and swagger — preserving the exact registration
// order + gates from the original run() body.
func registerRoutes(deps routeDeps) {
	router := deps.Router
	cfg := deps.Cfg
	database := deps.Database

	// Health check endpoint. Bare /health is the liveness probe (DB-only
	// top-level status, today's contract); ?ready=1 aggregates the
	// river/sync/disk components for an external pinger. The stalenessService
	// (constructed above) and a HealthRepository over river_job back the new
	// components; the watchdog kind ties the sync component's freshness guard to
	// the periodic watchdog job registered on the same River client.
	healthRepo := repository.NewHealthRepository(database.Queries)
	healthChecker := health.NewHealthChecker(database, cfg.Database.HealthTimeout, health.Deps{
		River:            healthRepo,
		Staleness:        deps.StalenessService,
		SyncWatchdogKind: scheduler.StalenessWatchdogArgs{}.Kind(),
		Thresholds: health.Thresholds{
			RiverDiscardedMax:  cfg.Health.RiverDiscardedMax,
			RiverOldestDueMax:  cfg.Health.RiverOldestDueMax,
			SyncWatchdogMaxAge: cfg.Health.SyncWatchdogMaxAge,
			DiskPath:           cfg.Health.DiskPath,
			DiskMinFreePercent: cfg.Health.DiskMinFreePercent,
		},
	})
	router.GET("/health", healthChecker.Handler)

	// OAuth callback routes (no auth - called by provider redirects)
	if deps.OAuthHandler != nil {
		handlers.RegisterOAuthCallbackRoutes(router, handlers.OAuthCallbackDeps{
			Handler:       deps.OAuthHandler,
			GoogleEnabled: deps.GoogleOAuthService != nil,
		})
	}

	// Mac-daemon public + host-auth routes (Pair + heartbeat + cursor +
	// known-ids). Registered via the shared helper so integration tests
	// exercise the same code path. Admin routes are registered later
	// inside the global-API-key-protected v1 group.
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    deps.MacHostRepo,
		Handler:     deps.MacHostHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})

	// Event bus ingestion endpoint (feature-flagged per spec §3.9).
	// Registered as a SIBLING of /api/v1 (not inside it) so the
	// composite IngestAuthMiddleware can branch per-request:
	//   - X-Mac-Host-ID present → MacHostAuthMiddleware (daemon path)
	//   - X-Mac-Host-ID absent  → APIKeyMiddleware (global-key path)
	// gin route trees reject duplicate registration of the same prefix
	// under different middleware groups, so the composite dispatch is
	// the minimum seam to support both auth paths on one URL.
	if cfg.Features.EnableEventBusIngest {
		ingestAuth := auth.IngestAuthMiddleware(
			auth.APIKeyMiddleware(cfg),
			auth.MacHostAuthMiddleware(deps.MacHostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
		)
		handlers.RegisterIngestRoutes(router, handlers.IngestRouteDeps{
			Auth:        ingestAuth,
			Ingest:      deps.IngestHandler,
			MeetingNote: deps.MeetingNoteHandler,
		})
		logger.Info().Msg("event bus ingestion endpoint enabled")
	}

	// API routes
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		// Contact + interaction routes (unconditional).
		handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
			Contact:       deps.ContactHandler,
			Interaction:   deps.InteractionHandler,
			Note:          deps.NoteHandler,
			ContactMethod: deps.ContactMethodHandler,
		})

		// Meeting-note conflict-resolution — user-driven, called from
		// the frontend with the global API key. Stays under the v1
		// APIKeyMiddleware group.
		handlers.RegisterMeetingNoteRoutes(v1, deps.MeetingNoteHandler)

		// Rematch routes — always registered; service no-ops when no handlers
		// are registered (e.g. telegram-disabled deployments still get calendar).
		handlers.RegisterRematchRoutes(v1, deps.RematchHandler)

		// Sync-staleness breaches — registered unconditionally (OUTSIDE the
		// EnableExternalSync-gated /sync group below): heartbeat/push breaches
		// must be visible even with external sync off. The static 2-segment
		// path coexists with that group's 3-segment param routes.
		handlers.RegisterSyncStalenessRoutes(v1, deps.StalenessHandler)

		// System routes
		handlers.RegisterSystemRoutes(v1, deps.SystemHandler)

		// Mac-daemon admin routes (under global API key middleware).
		// Pairing-token mint + revoke + list/get for the Mac settings UI.
		handlers.RegisterMacHostAdminRoutes(v1, deps.MacHostHandler)

		// OAuth routes (feature-flagged with external sync)
		if deps.OAuthHandler != nil {
			handlers.RegisterOAuthRoutes(v1, handlers.OAuthCallbackDeps{
				Handler:       deps.OAuthHandler,
				GoogleEnabled: deps.GoogleOAuthService != nil,
			})
		}

		// Todoist settings routes (only if Todoist is configured)
		if deps.TodoistHandler != nil {
			handlers.RegisterTodoistRoutes(v1, deps.TodoistHandler)
		}

		// Telegram routes (feature-flagged)
		if deps.TelegramHandler != nil {
			handlers.RegisterTelegramRoutes(v1, deps.TelegramHandler)
		}

		// External sync routes (feature-flagged)
		if cfg.Features.EnableExternalSync && deps.SyncHandler != nil {
			handlers.RegisterSyncRoutes(v1, deps.SyncHandler)
			handlers.RegisterIdentityRoutes(v1, deps.IdentityHandler)

			// Add contact task routes (manual tasks) if Todoist is configured
			if deps.ContactTaskHandler != nil {
				handlers.RegisterContactTaskRoutes(v1, deps.ContactTaskHandler)
			}

			// Add calendar event routes to contacts if calendar handler is initialized
			if deps.CalendarHandler != nil {
				handlers.RegisterCalendarRoutes(v1, deps.CalendarHandler)
			}

			// Import candidates routes
			if deps.ImportHandler != nil {
				handlers.RegisterImportRoutes(v1, handlers.ImportRouteDeps{
					Import:           deps.ImportHandler,
					AnarlogDiscovery: deps.AnarlogDiscoveryHandler,
					Suggestions:      deps.SuggestionHandler,
				})
			}
		}

		// Export/Import routes
		handlers.RegisterDataExchangeRoutes(v1, deps.SystemHandler)

		// Test routes (gated by CRM_ENV=testing or CRM_ENV=test)
		if cfg.Runtime.CRMEnvironment == "testing" || cfg.Runtime.CRMEnvironment == "test" {
			// Calendar repo for the test-env calendar handler below, so a test
			// environment without OAuth can still READ events the declared seed wrote.
			testCalendarRepo := repository.NewCalendarEventRepository(database.Queries)

			// Initialize calendar handler if not already done (allows reading seeded events in tests)
			calendarHandler := deps.CalendarHandler
			if calendarHandler == nil {
				calendarHandler = handlers.NewCalendarHandler(testCalendarRepo)
				// Register calendar routes that weren't registered earlier (OAuth not configured)
				handlers.RegisterCalendarRoutes(v1, calendarHandler)
				logger.Info().Msg("calendar handler initialized for testing (no OAuth)")
			}

			testSeedService := service.NewTestSeedService(database, deps.MeetingNoteRepoForIngest)
			testLockService := service.NewTestLockService(accelerated.GetCurrentTime)
			// The database is passed for the DECLARED seeding path only, which
			// drives internal/synthetic/declare from the handler with no service
			// in between: service cannot import synthetic (service → synthetic →
			// replay → service cycles), and the toolkit writes through the real
			// services itself, so the layer rule holds one level down. Documented
			// on the TestHandler type too; do not "fix" it by minting a service.
			testHandler := handlers.NewTestHandler(testSeedService, testLockService, database)
			handlers.RegisterTestRoutes(v1, testHandler)
			logger.Info().Msg("test API endpoints enabled (CRM_ENV=testing)")
		}
	}

	// Swagger documentation
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
}
