// @title Personal CRM API
// @version 1.0
// @description A personal customer relationship management API
// @termsOfService http://swagger.io/terms/

// @contact.name API Support
// @contact.url http://www.swagger.io/support
// @contact.email support@swagger.io

// @license.name MIT
// @license.url https://opensource.org/licenses/MIT

// @host localhost:8080
// @BasePath /api/v1
// @schemes http https

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Type "Bearer" followed by a space and JWT token.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"
	tgpkg "personal-crm/backend/internal/telegram"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "personal-crm/backend/docs" // Import generated docs
)

// noopJobArgs is a throwaway job type used only to prove that River's
// AddWorker / client registration wiring works end-to-end in PR 1 of #180.
// No jobs of this kind are ever enqueued at runtime. It will be removed in
// PR 2 once a real consumer worker (the InteractionRecorder wrapper per
// .ai/spec/event-bus-foundation.md §3.4) is registered.
//
// Kept inline in main.go (rather than a sibling file) because the repo's
// build convention is `go build cmd/crm-api/main.go` (single-file), used
// by the Makefile, CI cross-compile, and the Playwright webServer command.
type noopJobArgs struct{}

// Kind returns the River job kind identifier for noopJobArgs.
func (noopJobArgs) Kind() string { return "noop" }

// noopWorker is a no-op River worker paired with noopJobArgs.
type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

// Work implements river.Worker. Since no jobs of this kind are enqueued,
// this method is never called at runtime.
func (*noopWorker) Work(_ context.Context, _ *river.Job[noopJobArgs]) error {
	return nil
}

func main() {
	// Run the server body in a helper so its defers (database.Close,
	// telegramManager.Stop, cronScheduler.Stop, shutdown-ctx cancel) all
	// execute on a normal return — os.Exit would bypass them. The only
	// non-zero exit path is a failed graceful HTTP shutdown, signalled
	// via the return value.
	os.Exit(run())
}

func run() int {
	// Load and validate configuration first (before logger)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize structured logger with configuration
	logger.Init(cfg.Logger)

	logger.Info().
		Str("environment", cfg.Logger.Environment).
		Str("log_level", cfg.Logger.Level).
		Msg("configuration loaded successfully")

	// Run migrations before connecting to database (applies both our
	// golang-migrate migrations and River's queue schema).
	ctx := context.Background()
	logger.Info().Msg("running database migrations")
	if err := db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath); err != nil {
		logger.Fatal().Err(err).Msg("failed to run migrations")
	}

	// Initialize database
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer database.Close()

	logger.Info().Msg("database connected successfully")

	// Build the River worker set. In PR 1 only a no-op worker was registered;
	// PR 5 adds the InteractionRecorder consumer. The client is built BELOW
	// after the consumer is instantiated (AddWorker must happen before Start).
	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, &noopWorker{})

	// Initialize repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	// Shadow-observation repo is constructed unconditionally. When
	// InteractionMode=off the direct path never calls RecordDirectWrite and
	// no publisher emits events, so the repo's queries stay idle — zero
	// runtime cost.
	shadowObsRepo := repository.NewShadowObservationRepository(database.Queries, database.Pool)

	// Construct river client (before Start; consumer worker is added below).
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build river client")
	}

	// Event bus — first in-process publisher arrived in PR 4 (HTTP ingestion).
	// Constructed unconditionally: the Bus is three pointers and zero runtime
	// cost when nothing publishes. The ingest route is gated by
	// cfg.Features.EnableEventBusIngest; the shadow publishers are gated by
	// cfg.EventBus.InteractionMode below.
	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)
	ingestService := service.NewIngestService(database, eventBus)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	// Initialize services
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)

	// Compute effective InteractionMode. PR 5 implements only shadow;
	// cutover is forward-compat config and falls back to shadow in this
	// release so an early-set cutover still produces useful bake evidence
	// (plan Decision 12). Warn loudly so a misconfigured bake is visible.
	effectiveMode := cfg.EventBus.InteractionMode
	if effectiveMode == consumer.InteractionModeCutover {
		logger.Warn().
			Msg("EVENT_BUS_INTERACTION_MODE=cutover requested but PR 5 only implements shadow; treating as shadow")
		effectiveMode = consumer.InteractionModeShadow
	}

	// PR 5 InteractionRecorder consumer + shadow helper (spec §3.4.1, plan
	// Decisions 7/11). The consumer receives events from the 5 async-
	// publisher kinds via the river worker and inline-invokes from the
	// manual UI flow through ManualInteractionShadow.
	interactionRecorder := consumer.NewInteractionRecorder(
		effectiveMode,
		contactRepo,
		interactionRepo,
		eventBus,
		shadowObsRepo,
	)
	manualShadow := service.NewManualInteractionShadow(
		effectiveMode,
		database.Pool,
		eventBus,
		interactionRecorder,
	)

	// Register the consumer worker. The worker is registered UNCONDITIONALLY —
	// river rejects unknown job kinds at dequeue time, so having the worker
	// present with mode=off costs nothing (no events route to it). Mode
	// gating happens at the publisher sites via pubBus.
	river.AddWorker(riverWorkers, consumer.NewInteractionRecorderWorker(eventBus, database.Pool, interactionRecorder))

	// pubBus is the bus reference threaded into publisher constructors.
	// Nil when InteractionMode=off so publishers silently skip their
	// shadow emits — zero observable behavior change vs. today's code.
	var pubBus *events.Bus
	if effectiveMode != consumer.InteractionModeOff {
		pubBus = eventBus
		contactService.SetShadowObserver(shadowObsRepo)
		logger.Info().
			Str("mode", effectiveMode).
			Msg("event-bus shadow mode enabled")
	}

	if err := riverClient.Start(ctx); err != nil {
		logger.Fatal().Err(err).Msg("failed to start river client")
	}
	logger.Info().
		Int("worker_concurrency", cfg.River.WorkerConcurrency).
		Msg("river client started")
	noteService := service.NewNoteService(noteRepo, contactRepo)
	importMatchService := service.NewImportMatchService(contactRepo)
	// EnrichmentService is shared by the import handler (link/import flows) and
	// the Telegram peer matcher (auto-match enrichment). Constructed at outer
	// scope so both feature blocks share a single instance.
	enrichmentService := service.NewEnrichmentService(contactRepo, contactMethodRepo, enrichmentRepo)

	// Rematch service — wired now so downstream services can call SetRematchService.
	// Handlers are registered below once their dependencies are constructed.
	rematchService := service.NewRematchService()
	contactService.SetRematchService(rematchService)
	enrichmentService.SetRematchService(rematchService)

	// Initialize external sync components (feature-flagged)
	var syncService *service.SyncService
	var syncHandler *handlers.SyncHandler
	var identityHandler *handlers.IdentityHandler
	var oauthHandler *handlers.OAuthHandler
	var importHandler *handlers.ImportHandler
	var calendarHandler *handlers.CalendarHandler
	var todoistHandler *handlers.TodoistHandler
	var contactTaskHandler *handlers.ContactTaskHandler
	var googleOAuthService *google.OAuthService
	var todoistOAuthService *todoist.OAuthService
	var externalContactRepo *repository.ExternalContactRepository

	if cfg.Features.EnableExternalSync {
		syncRepo := repository.NewSyncRepository(database.Queries)
		identityRepo := repository.NewIdentityRepository(database.Queries)
		oauthRepo := repository.NewOAuthRepository(database.Queries)
		providerRegistry := sync.NewProviderRegistry()

		// Initialize Google OAuth service if configured
		if cfg.Google.ClientID != "" && cfg.Google.ClientSecret != "" {
			var err error
			googleOAuthService, err = google.NewOAuthService(cfg, oauthRepo, syncRepo)
			if err != nil {
				logger.Warn().Err(err).Msg("failed to initialize Google OAuth service")
			} else {
				oauthHandler = handlers.NewOAuthHandler(googleOAuthService, cfg.CORS.FrontendURL)
				logger.Info().Msg("Google OAuth service initialized")
			}
		} else {
			logger.Info().Msg("Google OAuth not configured (GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET required)")
		}

		// Initialize Todoist OAuth service if configured
		if cfg.Todoist.ClientID != "" && cfg.Todoist.ClientSecret != "" {
			var err error
			todoistOAuthService, err = todoist.NewOAuthService(cfg, oauthRepo, syncRepo)
			if err != nil {
				logger.Warn().Err(err).Msg("failed to initialize Todoist OAuth service")
			} else {
				// If OAuth handler exists (from Google), add Todoist to it
				// Otherwise create a new handler with nil Google service
				if oauthHandler != nil {
					oauthHandler.SetTodoistOAuth(todoistOAuthService)
				} else {
					oauthHandler = handlers.NewOAuthHandler(nil, cfg.CORS.FrontendURL)
					oauthHandler.SetTodoistOAuth(todoistOAuthService)
				}

				// Initialize Todoist settings handler
				todoistHandler = handlers.NewTodoistHandler(todoistOAuthService, syncRepo)

				logger.Info().Msg("Todoist OAuth service initialized")
			}
		} else {
			logger.Info().Msg("Todoist OAuth not configured (TODOIST_CLIENT_ID and TODOIST_CLIENT_SECRET required)")
		}

		// Initialize external contact repository
		externalContactRepo = repository.NewExternalContactRepository(database.Queries)

		// Initialize identity service (enrichmentService is constructed at outer scope
		// so the Telegram block can share it).
		identityService := service.NewIdentityService(identityRepo)

		// Calendar repo + handler + rematch handler are wired whenever external
		// sync is enabled, regardless of OAuth configuration. Rematch over
		// calendar_event is pure DB work and must run in test/local environments
		// that don't have Google OAuth set up.
		calendarRepo := repository.NewCalendarEventRepository(database.Queries)
		calendarHandler = handlers.NewCalendarHandler(calendarRepo)
		rematchService.Register(google.NewCalendarRematchHandler(calendarRepo, externalContactRepo, contactService, pubBus))
		logger.Info().Msg("Calendar rematch handler registered")

		// Register Google Contacts provider if OAuth is configured
		if googleOAuthService != nil {
			gcontactsProvider := google.NewContactsProvider(
				googleOAuthService,
				externalContactRepo,
				enrichmentService,
				identityService,
			)
			providerRegistry.Register(gcontactsProvider)
			logger.Info().Msg("Google Contacts sync provider registered")

			// Register Google Calendar provider
			gcalProvider := google.NewCalendarSyncProvider(
				googleOAuthService,
				calendarRepo,
				contactRepo,
				identityService,
				externalContactRepo,
				contactService,
				pubBus,
			)
			providerRegistry.Register(gcalProvider)
			logger.Info().Msg("Google Calendar sync provider registered")
		}

		// Register Todoist Cadence provider if OAuth is configured
		if todoistOAuthService != nil {
			todoistProvider := todoist.NewCadenceSyncProvider(
				todoistOAuthService,
				contactTaskRepo,
				contactRepo,
				syncRepo,
				cfg,
				contactService,
			)
			providerRegistry.Register(todoistProvider)
			logger.Info().Msg("Todoist Cadence sync provider registered")

			// Initialize follow-up service and inject into contact service
			followUpService := service.NewFollowUpService(
				contactTaskRepo,
				contactRepo,
				syncRepo,
				todoistOAuthService,
				cfg,
			)
			contactService.SetFollowUpManager(followUpService)
			todoistProvider.SetFollowUpCloser(followUpService)
			logger.Info().Msg("Follow-up service initialized")

			// Initialize contact task service and handler for action tasks
			contactTaskService := service.NewContactTaskService(
				contactTaskRepo,
				contactRepo,
				syncRepo,
				todoistOAuthService,
				cfg,
			)
			contactTaskHandler = handlers.NewContactTaskHandler(contactTaskService)
			logger.Info().Msg("Contact task handler initialized")
		}

		syncService = service.NewSyncService(syncRepo, contactRepo, providerRegistry)

		syncHandler = handlers.NewSyncHandler(syncService)
		identityHandler = handlers.NewIdentityHandler(identityService)

		// Initialize import handler
		importHandler = handlers.NewImportHandler(externalContactRepo, contactService, importMatchService, enrichmentService)

		logger.Info().Msg("external sync infrastructure enabled")
	}

	// Telegram integration (independent of external sync)
	var telegramManager *tgpkg.TelegramManager
	var telegramHandler *handlers.TelegramHandler

	if cfg.Features.EnableTelegramSync && cfg.External.TelegramAPIID != 0 {
		telegramSessionRepo := repository.NewTelegramSessionRepository(database.Queries)
		telegramUpdateStateRepo := repository.NewTelegramUpdateStateRepository(database.Queries)
		telegramChatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
		telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)
		telegramSyncRepo := repository.NewSyncRepository(database.Queries)

		// Phase 4: identity + aggregation dependencies
		tgIdentityRepo := repository.NewIdentityRepository(database.Queries)
		tgIdentityService := service.NewIdentityService(tgIdentityRepo)
		tgExternalContactRepo := repository.NewExternalContactRepository(database.Queries)

		encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to initialize Telegram encryptor (TOKEN_ENCRYPTION_KEY required)")
		}

		telegramManager = tgpkg.NewTelegramManager(
			telegramSessionRepo,
			telegramUpdateStateRepo,
			telegramChatConfigRepo,
			telegramMessageRepo,
			telegramSyncRepo,
			encryptor,
			cfg.External.TelegramAPIID,
			cfg.External.TelegramAPIHash,
			&cfg.Telegram,
			tgIdentityService,
			tgExternalContactRepo,
			enrichmentService,
			interactionRepo,
			contactService,
			contactService,
			contactService,
			pubBus,
		)

		if err := telegramManager.Start(ctx); err != nil {
			logger.Warn().Err(err).Msg("failed to start Telegram connection")
		}
		defer telegramManager.Stop()

		telegramHandler = handlers.NewTelegramHandler(telegramManager)

		// Register telegram rematch handlers (telegram + phone identifiers)
		// against the same matcher/aggregator instances the manager owns so
		// rematch behavior is identical to the post-import path.
		rematchService.Register(tgpkg.NewUsernameRematchHandler(telegramMessageRepo, telegramManager.PeerMatcher(), telegramManager.AggregationEngine()))
		rematchService.Register(tgpkg.NewPhoneRematchHandler(telegramMessageRepo, telegramManager.PeerMatcher(), telegramManager.AggregationEngine()))
		logger.Info().Msg("Telegram rematch handlers registered")

		logger.Info().Msg("Telegram integration initialized")
	}

	// Wire Telegram post-import hook (if both Telegram and imports are enabled)
	if telegramManager != nil && importHandler != nil {
		importHandler.SetPostImportHook(telegramManager)
	}

	// Initialize handlers
	contactHandler := handlers.NewContactHandler(contactService, manualShadow)
	noteHandler := handlers.NewNoteHandler(noteService)
	interactionHandler := handlers.NewInteractionHandler(contactService, interactionRepo, manualShadow)
	systemHandler := handlers.NewSystemHandler(contactRepo, cfg.Runtime)
	rematchHandler := handlers.NewRematchHandler(rematchService, contactService)

	// Initialize and start scheduler
	cronScheduler := scheduler.NewScheduler(syncService, cfg.Features.EnableExternalSync)
	if err := cronScheduler.Start(); err != nil {
		logger.Fatal().Err(err).Msg("failed to start scheduler")
	}
	defer cronScheduler.Stop()

	// Set up Gin router
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Add middleware
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	router.Use(api.ErrorHandlerMiddleware())

	// Health check endpoint
	healthChecker := health.NewHealthChecker(database, cfg.Database.HealthTimeout)
	router.GET("/health", healthChecker.Handler)

	// OAuth callback routes (no auth - called by provider redirects)
	if oauthHandler != nil {
		if googleOAuthService != nil {
			router.GET("/api/v1/auth/google/callback", oauthHandler.GoogleCallback)
		}
		if oauthHandler.HasTodoistOAuth() {
			router.GET("/api/v1/auth/todoist/callback", oauthHandler.TodoistCallback)
		}
	}

	// API routes
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		// Contact routes
		contacts := v1.Group("/contacts")
		{
			contacts.POST("", contactHandler.CreateContact)
			contacts.GET("/overdue", contactHandler.ListOverdueContacts)
			contacts.GET("", contactHandler.ListContacts)
			contacts.GET("/:id", contactHandler.GetContact)
			contacts.PUT("/:id", contactHandler.UpdateContact)
			contacts.DELETE("/:id", contactHandler.DeleteContact)
			contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
			contacts.GET("/:id/interactions", interactionHandler.ListContactInteractions)
			contacts.POST("/:id/interactions", interactionHandler.CreateInteraction)
			contacts.GET("/:id/notes", noteHandler.GetContactNotepad)
			contacts.PUT("/:id/notes", noteHandler.SaveContactNotepad)
			// Merge routes
			contacts.GET("/:id/merge/preview", contactHandler.GetMergePreview)
			contacts.POST("/:id/merge", contactHandler.MergeContacts)
		}

		// Interaction routes (non-contact-scoped)
		interactions := v1.Group("/interactions")
		{
			interactions.DELETE("/:id", interactionHandler.DeleteInteraction)
		}

		// Rematch routes — always registered; service no-ops when no handlers
		// are registered (e.g. telegram-disabled deployments still get calendar).
		rematchRoutes := v1.Group("/rematch")
		{
			rematchRoutes.GET("/jobs/:jobID", rematchHandler.GetJob)
			rematchRoutes.POST("/contacts/:id/rescan", rematchHandler.Rescan)
		}

		// System routes
		system := v1.Group("/system")
		{
			system.GET("/time", systemHandler.GetSystemTime)
			system.POST("/time/acceleration", systemHandler.SetTimeAcceleration)
		}

		// OAuth routes (feature-flagged with external sync)
		if oauthHandler != nil {
			authRoutes := v1.Group("/auth")
			{
				// Google OAuth (only if configured)
				if googleOAuthService != nil {
					authRoutes.GET("/google", oauthHandler.GetGoogleAuthURL)
					authRoutes.GET("/google/accounts", oauthHandler.ListGoogleAccounts)
					authRoutes.GET("/google/accounts/:id/status", oauthHandler.GetGoogleAccountStatus)
					authRoutes.POST("/google/accounts/:id/revoke", oauthHandler.RevokeGoogleAccount)
				}

				// Todoist OAuth (only if configured)
				if oauthHandler.HasTodoistOAuth() {
					authRoutes.GET("/todoist", oauthHandler.GetTodoistAuthURL)
					authRoutes.GET("/todoist/accounts", oauthHandler.ListTodoistAccounts)
					authRoutes.GET("/todoist/accounts/:id/status", oauthHandler.GetTodoistAccountStatus)
					authRoutes.POST("/todoist/accounts/:id/revoke", oauthHandler.RevokeTodoistAccount)
				}
			}
		}

		// Todoist settings routes (only if Todoist is configured)
		if todoistHandler != nil {
			todoistRoutes := v1.Group("/todoist")
			{
				todoistRoutes.GET("/settings", todoistHandler.GetSettings)
				todoistRoutes.PATCH("/settings", todoistHandler.UpdateSettings)
				todoistRoutes.GET("/projects", todoistHandler.ListProjects)
				todoistRoutes.GET("/labels", todoistHandler.ListLabels)
			}
		}

		// Telegram routes (feature-flagged)
		if telegramHandler != nil {
			tgRoutes := v1.Group("/telegram")
			{
				tgAuth := tgRoutes.Group("/auth")
				{
					tgAuth.POST("/start", telegramHandler.StartAuth)
					tgAuth.POST("/verify-code", telegramHandler.VerifyCode)
					tgAuth.POST("/verify-password", telegramHandler.VerifyPassword)
					tgAuth.POST("/cancel", telegramHandler.CancelAuth)
					tgAuth.DELETE("", telegramHandler.Disconnect)
					tgAuth.GET("/status", telegramHandler.GetStatus)
				}
				tgChats := tgRoutes.Group("/chats")
				{
					tgChats.GET("", telegramHandler.ListChats)
					tgChats.PATCH("/:chat_id", telegramHandler.UpdateChatStatus)
				}
			}
		}

		// External sync routes (feature-flagged)
		if cfg.Features.EnableExternalSync && syncHandler != nil {
			syncRoutes := v1.Group("/sync")
			{
				syncRoutes.GET("/status", syncHandler.GetSyncStatus)
				syncRoutes.GET("/providers", syncHandler.GetAvailableProviders)
				syncRoutes.GET("/logs", syncHandler.GetRecentSyncLogs)
				// Source-based routes (by source name like "gmail", "calendar")
				syncRoutes.GET("/:source/status", syncHandler.GetSyncState)
				syncRoutes.POST("/:source/trigger", syncHandler.TriggerSync)
				// State-based routes (by sync state UUID)
				syncRoutes.PATCH("/states/:id/enable", syncHandler.EnableSync)
				syncRoutes.GET("/states/:id/logs", syncHandler.GetSyncLogs)
			}

			// Identity matching routes
			identities := v1.Group("/identities")
			{
				identities.GET("/unmatched", identityHandler.ListUnmatchedIdentities)
				identities.GET("/:id", identityHandler.GetIdentity)
				identities.POST("/:id/link", identityHandler.LinkIdentity)
				identities.POST("/:id/unlink", identityHandler.UnlinkIdentity)
				identities.DELETE("/:id", identityHandler.DeleteIdentity)
			}

			// Add identity route to contacts
			contacts.GET("/:id/identities", identityHandler.ListIdentitiesForContact)

			// Add contact task routes (action tasks) if Todoist is configured
			if contactTaskHandler != nil {
				contacts.GET("/:id/tasks", contactTaskHandler.ListContactTasks)
				contacts.POST("/:id/tasks", contactTaskHandler.CreateActionTask)
				contacts.DELETE("/:id/tasks/:taskId", contactTaskHandler.DeleteTaskLink)
			}

			// Add calendar event routes to contacts if calendar handler is initialized
			if calendarHandler != nil {
				contacts.GET("/:id/events", calendarHandler.ListEventsForContact)
				contacts.GET("/:id/events/upcoming", calendarHandler.ListUpcomingEventsForContact)

				// Add global events route
				events := v1.Group("/events")
				{
					events.GET("/upcoming", calendarHandler.ListUpcomingEvents)
				}
			}

			// Import candidates routes
			if importHandler != nil {
				imports := v1.Group("/imports")
				{
					imports.GET("/candidates", importHandler.ListImportCandidates)
					imports.GET("/:id", importHandler.GetImportCandidate)
					imports.POST("/:id/import", importHandler.ImportContact)
					imports.POST("/:id/link", importHandler.LinkContact)
					imports.POST("/:id/ignore", importHandler.IgnoreContact)
				}
			}
		}

		// Export/Import routes
		v1.POST("/export", systemHandler.ExportData)
		v1.POST("/import", systemHandler.ImportData)

		// Event bus ingestion endpoint (feature-flagged per spec §3.9).
		// When the flag is off the route is NOT registered — gin's NoRoute
		// handler returns 404 without running the API-key middleware, per
		// plan Decision 1. This matches the spec's acceptance criterion:
		// "EVENT_BUS_INGEST_ENABLED=false (default) returns 404."
		if cfg.Features.EnableEventBusIngest {
			ingest := v1.Group("/ingest")
			{
				ingest.POST("/events", ingestHandler.IngestEvents)
			}
			logger.Info().Msg("event bus ingestion endpoint enabled")
		}

		// Test routes (gated by CRM_ENV=testing or CRM_ENV=test)
		if cfg.Runtime.CRMEnvironment == "testing" || cfg.Runtime.CRMEnvironment == "test" {
			// Initialize external contact repo if not already done (for non-sync environments)
			testExternalRepo := externalContactRepo
			if testExternalRepo == nil {
				testExternalRepo = repository.NewExternalContactRepository(database.Queries)
			}

			// Initialize calendar repo for test seeding
			testCalendarRepo := repository.NewCalendarEventRepository(database.Queries)

			// Initialize calendar handler if not already done (allows reading seeded events in tests)
			if calendarHandler == nil {
				calendarHandler = handlers.NewCalendarHandler(testCalendarRepo)
				// Register calendar routes that weren't registered earlier (OAuth not configured)
				contacts.GET("/:id/events", calendarHandler.ListEventsForContact)
				contacts.GET("/:id/events/upcoming", calendarHandler.ListUpcomingEventsForContact)
				events := v1.Group("/events")
				{
					events.GET("/upcoming", calendarHandler.ListUpcomingEvents)
				}
				logger.Info().Msg("calendar handler initialized for testing (no OAuth)")
			}

			testHandler := handlers.NewTestHandler(database, testExternalRepo, contactService, testCalendarRepo)
			testRoutes := v1.Group("/test")
			{
				testRoutes.POST("/seed/contacts", testHandler.SeedContacts)
				testRoutes.POST("/seed/external-contacts", testHandler.SeedExternalContacts)
				testRoutes.POST("/seed/overdue-contacts", testHandler.SeedOverdueContacts)
				testRoutes.POST("/seed/calendar-events", testHandler.SeedCalendarEvents)
				testRoutes.POST("/cleanup", testHandler.Cleanup)
				testRoutes.POST("/trigger-error", testHandler.TriggerError)
			}
			logger.Info().Msg("test API endpoints enabled (CRM_ENV=testing)")
		}
	}

	// Swagger documentation
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Start server with configured bind address
	addr := cfg.GetBindAddress()
	// Use a listener so we can discover the selected port when PORT=0
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Fatal().Err(err).Str("addr", addr).Msg("failed to bind listener")
	}

	// Discover the actual port (useful when PORT=0)
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		logger.Fatal().Msg("failed to determine TCP address")
	}
	selectedPort := tcpAddr.Port

	srv := &http.Server{
		Addr:    ln.Addr().String(),
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		logger.Info().
			Int("port", selectedPort).
			Str("addr", cfg.Server.Host).
			Msg("starting server")
		logger.Info().
			Str("url", fmt.Sprintf("http://%s:%d/swagger/index.html", cfg.Server.Host, selectedPort)).
			Msg("API documentation available")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("failed to start server")
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info().Msg("shutting down server")

	// Give outstanding HTTP requests a configured timeout to complete.
	// Use logger.Error (not Fatal) for HTTP shutdown failure so that the
	// River drain below still runs. logger.Fatal calls os.Exit and would
	// skip Stop, leaving jobs holding leases until re-lease on next boot.
	// We remember the error and exit non-zero at the end so supervisors
	// (systemd, etc.) still see the failure.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer httpCancel()
	var shutdownErr error
	if err := srv.Shutdown(httpCtx); err != nil {
		logger.Error().Err(err).Msg("server forced to shutdown")
		shutdownErr = err
	}

	// Drain in-flight River jobs with a FRESH budget so a slow HTTP drain
	// doesn't steal River's deadline. Sharing one ctx between srv.Shutdown
	// and riverClient.Stop means that if a long-polling HTTP request burns
	// the full ShutdownTimeout, River gets an already-expired ctx and cannot
	// drain — jobs stay leased until next boot. A separate ctx preserves the
	// drain window. If River's own ctx does expire, its crash-resume
	// semantics handle the re-lease on next boot.
	riverCtx, riverCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer riverCancel()
	if err := riverClient.Stop(riverCtx); err != nil {
		logger.Warn().Err(err).Msg("river client stop returned error")
	}

	logger.Info().Msg("server exited")

	// Print the selected port on graceful exit for supervising processes
	fmt.Printf("PORT=%d\n", selectedPort) //nolint:forbidigo // Intentional stdout output for supervisor

	if shutdownErr != nil {
		// Surface the shutdown failure to supervisors via exit code
		// once run() returns and its defers fire (database.Close,
		// telegramManager.Stop, cronScheduler.Stop, cancel).
		return 1
	}
	return 0
}
