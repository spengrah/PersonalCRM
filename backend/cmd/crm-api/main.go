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
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "personal-crm/backend/docs" // Import generated docs
)

// riverDiscardedJobRetention is how long River keeps discarded job rows before
// JobCleaner prunes them. River's 7-day default would destroy the forensic
// trail that `crm-admin --list-jobs` (and any health-check counts over
// river_job) rely on. Single-user row counts are trivial, so we keep discarded
// jobs for 90 days. Not exposed via config — no caller needs to vary it.
const riverDiscardedJobRetention = 90 * 24 * time.Hour

func main() {
	// Run the server body in a helper so its defers (database.Close,
	// telegramManager.Stop, riverClient.Stop, shutdown-ctx cancel) all
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

	// Record the serving build (stamped via -ldflags at build time). In prod this
	// lands in `podman logs crm-backend`, a log-side record of which commit is live.
	logger.Info().
		Str("version", health.Version).
		Str("git_commit", health.GitCommit).
		Str("build_time", health.BuildTime).
		Msg("build info")

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

	// Initialize repositories
	core := buildCoreRepos(database.Queries)

	// River client + event bus + consumer wiring. Built EARLY (before
	// downstream services) so `pubBus` and `manualHandler` are in scope
	// for constructors that need them (Calendar, Telegram, manual handlers).
	// Sync workers + periodic job are registered LATER (once syncService
	// exists) via river.AddWorker + riverClient.PeriodicJobs().Add(), both
	// of which are safe between NewClient and Start.
	//
	// eventBus + rematchService are constructed BEFORE ContactService /
	// EnrichmentService so those services can take them as constructor
	// args (the rematch registry is required; SetRematchService setter
	// is gone).
	riverWorkers := river.NewWorkers()
	// Recording delegate over the worker + periodic-job bundles. Built
	// around riverWorkers BEFORE river.NewClient so the noopWorker
	// registration is recorded; its periodic bundle is attached
	// immediately after NewClient returns. The wire functions register
	// through reg instead of calling river.AddWorker / PeriodicJobs().Add
	// directly, so the golden-list test can pin the registration set.
	reg := newRiverRegistrar(riverWorkers)
	addWorker(reg, &noopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: riverWorkers,
		// Dead-letter visibility: the ErrorHandler logs every errored
		// attempt + panic (ERROR on final-attempt discard, WARN otherwise);
		// Logger routes River's own internal logs into the zerolog stream
		// instead of a default slog TextHandler on stdout; the retention raise
		// keeps discarded rows queryable for forensics.
		ErrorHandler:                events.NewRiverErrorHandler(logger.Get()),
		Logger:                      logger.NewSlogLogger(logger.Get()),
		DiscardedJobRetentionPeriod: riverDiscardedJobRetention,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build river client")
	}
	// Attach the periodic-job bundle now that the client exists. Every
	// addPeriodic call happens later in the wire chain, so this late
	// attach is safe by construction.
	reg.periodic = riverClient.PeriodicJobs()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)

	// Ingest repositories + identity service (host-auth ingest path).
	// Several instances are reused by the external-sync / mac-host /
	// calendar / telegram blocks below.
	ingest := buildIngestRepos(database.Queries)
	messagesMessageRepo := ingest.MessagesMessage
	meetingNoteRepoForIngest := ingest.MeetingNote
	calendarRepoForIngest := ingest.CalendarEvent

	// Rematch service + ContactService + graph (SP1) store. Rematch is
	// constructed above ContactService so it can be passed as the
	// RematchRegistry constructor arg.
	graph := buildContactGraphCore(database, core, eventBus)
	contactService := graph.ContactService

	// Message-store repos + staging registry + venue resolver.
	messaging := buildMessagingFoundation(database.Queries, messagesMessageRepo, calendarRepoForIngest)

	// Event-bus consumers (Cadence / Knowledge / FollowUp / InteractionRecorder)
	// + their shared collaborators, wired into ContactService via its setters.
	consumers := buildEventConsumers(cfg, database, core, graph, ingest, messaging, eventBus, riverClient)

	// IngestService + meeting-note conflict-resolution surface. Hoisted
	// after the consumers so the call.* inline handler can reuse them in
	// the same tx as the staging-row write.
	ingestStk := buildIngestStack(database, core, graph, ingest, messaging, consumers, eventBus, riverClient)
	ingestHandler := ingestStk.IngestHandler
	meetingNoteHandler := ingestStk.MeetingNoteHandler

	// Register the three always-on core consumer workers (recorder,
	// calendar-decline, email-interaction).
	registerCoreConsumerWorkers(reg, database, core, graph, messaging, consumers, eventBus)

	// Interaction-mode wiring gate → pubBus (concrete *events.Bus, nil in
	// off mode) + manual handler.
	pubBus, manualHandler := resolveInteractionMode(cfg, database, consumers, eventBus)

	// Register the cadence / knowledge-cache / follow-up / Todoist workers
	// + their mode boot logs.
	registerModeWorkers(reg, cfg, database, core, consumers, eventBus, riverClient)

	// Domain services (note / import-match / enrichment / address-book
	// reconcile) + the EnrichmentService setter wiring + the IngestService
	// AddressBookReconciler back-reference.
	domain := buildDomainServices(database, core, graph, ingest, consumers, ingestStk, eventBus)
	noteService := domain.NoteService

	// Rematch dispatcher consumer — subscribes to contact_methods.added
	// events and runs RematchService.Run with per-contact mutex
	// serialization. Rematch handlers themselves (calendar, telegram) are
	// registered below once their deps are constructed.
	registerRematchDispatcher(reg, graph, database, eventBus)

	// External sync components (feature-flagged). A zero syncStack
	// reproduces today's all-nil handler set when disabled; the gate stays
	// here so buildExternalSync runs only when the feature is on.
	var syncStk syncStack
	if cfg.Features.EnableExternalSync {
		syncStk = buildExternalSync(ctx, cfg, database, core, graph, ingest, messaging, consumers, domain, eventBus, riverClient, pubBus)
	}
	syncHandler := syncStk.SyncHandler
	identityHandler := syncStk.IdentityHandler
	oauthHandler := syncStk.OAuthHandler
	importHandler := syncStk.ImportHandler
	suggestionHandler := syncStk.SuggestionHandler
	anarlogDiscoveryHandler := syncStk.AnarlogDiscoveryHandler
	calendarHandler := syncStk.CalendarHandler
	todoistHandler := syncStk.TodoistHandler
	contactTaskHandler := syncStk.ContactTaskHandler
	googleOAuthService := syncStk.GoogleOAuthService
	externalContactRepo := syncStk.ExternalContactRepo
	gchatProvider := syncStk.GChatProvider
	gchatSyncStates := syncStk.GChatSyncStates

	// Telegram integration (independent of external sync). A zero
	// telegramStack reproduces today's nil manager/handler when disabled.
	var telegramStk telegramStack
	if cfg.Features.EnableTelegramSync && cfg.External.TelegramAPIID != 0 {
		telegramStk = buildTelegram(ctx, cfg, database, core, graph, messaging, domain, pubBus, riverClient)
	}
	telegramManager := telegramStk.Manager
	telegramHandler := telegramStk.Handler
	// The Stop defer lives here (not inside buildTelegram) so it fires on
	// run() return, not when the wire function returns. Nil-guarded:
	// registers only when a manager was built, exactly as the old
	// branch-local defer did (decision 4).
	if telegramManager != nil {
		defer telegramManager.Stop()
	}

	// Wire Telegram post-import hook (if both Telegram and imports are enabled)
	if telegramManager != nil && importHandler != nil {
		importHandler.SetPostImportHook(telegramManager)
	}

	// Aggregation engines (messages + gchat) + reenqueuer registry.
	agg := buildAggregationEngines(database, core, graph, ingest, messaging, consumers, eventBus, riverClient, telegramManager, gchatProvider, gchatSyncStates)

	// Register the chat-aware messaging aggregate worker + sweeper (+ periodic).
	registerMessagingWorkers(reg, ingest, messaging, agg, riverClient)

	// Core HTTP handlers.
	handlersCore := buildCoreHandlers(core, graph, cfg, noteService, manualHandler)
	contactHandler := handlersCore.Contact
	noteHandler := handlersCore.Note
	interactionHandler := handlersCore.Interaction
	systemHandler := handlersCore.System
	rematchHandler := handlersCore.Rematch

	// Mac-daemon host management (+ pairing-token janitor worker/periodic).
	machost := buildMacHost(reg, database, core, ingest)
	macHostRepo := machost.Repo
	macHostHandler := machost.Handler

	// Sync-staleness watchdog (+ periodic).
	staleness := buildStaleness(reg, cfg, database, machost)
	stalenessService := staleness.Service
	stalenessHandler := staleness.Handler

	// Assertion valid-time rollover (daily worker + periodic).
	registerAssertionRollover(reg, graph)

	// Scheduler-tick + sync-provider workers (gated) + River enqueuer wiring,
	// all before riverClient.Start.
	registerSyncScheduler(reg, cfg, syncStk, riverClient)

	if err := riverClient.Start(ctx); err != nil {
		logger.Fatal().Err(err).Msg("failed to start river client")
	}
	logger.Info().
		Int("worker_concurrency", cfg.River.WorkerConcurrency).
		Dur("job_timeout", cfg.River.JobTimeout).
		Msg("river client started")

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

	// Health check endpoint. Bare /health is the liveness probe (DB-only
	// top-level status, today's contract); ?ready=1 aggregates the
	// river/sync/disk components for an external pinger. The stalenessService
	// (constructed above) and a HealthRepository over river_job back the new
	// components; the watchdog kind ties the sync component's freshness guard to
	// the periodic watchdog job registered on the same River client.
	healthRepo := repository.NewHealthRepository(database.Queries)
	healthChecker := health.NewHealthChecker(database, cfg.Database.HealthTimeout, health.Deps{
		River:            healthRepo,
		Staleness:        stalenessService,
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
	if oauthHandler != nil {
		handlers.RegisterOAuthCallbackRoutes(router, handlers.OAuthCallbackDeps{
			Handler:       oauthHandler,
			GoogleEnabled: googleOAuthService != nil,
		})
	}

	// Mac-daemon public + host-auth routes (Pair + heartbeat + cursor +
	// known-ids). Registered via the shared helper so integration tests
	// exercise the same code path. Admin routes are registered later
	// inside the global-API-key-protected v1 group.
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    macHostRepo,
		Handler:     macHostHandler,
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
			auth.MacHostAuthMiddleware(macHostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
		)
		handlers.RegisterIngestRoutes(router, handlers.IngestRouteDeps{
			Auth:        ingestAuth,
			Ingest:      ingestHandler,
			MeetingNote: meetingNoteHandler,
		})
		logger.Info().Msg("event bus ingestion endpoint enabled")
	}

	// API routes
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		// Contact + interaction routes (unconditional).
		handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
			Contact:     contactHandler,
			Interaction: interactionHandler,
			Note:        noteHandler,
		})

		// Meeting-note conflict-resolution — user-driven, called from
		// the frontend with the global API key. Stays under the v1
		// APIKeyMiddleware group.
		handlers.RegisterMeetingNoteRoutes(v1, meetingNoteHandler)

		// Rematch routes — always registered; service no-ops when no handlers
		// are registered (e.g. telegram-disabled deployments still get calendar).
		handlers.RegisterRematchRoutes(v1, rematchHandler)

		// Sync-staleness breaches — registered unconditionally (OUTSIDE the
		// EnableExternalSync-gated /sync group below): heartbeat/push breaches
		// must be visible even with external sync off. The static 2-segment
		// path coexists with that group's 3-segment param routes.
		handlers.RegisterSyncStalenessRoutes(v1, stalenessHandler)

		// System routes
		handlers.RegisterSystemRoutes(v1, systemHandler)

		// Mac-daemon admin routes (under global API key middleware).
		// Pairing-token mint + revoke + list/get for the Mac settings UI.
		handlers.RegisterMacHostAdminRoutes(v1, macHostHandler)

		// OAuth routes (feature-flagged with external sync)
		if oauthHandler != nil {
			handlers.RegisterOAuthRoutes(v1, handlers.OAuthCallbackDeps{
				Handler:       oauthHandler,
				GoogleEnabled: googleOAuthService != nil,
			})
		}

		// Todoist settings routes (only if Todoist is configured)
		if todoistHandler != nil {
			handlers.RegisterTodoistRoutes(v1, todoistHandler)
		}

		// Telegram routes (feature-flagged)
		if telegramHandler != nil {
			handlers.RegisterTelegramRoutes(v1, telegramHandler)
		}

		// External sync routes (feature-flagged)
		if cfg.Features.EnableExternalSync && syncHandler != nil {
			handlers.RegisterSyncRoutes(v1, syncHandler)
			handlers.RegisterIdentityRoutes(v1, identityHandler)

			// Add contact task routes (manual tasks) if Todoist is configured
			if contactTaskHandler != nil {
				handlers.RegisterContactTaskRoutes(v1, contactTaskHandler)
			}

			// Add calendar event routes to contacts if calendar handler is initialized
			if calendarHandler != nil {
				handlers.RegisterCalendarRoutes(v1, calendarHandler)
			}

			// Import candidates routes
			if importHandler != nil {
				handlers.RegisterImportRoutes(v1, handlers.ImportRouteDeps{
					Import:           importHandler,
					AnarlogDiscovery: anarlogDiscoveryHandler,
					Suggestions:      suggestionHandler,
				})
			}
		}

		// Export/Import routes
		handlers.RegisterDataExchangeRoutes(v1, systemHandler)

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
				handlers.RegisterCalendarRoutes(v1, calendarHandler)
				logger.Info().Msg("calendar handler initialized for testing (no OAuth)")
			}

			testSeedService := service.NewTestSeedService(database, testExternalRepo, contactService, testCalendarRepo, macHostRepo, meetingNoteRepoForIngest)
			testHandler := handlers.NewTestHandler(testSeedService)
			handlers.RegisterTestRoutes(v1, testHandler)
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
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		// telegramManager.Stop, riverClient.Stop, cancel).
		return 1
	}
	return 0
}
