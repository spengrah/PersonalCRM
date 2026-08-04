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
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/telegram"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/gin-gonic/gin"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

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

	// River job-execution sampling repository (job_exec_sample). Feeds both the
	// Subscribe recorder and the job_sample_trim periodic worker below.
	jobSampleRepo := repository.NewJobSampleRepository(database.Queries)

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

	// Rematch service + graph (SP1) store. ContactService is NOT built here
	// (INV-5): it depends on the event-bus consumers, so it is constructed by
	// buildContactService once those exist.
	graph := buildGraphCore(database, eventBus)

	// Message-store repos + staging registry + venue resolver.
	messaging := buildMessagingFoundation(database.Queries, messagesMessageRepo, calendarRepoForIngest)
	// The pool is a property of the repository, not of any feature: without it
	// AttachUnmatchedByPeer — whose two statements must commit together — fails
	// at runtime. Unconditional and outside every feature gate, matching the
	// contact repository's precedent.
	messaging.CommsMessageRepo.SetPool(database.Pool)

	// Event-bus consumers (Cadence / Knowledge / FollowUp) + their shared
	// collaborators, built BEFORE ContactService so they can be passed to
	// NewContactService as constructor args (INV-5 reorder).
	consumers := buildDomainConsumers(cfg, database, core, graph, eventBus, riverClient)

	// ContactService — constructed with the consumers + AssertService as
	// constructor args (the setters are gone). The merge-time task-close
	// enqueuer (a cross-block dep) is wired inside buildContactService.
	contactService := buildContactService(cfg, database, core, graph, consumers, eventBus, riverClient)

	// InteractionRecorder — takes ContactService as a ctor arg, so it is
	// built after buildContactService.
	interactionRecorder := buildInteractionRecorder(contactService, messaging, ingest, consumers, eventBus)

	// IngestService + meeting-note conflict-resolution surface. Hoisted
	// after the consumers so the call.* inline handler can reuse them in
	// the same tx as the staging-row write.
	ingestStk := buildIngestStack(database, core, contactService, ingest, messaging, consumers, eventBus, riverClient)
	ingestHandler := ingestStk.IngestHandler
	meetingNoteHandler := ingestStk.MeetingNoteHandler

	// Register the three always-on core consumer workers (recorder,
	// calendar-decline, email-interaction).
	registerCoreConsumerWorkers(reg, database, core, contactService, interactionRecorder, messaging, consumers, eventBus)

	// Interaction-mode wiring gate → pubBus (concrete *events.Bus, nil in
	// off mode) + manual handler.
	pubBus, manualHandler := resolveInteractionMode(cfg, database, interactionRecorder, eventBus)

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
	// reproduces today's all-nil handler set when disabled; the gate lives in
	// buildExternalSyncIfEnabled, the seam shared with the OAuth route-wiring
	// boundary test.
	syncStk := buildExternalSyncIfEnabled(ctx, cfg, database, core, contactService, graph, ingest, messaging, consumers, domain, eventBus, riverClient, pubBus)
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
		telegramStk = buildTelegram(ctx, cfg, database, core, contactService, graph, messaging, domain, pubBus, riverClient)
	}
	telegramManager := telegramStk.Manager
	telegramHandler := telegramStk.Handler
	// The Stop defer lives here (not inside buildTelegram) so it fires on
	// run() return, not when the wire function returns. Nil-guarded:
	// registers only when a manager was built, exactly as the old
	// branch-local defer did.
	if telegramManager != nil {
		defer telegramManager.Stop()
	}

	// WhatsApp integration. Config validation already refuses a WhatsApp-on /
	// external-sync-off configuration, so no second gate is needed here. A zero
	// whatsappStack reproduces the nil manager/handler when disabled.
	var whatsappStk whatsappStack
	// Declared outside the gated block because the post-import hook wiring
	// below is outside it and must reference the same instance.
	var waIngestor *wapkg.Ingestor
	if cfg.Features.EnableWhatsAppSync {
		// Config validation refuses WhatsApp-on / external-sync-off, so a nil
		// external contact repository here is a validation escape rather than a
		// supported configuration: it leaves the ingestor nil, which keeps the
		// readiness gate reporting not_ready.
		if externalContactRepo == nil {
			logger.Error().Msg("whatsapp: external contact repository is absent; ingest stays unwired")
		} else {
			waIngestor = buildWhatsAppIngestor(cfg, database, messaging.CommsMessageRepo, ingest.IdentityService, externalContactRepo, domain.EnrichmentService)
		}
		// The prerequisite set the readiness gate reads. It is still
		// deliberately partial: the drain worker is not built yet, so the
		// manager reports not_ready and never connects. Filling the remaining
		// field is what turns WhatsApp on.
		//
		// The nil check is load-bearing: assigning a nil *Ingestor into the
		// interface field would produce a non-nil interface holding a nil
		// pointer, which the readiness gate would read as "wired".
		prereqs := whatsappPrereqs{}
		if waIngestor != nil {
			prereqs.Ingestor = waIngestor
		}
		whatsappStk = buildWhatsApp(ctx, cfg, database, prereqs)
	}
	whatsappManager := whatsappStk.Manager
	whatsappHandler := whatsappStk.Handler
	// Same rationale as Telegram's: the Stop defer lives here so it fires on
	// run() return, not when the wire function returns.
	if whatsappManager != nil {
		defer whatsappManager.Stop()
	}

	// Wire the per-source post-import hooks (message back-linking). Each is
	// registered only when its integration is built.
	if importHandler != nil {
		if telegramManager != nil {
			importHandler.RegisterPostImportHook(telegram.NewPostImportHook(telegramManager))
		}
		if waIngestor != nil {
			importHandler.RegisterPostImportHook(wapkg.NewPostImportHook(waIngestor.PeerMatcher()))
		}
	}

	// Aggregation engines (messages + gchat) + reenqueuer registry.
	agg := buildAggregationEngines(database, core, contactService, graph, ingest, messaging, consumers, eventBus, riverClient, telegramManager, gchatProvider, gchatSyncStates)

	// Register the chat-aware messaging aggregate worker + sweeper (+ periodic).
	registerMessagingWorkers(reg, ingest, messaging, agg, riverClient)

	// Core HTTP handlers.
	handlersCore := buildCoreHandlers(database, core, contactService, graph, cfg, noteService, manualHandler, eventBus)
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

	// River job-execution sampling: register the trim periodic job, then start
	// the Subscribe recorder BEFORE Start so no early finished-job events are
	// missed. jobSampleWait joins the recorder goroutine on shutdown.
	registerJobSampleWorkers(reg, jobSampleRepo, cfg)
	jobSampleWait := startJobSampleRecorder(ctx, riverClient, jobSampleRepo)

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

	// Register the full route tree (health, OAuth callbacks, mac-host,
	// ingest, the /api/v1 authenticated group, swagger). All conditional
	// gates live at their call sites inside registerRoutes.
	registerRoutes(routeDeps{
		Router:                   router,
		Cfg:                      cfg,
		Database:                 database,
		StalenessService:         stalenessService,
		MacHostRepo:              macHostRepo,
		MacHostHandler:           macHostHandler,
		IngestHandler:            ingestHandler,
		MeetingNoteHandler:       meetingNoteHandler,
		ContactHandler:           contactHandler,
		InteractionHandler:       interactionHandler,
		NoteHandler:              noteHandler,
		ContactMethodHandler:     handlersCore.ContactMethod,
		RematchHandler:           rematchHandler,
		StalenessHandler:         stalenessHandler,
		SystemHandler:            systemHandler,
		OAuthHandler:             oauthHandler,
		GoogleOAuthService:       googleOAuthService,
		TodoistHandler:           todoistHandler,
		TelegramHandler:          telegramHandler,
		WhatsAppHandler:          whatsappHandler,
		SyncHandler:              syncHandler,
		IdentityHandler:          identityHandler,
		ContactTaskHandler:       contactTaskHandler,
		CalendarHandler:          calendarHandler,
		ImportHandler:            importHandler,
		AnarlogDiscoveryHandler:  anarlogDiscoveryHandler,
		SuggestionHandler:        suggestionHandler,
		ExternalContactRepo:      externalContactRepo,
		ContactService:           contactService,
		MeetingNoteRepoForIngest: meetingNoteRepoForIngest,
	})

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

	// Join the job-exec-sample recorder goroutine. Stop closed its subscription
	// channel above, so this returns promptly; the DB pool is still open
	// (database.Close is the last-running LIFO defer) so any in-flight write in
	// the drain completes.
	jobSampleWait()

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
