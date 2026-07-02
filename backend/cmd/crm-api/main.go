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
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	contactRepo := core.Contact
	contactMethodRepo := core.ContactMethod
	interactionRepo := core.Interaction

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
	externalContactRepoForIngest := ingest.ExternalContact
	macHostRepoForIngest := ingest.MacHost
	meetingNoteRepoForIngest := ingest.MeetingNote
	calendarRepoForIngest := ingest.CalendarEvent

	// Rematch service + ContactService + graph (SP1) store. Rematch is
	// constructed above ContactService so it can be passed as the
	// RematchRegistry constructor arg.
	graph := buildContactGraphCore(database, core, eventBus)
	rematchService := graph.RematchService
	contactService := graph.ContactService
	assertService := graph.AssertService

	// Message-store repos + staging registry + venue resolver.
	messaging := buildMessagingFoundation(database.Queries, messagesMessageRepo, calendarRepoForIngest)
	telegramMessageRepo := messaging.TelegramMessageRepo
	commsMessageRepo := messaging.CommsMessageRepo

	// Event-bus consumers (Cadence / Knowledge / FollowUp / InteractionRecorder)
	// + their shared collaborators, wired into ContactService via its setters.
	consumers := buildEventConsumers(cfg, database, core, graph, ingest, messaging, eventBus, riverClient)
	aggregatorReenqueuerHolder := consumers.AggregatorReenqueuerHolder

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
	enrichmentService := domain.EnrichmentService

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
	syncService := syncStk.SyncService
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

	// Telegram integration (independent of external sync)
	var telegramManager *tgpkg.TelegramManager
	var telegramHandler *handlers.TelegramHandler

	if cfg.Features.EnableTelegramSync && cfg.External.TelegramAPIID != 0 {
		telegramSessionRepo := repository.NewTelegramSessionRepository(database.Queries)
		telegramUpdateStateRepo := repository.NewTelegramUpdateStateRepository(database.Queries)
		telegramChatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
		// telegramMessageRepo is hoisted above (needed by the consumer
		// wiring); no re-construction here.
		telegramSyncRepo := repository.NewSyncRepository(database.Queries)

		// Phase 4: identity + aggregation dependencies
		tgIdentityRepo := repository.NewIdentityRepository(database.Queries)
		tgIdentityService := service.NewIdentityService(tgIdentityRepo)
		tgExternalContactRepo := repository.NewExternalContactRepository(database.Queries)

		encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to initialize Telegram encryptor (TOKEN_ENCRYPTION_KEY required)")
		}

		// River-backed stale-claim recovery enqueuer. Uses UniqueOpts
		// {ByArgs: true} paired with the InteractionRecorderJobArgs.EventID
		// `river:"unique"` tag so repeated recovery enqueues against the
		// same event coalesce into one in-flight job (spec §3 Race
		// Mechanics).
		tgRecoveryEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)

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
			pubBus,
			database.Pool,
			tgRecoveryEnqueuer,
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

	// Messages aggregator engine + reenqueuer + worker + sweeper.
	// Wired unconditionally — the Mac daemon push pipeline accepts
	// raw_message.* envelopes regardless of any feature flag, and the
	// engine is a stateless function over messagesMessageRepo (no
	// daemon-side connection or background loop).
	//
	// The chat-aware AggregateForContact path is what preserves the
	// engine's extend/bridge/coalesce contract. The
	// MessagingAggregateForContactWorker iterates over the contact's
	// distinct unprocessed chats and invokes it per chat; the periodic
	// sweeper provides a 5-min safety net for the never-claimed
	// stranded-row gap.
	messagesEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	const messagesBurstWindowHours = 4
	const messagesReplyBridgeHours = 48
	messagesEngine := messages.NewAggregationEngine(
		messagesBurstWindowHours,
		messagesReplyBridgeHours,
		messagesMessageRepo,
		interactionRepo,
		contactService,
		contactService,
		eventBus,
		database.Pool,
		messagesEnqueuer,
	)
	messagesReenqueuer := consumer.NewMessagesAggregatorReenqueuer(
		messagesEngine,
		riverClient,
		repository.InteractionSourceMessages,
	)

	// GChat aggregation engine over comms_message. LIVE but INERT: the
	// engine/worker/sweeper/reenqueuer for gchat run on every tick, but every
	// query is source='gchat'-scoped and returns zero rows until a provider +
	// enablement write comms_message(source='gchat') rows. Burst/reply windows
	// are hard-coded here (matching how messages hard-codes its constants);
	// env-var overrides are out of scope for now.
	gchatEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	const gchatBurstWindowHours = 2
	const gchatReplyBridgeHours = 48
	gchatEngine := google.NewGChatAggregationEngine(
		gchatBurstWindowHours,
		gchatReplyBridgeHours,
		commsMessageRepo,
		interactionRepo,
		contactService,
		contactService,
		eventBus,
		database.Pool,
		gchatEnqueuer,
	)

	// GChat rematch handlers: registered when the provider was constructed
	// (Google OAuth configured) AND external sync is enabled. They are provably
	// inert until an enabled gchat sync state exists — each gates FIRST on
	// ListEnabledSyncStates filtered to source='gchat' and returns (0, nil) when
	// that set is empty. The email handler co-registers under "email" alongside
	// Gmail/Calendar; its gchat-scoped gate means it no-ops while the others do
	// their real work. The provider itself is NOT registered into
	// providerRegistry (the scheduler never runs it).
	if gchatProvider != nil && gchatSyncStates != nil {
		rematchService.Register(google.NewGChatHandleRematchHandler(gchatProvider, gchatSyncStates, commsMessageRepo, gchatEngine))
		rematchService.Register(google.NewGChatEmailRematchHandler(gchatProvider, gchatSyncStates, commsMessageRepo, gchatEngine))
		logger.Info().Msg("GChat rematch handlers registered (inert until a gchat sync state is enabled)")
	}

	gchatReenqueuer := consumer.NewCommsAggregatorReenqueuer(
		gchatEngine,
		riverClient,
		repository.InteractionSourceGChat,
	)

	// Wire the per-source aggregator reenqueuer registry. The
	// InteractionRecorderWorker holds the deferred holder; this
	// assignment makes the post-commit reenqueue path live for both
	// telegram-source and messages-source events. When Telegram is
	// disabled the telegram entry is a no-op reenqueuer (so calls for
	// telegram-source envelopes — which won't be produced anyway —
	// degrade cleanly).
	reenqueuerEntries := map[string]consumer.AggregatorReenqueuer{
		repository.InteractionSourceMessages: messagesReenqueuer,
		repository.InteractionSourceGChat:    gchatReenqueuer,
	}
	if telegramManager != nil {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NewTelegramAggregatorReenqueuer(telegramManager.AggregationEngine())
	} else {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NoopAggregatorReenqueuer{}
	}
	aggregatorReenqueuerHolder.set(consumer.NewAggregatorReenqueuerRegistry(reenqueuerEntries))

	// Register the messaging aggregate workers. The chat-lister
	// registry maps source → repository's ListUnprocessedChatsByContact;
	// future messaging sources (whatsapp etc) extend the map without
	// touching the worker.
	chatListerRegistry := scheduler.NewPerSourceChatListerRegistry(
		map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			repository.InteractionSourceMessages: messagesMessageRepo.ListUnprocessedChatsByContact,
			// Source-bound closure: the comms repo method is multi-source
			// (ListUnprocessedChatsByContactForSource), so bind 'gchat'.
			repository.InteractionSourceGChat: func(ctx context.Context, contactID uuid.UUID) ([]string, error) {
				return commsMessageRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceGChat, contactID)
			},
		},
	)
	river.AddWorker(riverWorkers, scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{
			repository.InteractionSourceMessages: messagesEngine,
			repository.InteractionSourceGChat:    gchatEngine,
		},
		chatListerRegistry,
	))

	// Periodic 5-min sweeper — drains never-claimed stranded rows that
	// the in-line worker re-list loop AND the post-Stage-3 reenqueue
	// both missed. Run once on startup so restart-recovery does not wait
	// a full interval before the safety net engages.
	sweeperListers := map[string]scheduler.UnprocessedContactLister{
		repository.InteractionSourceMessages: messagesMessageRepo,
		// Source-bound adapter: comms_message is multi-source, so wrap the
		// repo with a 'gchat'-pinned lister.
		repository.InteractionSourceGChat: newCommsSourceContactLister(commsMessageRepo, repository.InteractionSourceGChat),
	}
	river.AddWorker(riverWorkers, scheduler.NewMessagingAggregateSweeperWorker(sweeperListers, riverClient))
	riverClient.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return consumerjobs.MessagingAggregateSweeperArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Initialize handlers
	contactHandler := handlers.NewContactHandler(contactService)
	noteHandler := handlers.NewNoteHandler(noteService)
	interactionHandler := handlers.NewInteractionHandler(interactionRepo, manualHandler)
	systemHandler := handlers.NewSystemHandler(contactRepo, cfg.Runtime)
	rematchHandler := handlers.NewRematchHandler(rematchService, contactService)

	// Mac-daemon host management. Wires the pairing flow, heartbeat,
	// cursor protocol, and admin revoke. The pairing endpoint is
	// unauthenticated (token-gated) so it lives on the bare router; the
	// daemon endpoints live behind MacHostAuthMiddleware (sibling
	// /api/v1 group); the admin endpoints live behind the existing
	// global API-key middleware.
	// macHostRepoForIngest was constructed earlier (line ~308) so the
	// IngestService could take it as a HostLivenessChecker dep. Reuse
	// the same instance here so the host-management service shares the
	// same repository wrapper.
	macHostRepo := macHostRepoForIngest
	pairingTokenRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	// Mac cursor commit needs a tx — use the pool-wired SyncRepository.
	macSyncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostService := service.NewMacHostService(
		macHostRepo,
		pairingTokenRepo,
		macSyncRepo,
		contactMethodRepo,
		externalContactRepoForIngest, // /known-ids reader (external_contact)
		meetingNoteRepoForIngest,     // /known-ids reader (anarlog_sessions)
		database.Pool,
		0, // default bcrypt cost
	)
	pairingIPLimiter := auth.NewPairingIPRateLimiter()
	macHostHandler := handlers.NewMacHostHandler(macHostService, pairingIPLimiter)

	// Register the pairing-token janitor periodic job (5 min). Worker
	// registered unconditionally; the periodic-job inserter triggers it
	// on the same River client. See
	// backend/internal/scheduler/pairing_token_janitor_worker.go.
	river.AddWorker(riverWorkers, scheduler.NewPairingTokenJanitorWorker(pairingTokenRepo))
	riverClient.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.PairingTokenJanitorArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Sync-staleness watchdog. Registered unconditionally (like the janitor):
	// heartbeat/push breaches must be detected even with external sync off,
	// and the watchdog reads existing freshness state rather than driving any
	// provider sync. The endpoint reader uses the same service instance.
	stalenessBreachRepo := repository.NewStalenessRepository(database.Queries)
	stalenessService := service.NewStalenessService(
		cfg.Staleness,
		cfg.Features.EnableExternalSync,
		macSyncRepo,
		macHostRepo,
		stalenessBreachRepo,
	)
	stalenessHandler := handlers.NewStalenessHandler(stalenessService)
	river.AddWorker(riverWorkers, scheduler.NewStalenessWatchdogWorker(stalenessService))
	riverClient.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.StalenessWatchdogArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Assertion valid-time rollover — a daily catch-up sweep that terminalizes the
	// bounded-with-pending-successor assertions whose valid_to has passed (no event
	// fires at that future date otherwise). Stateless; RunOnStart catches up any
	// overdue rollovers on boot.
	river.AddWorker(riverWorkers, scheduler.NewAssertionRolloverWorker(assertService))
	riverClient.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.AssertionRolloverArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Build the River worker set, periodic-job list, and client. The
	// scheduler-tick + sync-provider-account workers are only registered
	// when external sync is enabled and we have a real syncService —
	// otherwise there is nothing for them to do. See DD 6 in
	// .ai/log/plan/event-bus-foundation-pr3-scheduler-river.md for the
	// construction-order rationale.
	//
	// Sync workers + periodic job are registered AFTER syncService is
	// constructed. Safe between NewClient (done earlier) and Start —
	// river.AddWorker and PeriodicJobs().Add both mutate the client
	// in-place.
	if cfg.Features.EnableExternalSync && syncService != nil {
		river.AddWorker(riverWorkers, scheduler.NewSchedulerTickWorker(syncService))
		river.AddWorker(riverWorkers, scheduler.NewSyncProviderAccountWorker(syncService))
		riverClient.PeriodicJobs().Add(river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return scheduler.SchedulerTickArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	// Wire the enqueuer onto the service BEFORE starting the client so
	// any tick fire or TriggerSync that races with bring-up goes through
	// river instead of falling back to inline sync. See DD 6 step 8.
	if syncService != nil && cfg.Features.EnableExternalSync {
		syncService.SetRiverEnqueuer(riverClient)
	}

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
