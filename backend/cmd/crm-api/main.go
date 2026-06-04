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
	stdsync "sync"
	"syscall"
	"time"

	"personal-crm/backend/internal/anarlog"
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
	"personal-crm/backend/internal/push"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"
	tgpkg "personal-crm/backend/internal/telegram"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "personal-crm/backend/docs" // Import generated docs
)

// noopJobArgs is the args type for the placeholder worker below. It is
// never enqueued in production; its sole purpose is to satisfy river's
// "must have at least one registered worker" invariant when external
// sync is disabled.
type noopJobArgs struct{}

func (noopJobArgs) Kind() string { return "noop" }

// noopWorker exists so the river client always has at least one
// registered worker, even when cfg.Features.EnableExternalSync is false
// and the scheduler workers are not registered. river.NewClient rejects
// an empty Workers bundle (the constructor returns an error), so the
// API fails to boot in the default non-sync configuration without this
// placeholder.
type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

// Work implements river.Worker. Since no 'noop' jobs are enqueued
// anywhere in the codebase, this method is never called at runtime.
func (*noopWorker) Work(_ context.Context, _ *river.Job[noopJobArgs]) error {
	return nil
}

// followUpSettingsRef holds a deferred reference to the Todoist OAuth
// service + sync repo so the FollowUpManager settings func can be
// wired at construction time (before the external-sync branch decides
// whether Todoist is configured). The external-sync branch populates
// oauth+sync when Todoist is initialized; until then fn() returns
// service.ErrNoTodoistAccount to keep the consumer's Todoist-dependent
// post-commit paths a best-effort no-op.
type followUpSettingsRef struct {
	oauth       *todoist.OAuthService
	sync        *repository.SyncRepository
	frontendURL string
}

// fn returns a TodoistSettingsFunc closure that resolves settings
// through the populated refs. Todoist-unconfigured states (no account,
// no sync state, missing label) collapse to consumer.ErrTodoistUnconfigured
// so the follow-up consumer can treat them as a non-fatal skip rather
// than rolling back the interaction write.
func (r *followUpSettingsRef) fn() consumer.TodoistSettingsFunc {
	return func(ctx context.Context) (*todoist.Settings, string, error) {
		if r.oauth == nil || r.sync == nil {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		accounts, err := r.oauth.ListAccounts(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("list todoist accounts: %w", err)
		}
		if len(accounts) == 0 {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		accountID := accounts[0].AccountID
		accessToken, err := r.oauth.GetAccessToken(ctx, accountID)
		if err != nil {
			return nil, "", fmt.Errorf("get access token: %w", err)
		}
		state, err := r.sync.GetSyncStateBySource(ctx, todoist.SourceName, &accountID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, "", consumer.ErrTodoistUnconfigured
			}
			return nil, "", fmt.Errorf("get sync state: %w", err)
		}
		settings := &todoist.Settings{}
		if state.Metadata != nil {
			if v, ok := state.Metadata[todoist.MetadataKeyProjectID].(string); ok {
				settings.ProjectID = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyProjectName].(string); ok {
				settings.ProjectName = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyLabelID].(string); ok {
				settings.LabelID = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyLabelName].(string); ok {
				settings.LabelName = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyIntegrationInstance].(string); ok {
				settings.IntegrationInstanceID = v
			}
		}
		if settings.LabelID == "" {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		return settings, accessToken, nil
	}
}

// deferredAggregatorReenqueuer is a thread-safe holder that the
// InteractionRecorderWorker is constructed against before the
// Telegram aggregation engine exists. The cfg.Features.EnableTelegramSync
// branch calls .set once telegramManager is built; until then the
// holder dispatches every Reenqueue call to a logged-warn no-op.
//
// Satisfies consumer.AggregatorReenqueuer (via the holder pointer);
// safe to pass to the worker constructor before the inner registry
// is wired.
type deferredAggregatorReenqueuer struct {
	mu    stdsync.RWMutex
	inner consumer.AggregatorReenqueuer
}

// set installs the concrete reenqueuer. May be called once after the
// Telegram aggregation engine exists.
func (d *deferredAggregatorReenqueuer) set(inner consumer.AggregatorReenqueuer) {
	d.mu.Lock()
	d.inner = inner
	d.mu.Unlock()
}

// Reenqueue implements consumer.AggregatorReenqueuer. Falls back to a
// logged-warn no-op when the inner registry has not been wired yet
// (e.g. cfg.Features.EnableTelegramSync is false).
func (d *deferredAggregatorReenqueuer) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	d.mu.RLock()
	inner := d.inner
	d.mu.RUnlock()
	if inner == nil {
		log.Printf("aggregator-reenqueuer: registry not yet wired; skipping (source=%s contact=%s)",
			env.Source, contactID.String())
		return nil
	}
	return inner.Reenqueue(ctx, env, contactID)
}

// meetingNoteLinkageTargetReader adapts the calendar + phone_call
// repositories into the polymorphic service.LinkageTargetReader
// interface that MeetingNoteService consumes. Kept inline in main.go
// per the single-file build convention (any file the binary needs must
// be compilable as part of `go build cmd/crm-api/main.go`).
type meetingNoteLinkageTargetReader struct {
	calendarRepo  *repository.CalendarEventRepository
	phoneCallRepo *repository.PhoneCallRepository
}

// GetEventByID satisfies service.LinkageTargetReader.
func (r *meetingNoteLinkageTargetReader) GetEventByID(ctx context.Context, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByID(ctx, id)
}

// GetPhoneCallByID satisfies service.LinkageTargetReader.
func (r *meetingNoteLinkageTargetReader) GetPhoneCallByID(ctx context.Context, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByID(ctx, id)
}

// GetEventByIDTx satisfies service.LinkageTargetReader for the
// tx-bound resolve flow.
func (r *meetingNoteLinkageTargetReader) GetEventByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByIDTx(ctx, tx, id)
}

// GetPhoneCallByIDTx satisfies service.LinkageTargetReader for the
// tx-bound resolve flow.
func (r *meetingNoteLinkageTargetReader) GetPhoneCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByIDTx(ctx, tx, id)
}

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
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	noteRepo := repository.NewNoteRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

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
	river.AddWorker(riverWorkers, &noopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build river client")
	}

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)

	// Identity repository + service for the ingest path. The host-auth
	// ingest path (raw_message.* envelopes from the Mac daemon) needs a
	// tx-bound identity matcher so the per-event savepoint can roll back
	// the identity write atomically with the staging-row insert on
	// failure. The external-sync block (further down) constructs its own
	// IdentityService for the providers that use it — IdentityService is
	// stateless so the two instances are interchangeable.
	identityRepoForIngest := repository.NewIdentityRepository(database.Queries)
	identityServiceForIngest := service.NewIdentityService(identityRepoForIngest)

	// Messages staging repo (Mac daemon spec §3). Wired here so the
	// InteractionRecorder's staging registry can dispatch
	// source="messages" mark-processed calls correctly once the Mac
	// daemon ingest writer is live, and so the IngestService can upsert
	// staging rows from raw_message.* envelopes inside its per-event
	// savepoint.
	messagesMessageRepo := repository.NewMessagesMessageRepository(database.Queries)

	// External contact repo for the IngestService's inline
	// external_contact.* handler (Mac daemon iCloud Contacts watcher
	// path). Constructed unconditionally because the IngestService is
	// always wired even when external sync is disabled — the daemon
	// can still call /api/v1/ingest/events on a host-auth path. A
	// later block under `if cfg.Features.EnableExternalSync` reuses
	// the same instance for the Import / Calendar rematch wiring.
	externalContactRepoForIngest := repository.NewExternalContactRepository(database.Queries)

	// Mac host repo for the IngestService's per-batch host-liveness
	// re-check (SELECT ... FOR UPDATE on mac_host inside the batch tx).
	// Constructed here so the IngestService can take it as a dep; the
	// host-auth / pairing / heartbeat handlers below construct their
	// own MacHostService that wraps the same repo instance.
	macHostRepoForIngest := repository.NewMacHostRepository(database.Queries)

	// meeting_note repo + calendar repo + identity repo for the inline
	// meeting_note.recorded / .deleted handlers. Constructed
	// unconditionally because the IngestService is always wired even
	// when external sync is disabled — the Mac daemon can still post
	// meeting_note.* on the host-auth path. The calendarRepo here is
	// the same logical resource the feature-flagged
	// google.NewCalendarRematchHandler block below constructs; safe to
	// have two instances pointing at the same queries since
	// CalendarEventRepository is stateless.
	meetingNoteRepoForIngest := repository.NewMeetingNoteRepository(database.Queries)
	calendarRepoForIngest := repository.NewCalendarEventRepository(database.Queries)

	// PhoneCall repository for the IngestService's call.* inline
	// handler (phase 1.5).
	phoneCallRepoForIngest := repository.NewPhoneCallRepository(database.Queries)

	// Note: ingestService construction is hoisted DOWN to after
	// contactService / cadenceUpdater / followUpManager are built —
	// the call.* inline handler needs those collaborators to emit
	// interaction.recorded + apply cadence/follow-up in the same tx
	// as the staging-row write. See the construction near
	// "ingestService := service.NewIngestService" further below.

	// Rematch service — constructed above ContactService so it can be
	// passed as the RematchRegistry constructor arg. Handlers register
	// later once their dependencies are constructed.
	rematchService := service.NewRematchService()

	// Initialize services
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, eventBus, rematchService)

	// Telegram message repo construction (hoisted above the
	// InteractionRecorder wiring so the consumer can mark messages
	// processed in the same tx as the interaction insert).
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)
	// Comms message repo (shared cross-source content store). Hoisted here
	// so the staging registry below can add the gchat session-scoped
	// processor; also reused by the email-consumer wiring + the gchat
	// aggregation engine further down.
	commsMessageRepo := repository.NewCommsMessageRepository(database.Queries)

	// Source-neutral staging registry — InteractionRecorder dispatches
	// MarkProcessedTx by env.Source to the right repository. Unknown
	// sources are a logged warning (not an error) so non-message kinds
	// continue to bypass the registry without erroring.
	stagingRegistry := repository.NewStagingProcessorRegistry(
		map[string]repository.StagingProcessor{
			repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramMessageRepo),
			repository.InteractionSourceMessages: repository.NewMessagesStagingProcessor(messagesMessageRepo),
			// GChat create-path: the InteractionRecorder consumer marks
			// comms_message(source='gchat') rows processed via this
			// session-scoped processor. Without it the recorder's
			// zero-rows-affected rollback fires and the engine reprocesses
			// forever. Inert until a chat provider writes gchat rows.
			repository.InteractionSourceGChat: repository.NewCommsSessionStagingProcessor(commsMessageRepo),
		},
	)

	// Aggregator reenqueuer holder. The Telegram entry needs the
	// Telegram aggregation engine, which is constructed later (inside
	// the cfg.Features.EnableTelegramSync branch). The worker is
	// constructed earlier, so we wrap the registry in a deferred
	// pointer that the Telegram branch fills in once the engine
	// exists. When Telegram is disabled the registry's telegram entry
	// stays unset and the consumer's reenqueue degrades to "no entry
	// registered for source" (logged warn) — safe, the consumer's
	// interaction has already committed.
	aggregatorReenqueuerHolder := &deferredAggregatorReenqueuer{}

	// CadenceUpdater must be constructed BEFORE InteractionRecorder so
	// the recorder can inline-invoke it after bus.PublishTx on fresh
	// writes. Wired here even though its worker is registered further
	// down, so the construction order matches the runtime dispatch
	// order. contactRepo.SetPool is called at the first writer-path
	// construction so the cadence updater can open its own tx if ever
	// needed outside the caller's tx (defensive — the current path
	// always runs in the caller's tx).
	contactRepo.SetPool(database.Pool)
	eventClaimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		eventClaimRepo,
		contactRepo,
		database.Queries,
		consumer.CadenceModeFromConfig(cfg.EventBus.CadenceMode),
		cfg.EventBus.UnsafeAllowOffMode,
	)

	// Wire CadenceUpdater into ContactService so Merge / Extend / Promote
	// / UpdateContact cadence-edit paths route cadence writes through
	// the sole writer.
	contactService.SetCadenceUpdater(cadenceUpdater)

	// FollowUpManager consumer — the sole writer of
	// contact_task.kind='follow_up' lifecycle post-cutover. Constructed
	// BEFORE the InteractionRecorder because the recorder takes it as a
	// constructor arg (inline-invoke on fresh writes). Todoist settings
	// are looked up via a deferred holder populated later in the
	// external-sync branch; until populated, Todoist-dependent post-
	// commit paths (refresh item_update, close retries) degrade to
	// local-only writes with a logged warning.
	followUpMode := consumer.FollowUpModeFromConfig(cfg.EventBus.FollowUpMode)
	followUpSettingsHolder := &followUpSettingsRef{frontendURL: cfg.CORS.FrontendURL}
	followUpSettings := followUpSettingsHolder.fn()
	followUpManager := consumer.NewFollowUpManager(
		followUpMode,
		eventClaimRepo,
		contactRepo,
		contactTaskRepo,
		contactTaskRepo,
		interactionRepo,
		riverClient,
		database.Pool,
		followUpSettings,
		todoist.DefaultClientFactory,
		cfg.CORS.FrontendURL,
		cfg.Watchdog,
	)
	// Wire the consumer as the sole follow-up writer on the direct path.
	// Non-bus callers (Todoist completion, Promote/Extend) route through
	// FollowUpManager.ApplyInteraction via ContactService.
	contactService.SetFollowUpConsumer(followUpManager)

	// InteractionRecorder consumer + manual handler (spec §3.4.1).
	// Delegates the write to ContactService.RecordInteractionTx, then
	// marks telegram_messages processed (for message.* kinds) and emits
	// interaction.recorded — all inside the caller's tx. After emitting
	// interaction.recorded, the recorder inline-invokes
	// cadenceUpdater.HandleEvent + followUpManager.HandleEvent on
	// fresh writes so cadence + follow-up state apply synchronously and
	// queued re-deliveries become durable no-ops via
	// event_consumer_claim.
	interactionRecorder := consumer.NewInteractionRecorder(
		contactService,
		stagingRegistry,
		eventBus,
		cadenceUpdater,
		followUpManager,
		// calendarRepoForIngest satisfies calendarEventLocker: the
		// calendar.attended branch takes a FOR SHARE lock on the backing
		// calendar_event so a concurrent decline DELETE cannot strand a
		// false interaction.
		calendarRepoForIngest,
	)

	// IngestService — hoisted here so the call.* inline handler can
	// reuse contactService.RecordInteractionTx, cadenceUpdater,
	// followUpManager, and eventBus.PublishTx to emit
	// interaction.recorded + apply cadence + follow-up in the SAME tx
	// as the phone_call staging row write (spec §`phone_calls`
	// content-delivered cadence). raw_message.* and external_contact.*
	// inline handlers don't need these four — they were nil before the
	// v1.5 expansion. The meeting_note.* inline handler reuses
	// contactService via the ContactInteractionRecorder interface for
	// session-attributed interaction writes so cadence + follow-up
	// fire correctly.
	//
	// anarlog title-extraction deps for the meeting_note.recorded inline
	// handler: TitleMatcher disambiguates a single name token against
	// the CRM contact table (trigram + collision-gap); DiscoveryWriter
	// persists weak-candidate anarlog_title rows for unmatched tokens.
	titleMatcher := anarlog.NewTitleMatcher(contactRepo)
	titleDiscoveryWriter := anarlog.NewDiscoveryWriter(externalContactRepoForIngest)
	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityServiceForIngest,
		messagesMessageRepo,
		riverClient,
		externalContactRepoForIngest,
		macHostRepoForIngest, // host-liveness re-check inside the batch tx
		meetingNoteRepoForIngest,
		calendarRepoForIngest,
		interactionRepo,
		identityRepoForIngest,
		contactService,
		phoneCallRepoForIngest,
		contactService,
		cadenceUpdater,
		followUpManager,
		titleMatcher,
		titleDiscoveryWriter,
		phoneCallRepoForIngest, // phone_call linkage candidates for meeting_note Step 1
	)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	// User-driven conflict-resolution surface for meeting_note rows.
	// Mirrors the daemon-side IngestService.handleMeetingNoteRecorded
	// path; uses a small adapter to satisfy the polymorphic
	// LinkageTargetReader interface (event vs phone_call lookup by UUID).
	meetingNoteLinkageTargets := &meetingNoteLinkageTargetReader{
		calendarRepo:  calendarRepoForIngest,
		phoneCallRepo: phoneCallRepoForIngest,
	}
	meetingNoteService := service.NewMeetingNoteService(
		database,
		meetingNoteRepoForIngest,
		meetingNoteRepoForIngest,
		meetingNoteLinkageTargets,
		identityRepoForIngest,
		titleMatcher,
		titleDiscoveryWriter,
		contactService,
		contactRepo,
	)
	meetingNoteHandler := handlers.NewMeetingNoteHandler(meetingNoteService)

	// Register the consumer worker. The worker is registered UNCONDITIONALLY —
	// river rejects unknown job kinds at dequeue time, so having the worker
	// present with mode=off costs nothing (no events route to it when
	// pubBus is nil). Mode gating happens at the publisher sites via
	// pubBus and at the manual-handler level via manualHandler.
	//
	// aggregatorReenqueuerHolder is wired in here; the actual telegram
	// entry is filled by the cfg.Features.EnableTelegramSync branch
	// further down. Until that branch runs (or in test/no-telegram
	// modes), the holder dispatches to a logged-warn no-op.
	river.AddWorker(riverWorkers, consumer.NewInteractionRecorderWorker(eventBus, database.Pool, interactionRecorder, aggregatorReenqueuerHolder))

	// Calendar decline consumer: when a stored calendar_event is
	// declined / cancelled / user-removed upstream, the publisher removes
	// the row + emits calendar.declined per matched contact; this consumer
	// soft-deletes the derived gcal interaction and recomputes the contact's
	// date columns. Registered unconditionally — no events route to it when
	// the publisher (CalendarSyncProvider) is in off mode.
	calendarDeclineHandler := consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo)
	river.AddWorker(riverWorkers, consumer.NewCalendarDeclineHandlerWorker(eventBus, database.Pool, calendarDeclineHandler))

	// Email-interaction consumer: derives a per-(contact, thread, local-day)
	// aggregated interaction from email.received / email.sent events + their
	// comms_message content rows. contactService fills both the
	// interactionWriter slot (create branch) and the emailAggregator slot
	// (found-branch extend/promote). cadenceUpdater + followUpManager are the
	// SAME instances the InteractionRecorder uses, so the create branch's
	// inline cadence/follow-up apply shares the durable event-claim store.
	// Registered unconditionally; it processes the email.received /
	// email.sent events the Gmail provider publishes in production (and
	// stays idle when no such event is routed, e.g. event-bus off mode).
	// commsMessageRepo was hoisted earlier (above the staging registry).
	emailInteractionConsumer := consumer.NewEmailInteractionConsumer(
		contactService, commsMessageRepo, interactionRepo, contactService,
		eventBus, cadenceUpdater, followUpManager,
	)
	river.AddWorker(riverWorkers, consumer.NewEmailInteractionConsumerWorker(eventBus, database.Pool, emailInteractionConsumer))

	// Interaction-mode wiring gate. Cutover is the normal operating
	// posture; off is the emergency-override retained so rollback can
	// silence publisher-driven paths without a code change. A deploy in
	// off mode does NOT restore any pre-cutover direct path — rollback
	// is `git revert`.
	effectiveMode := cfg.EventBus.InteractionMode
	var pubBus *events.Bus
	var manualHandler *service.ManualInteractionHandler
	switch effectiveMode {
	case config.EventBusInteractionModeCutover:
		pubBus = eventBus
		manualHandler = service.NewManualInteractionHandler(database.Pool, eventBus, interactionRecorder)
		logger.Info().
			Str("mode", "cutover").
			Msg("event-bus interaction consumer: cutover active")
	default: // off
		pubBus = nil
		manualHandler = nil
		logger.Warn().
			Str("mode", effectiveMode).
			Msg("event-bus interaction consumer: mode=off — publisher-driven " +
				"(telegram/calendar/manual) interactions will NOT be recorded. " +
				"HTTP ingest path is unaffected. Use EVENT_BUS_INTERACTION_MODE=cutover (default) to restore publisher paths.")
	}

	// Informational warning when ingest is enabled but cutover isn't —
	// ingested events still write interactions (the HTTP ingest path is
	// an intentional carve-out of the off-mode gate); this log line
	// makes the seam visible in operator logs.
	if cfg.Features.EnableEventBusIngest && effectiveMode != config.EventBusInteractionModeCutover {
		logger.Warn().
			Str("interaction_mode", effectiveMode).
			Bool("ingest_enabled", cfg.Features.EnableEventBusIngest).
			Msg("event-bus ingest enabled but InteractionRecorder is not in cutover mode; " +
				"ingested events WILL still be written by the consumer — the mode=off warning " +
				"above does NOT apply to ingested-event-driven writes.")
	}

	// CadenceUpdater is constructed above (alongside InteractionRecorder).
	// Register its river worker unconditionally — events.consumerJobsForKind
	// always enqueues a cadence_updater job for interaction.recorded. In
	// cutover mode the inline recorder path claims the event first, so
	// this worker is almost always a durable no-op on re-delivery. In
	// mode=off HandleEvent short-circuits before any DB write.
	river.AddWorker(riverWorkers, consumer.NewCadenceUpdaterWorker(eventBus, database.Pool, cadenceUpdater))

	// FollowUpManager + river workers. Routing is config-blind
	// (events.consumerJobsForKind always enqueues cadence + follow-up
	// jobs for interaction.recorded); HandleEvent short-circuits on
	// mode=off without DB writes. The Todoist create / close / refresh
	// workers are registered so river knows their kinds even when
	// Todoist isn't wired — in that case the settings func returns an
	// ErrNoTodoistAccount-equivalent error and the worker returns a
	// retryable failure for river to back off.
	river.AddWorker(riverWorkers, consumer.NewFollowUpManagerWorker(eventBus, database.Pool, followUpManager))
	river.AddWorker(riverWorkers, consumer.NewTodoistFollowUpCreateJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoist.DefaultClientFactory, riverClient, database.Pool,
	))
	river.AddWorker(riverWorkers, consumer.NewTodoistFollowUpCloseJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoist.DefaultClientFactory,
	))
	river.AddWorker(riverWorkers, consumer.NewTodoistFollowUpRefreshJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoist.DefaultClientFactory,
	))

	switch cfg.EventBus.FollowUpMode {
	case config.EventBusFollowUpModeCutover:
		logger.Info().
			Str("mode", "cutover").
			Msg("event-bus FollowUpManager: cutover active (sole writer of follow-up tasks; inline recorder dispatch enabled)")
	default: // off
		cfg.EventBus.MaybeWarnUnsafeOff()
		logger.Warn().
			Str("mode", "off").
			Msg("event-bus FollowUpManager: mode=off active — NO follow-up tasks will be created or completed until EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF is unset or a `git revert` ships")
	}

	switch cfg.EventBus.CadenceMode {
	case config.EventBusCadenceModeCutover:
		logger.Info().
			Str("mode", "cutover").
			Msg("event-bus CadenceUpdater: cutover active (sole writer of cadence columns; inline recorder dispatch enabled)")
	default: // off
		// Validate() already rejected this unless UnsafeAllowOffMode is
		// true; we reach here only via the emergency escape hatch. The
		// WARN log in config.Load already fired; repeat it here for
		// observability on the main-wire path.
		cfg.EventBus.MaybeWarnUnsafeOff()
		logger.Warn().
			Str("mode", "off").
			Msg("event-bus CadenceUpdater: mode=off active — NO cadence columns will be updated until EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF is unset or a `git revert` ships")
	}
	noteService := service.NewNoteService(noteRepo, contactRepo)
	importMatchService := service.NewImportMatchService(contactRepo)
	// EnrichmentService is shared by the import handler (link/import flows) and
	// the Telegram peer matcher (auto-match enrichment). Constructed at outer
	// scope so both feature blocks share a single instance.
	enrichmentService := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo, eventBus, rematchService)
	enrichmentService.SetCadenceUpdater(cadenceUpdater)

	// Address-book method reconcile: re-propagates address-book methods
	// onto already-linked contacts (auto-propagate for matched, record
	// suggestion for imported). Shared by the gcontacts forward hook and
	// the icloud post-commit hook so the auto-vs-suggest + dup precedence
	// logic lives in one place.
	addressBookReconcileService := service.NewAddressBookReconcileService(
		enrichmentService,
		contactRepo,
		contactMethodRepo,
		externalContactRepoForIngest,
	)
	ingestService.SetAddressBookReconciler(addressBookReconcileService)

	// Rematch dispatcher consumer — subscribes to contact_methods.added
	// events and runs RematchService.Run with per-contact mutex
	// serialization. Always-on (no mode flag): a registered River
	// worker that returned nil in kill-switch mode would permanently
	// ack queued jobs, so rollback is `git revert` only. Rematch
	// handlers themselves (calendar, telegram) are registered below
	// once their deps are constructed.
	rematchDispatcher := consumer.NewRematchDispatcher(rematchService)
	river.AddWorker(riverWorkers, consumer.NewRematchDispatcherWorker(eventBus, database.Pool, rematchDispatcher))
	logger.Info().Msg("event-bus RematchDispatcher: cutover active")

	// Initialize external sync components (feature-flagged)
	var syncService *service.SyncService
	var syncHandler *handlers.SyncHandler
	var identityHandler *handlers.IdentityHandler
	var oauthHandler *handlers.OAuthHandler
	var importHandler *handlers.ImportHandler
	var suggestionHandler *handlers.SuggestionHandler
	var anarlogDiscoveryHandler *handlers.AnarlogDiscoveryHandler
	var calendarHandler *handlers.CalendarHandler
	var todoistHandler *handlers.TodoistHandler
	var contactTaskHandler *handlers.ContactTaskHandler
	var googleOAuthService *google.OAuthService
	var todoistOAuthService *todoist.OAuthService
	var externalContactRepo *repository.ExternalContactRepository

	// GChat provider + enabled-state lister, hoisted to function scope so the
	// LATE depth-0 gchatEngine block (below, OUTSIDE the EnableExternalSync block)
	// can register the gchat rematch handlers. The provider is constructed but
	// INTENTIONALLY NOT registered into providerRegistry: the scheduler must
	// never run it until an enablement/boot-reconciliation path creates an
	// enabled external_sync_state(source='gchat') row (not wired here). Both stay
	// nil unless Google OAuth is configured AND external sync is enabled.
	var gchatProvider *google.GChatSyncProvider
	var gchatSyncStates google.GChatSyncStateLister

	if cfg.Features.EnableExternalSync {
		syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
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

				// Populate the FollowUpManager's deferred Todoist settings
				// ref so the cutover consumer can resolve settings via the
				// real OAuth service for its post-commit refresh / close /
				// retry workers.
				followUpSettingsHolder.oauth = todoistOAuthService
				followUpSettingsHolder.sync = syncRepo

				logger.Info().Msg("Todoist OAuth service initialized")
			}
		} else {
			logger.Info().Msg("Todoist OAuth not configured (TODOIST_CLIENT_ID and TODOIST_CLIENT_SECRET required)")
		}

		// External contact repository — reuse the instance constructed
		// at outer scope for the IngestService so all code paths share
		// the same repo (no behavioral difference today; future stateful
		// caching is centralized).
		externalContactRepo = externalContactRepoForIngest

		// Initialize identity service (enrichmentService is constructed at outer scope
		// so the Telegram block can share it).
		identityService := service.NewIdentityService(identityRepo)

		// Calendar repo + handler + rematch handler are wired whenever external
		// sync is enabled, regardless of OAuth configuration. Rematch over
		// calendar_event is pure DB work and must run in test/local environments
		// that don't have Google OAuth set up.
		calendarRepo := repository.NewCalendarEventRepository(database.Queries)
		calendarHandler = handlers.NewCalendarHandler(calendarRepo)
		rematchService.Register(google.NewCalendarRematchHandler(calendarRepo, externalContactRepo, pubBus))
		logger.Info().Msg("Calendar rematch handler registered")

		// Register Google Contacts provider if OAuth is configured
		if googleOAuthService != nil {
			gcontactsProvider := google.NewContactsProvider(
				googleOAuthService,
				externalContactRepo,
				enrichmentService,
				identityService,
				addressBookReconcileService,
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
				pubBus,
				database.Pool,
			)
			providerRegistry.Register(gcalProvider)
			logger.Info().Msg("Google Calendar sync provider registered")

			// Gmail provider + rematch handler: publisher-driven, so register
			// ONLY in cutover mode. In off-mode pubBus is a nil *events.Bus;
			// passing it into the provider's busTx interface field would create
			// a non-nil-interface-wrapping-typed-nil and bypass the provider's
			// own `bus == nil` guard, panicking on the first PublishTx. Off-mode
			// is an emergency rollback posture where no publisher should run.
			// commsMessageRepo is reused from the email-consumer wiring above.
			if pubBus != nil {
				gmailProvider := google.NewGmailSyncProvider(
					googleOAuthService,
					commsMessageRepo,
					pubBus,
					database.Pool,
				)
				providerRegistry.Register(gmailProvider)
				// syncRepo is the enabled-email-states lister: the rematch scan
				// runs only over accounts whose email sync is enabled (the same
				// gate the scheduler uses), not every connected OAuth account.
				rematchService.Register(google.NewGmailRematchHandler(
					gmailProvider,
					syncRepo,
					commsMessageRepo,
				))
				// Correspondence discovery: an in-sync hook that runs the link-only
				// candidate gate over every fetched message's From/To/Cc
				// participants (between fetch and storage), so multi-party threads
				// the storage gate drops still surface unknown addresses that
				// strong-match an existing contact. Wired into the provider via a
				// setter (nil-safe; the hook is a no-op when unset). No periodic
				// job — discovery piggybacks the existing sync fetch.
				gmailProvider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(
					contactRepo,
					externalContactRepo,
				))
				logger.Info().Msg("Gmail sync provider + rematch handler + correspondence discovery registered")
			} else {
				logger.Warn().Msg("Gmail provider NOT registered: event-bus interaction mode=off (pubBus nil)")
			}

			// GChat provider: constructed but NOT registered into
			// providerRegistry — the scheduler must never run it until enablement
			// exists (INERT). It is store-only + event-free, so unlike Gmail it
			// does NOT gate on pubBus. gchatSyncStates = syncRepo is the
			// enabled-state lister the gchat rematch handlers gate on (registered
			// in the late depth-0 block).
			gchatProvider = google.NewGChatSyncProvider(
				googleOAuthService,
				commsMessageRepo,
				syncRepo,
			)
			gchatSyncStates = syncRepo
			logger.Info().Msg("GChat sync provider constructed (inert: not registered into provider registry)")
		}

		// Register Todoist Cadence provider if OAuth is configured
		if todoistOAuthService != nil {
			todoistProvider := todoist.NewCadenceSyncProvider(
				todoistOAuthService,
				contactTaskRepo,
				contactRepo,
				syncRepo,
				cfg,
				eventBus,
				cadenceUpdater,
				database.Pool,
				todoist.DefaultClientFactory,
			)
			providerRegistry.Register(todoistProvider)
			logger.Info().Msg("Todoist Cadence sync provider registered")

			// Follow-up lifecycle is handled by consumer.FollowUpManager
			// (wired above via SetFollowUpConsumer). The Todoist
			// dependency (settings + client factory) routes through
			// followUpSettingsHolder which was populated when Todoist
			// OAuth initialized. No follow-up service is constructed
			// here — FollowUpManager is the sole writer.

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

		// Register every Mac-daemon push-source provider. Each is
		// push-only — data lands via /api/v1/ingest/events, never via the
		// scheduler (ListDueAccounts skips push strategy) — so Sync() is a
		// no-op. The registration lives in one helper so the daemonFamily
		// agreement test can cross-check it against the descriptor table.
		push.RegisterPushProviders(providerRegistry)
		logger.Info().Msg("Push providers registered (messages, icloud_contacts, phone_calls)")

		syncService = service.NewSyncService(syncRepo, contactRepo, providerRegistry)

		// Email enablement reconciliation (Gmail go-live). Only meaningful in
		// cutover mode with a connected Google account: the Gmail provider is
		// registered only when pubBus != nil, so there is no point reconciling
		// email states no registered provider can serve. Wire the account
		// lister + OAuth-connect hook, then run the idempotent boot
		// reconciliation BEFORE riverClient.Start so the RunOnStart tick already
		// sees the freshly-enabled email states.
		if googleOAuthService != nil && pubBus != nil {
			syncService.SetEmailAccountLister(googleOAuthService)
			if oauthHandler != nil {
				oauthHandler.SetEmailStateReconciler(func(ctx context.Context) error {
					return syncService.ReconcileEmailSyncStates(ctx)
				})
			}
			if err := syncService.ReconcileEmailSyncStates(ctx); err != nil {
				// Non-fatal: the scheduler simply has nothing to do for email
				// until states exist; the next connect or next boot retries.
				logger.Warn().Err(err).Msg("boot email sync reconciliation failed (non-fatal)")
			}
		}

		syncHandler = handlers.NewSyncHandler(syncService)
		identityHandler = handlers.NewIdentityHandler(identityService)

		// Suggestion service composes the method-suggestion group with the
		// confidence-ranked candidate list and runs resolve/dismiss. Shared
		// by the import handler (its candidate sort) and the suggestion
		// handler (the new People-tab surface).
		suggestionService := service.NewSuggestionService(
			externalContactRepo,
			contactRepo,
			contactMethodRepo,
			enrichmentService,
			importMatchService,
			database,
		)
		suggestionHandler = handlers.NewSuggestionHandler(suggestionService)

		// Initialize import handler
		importHandler = handlers.NewImportHandler(externalContactRepo, identityServiceForIngest, contactService, importMatchService, enrichmentService, suggestionService)

		// Anarlog-title discovery surface (People-tab grouped weak
		// candidates + token-group resolve). Reuses the external_contact
		// repo and ContactService — both already constructed above.
		anarlogDiscoveryService := service.NewAnarlogDiscoveryService(externalContactRepo, contactService)
		anarlogDiscoveryHandler = handlers.NewAnarlogDiscoveryHandler(anarlogDiscoveryService)

		logger.Info().Msg("external sync infrastructure enabled")
	}

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
		// repo with a 'gchat'-pinned lister (built type, not inline struct,
		// per the single-file build convention).
		repository.InteractionSourceGChat: repository.NewCommsSourceContactLister(commsMessageRepo, repository.InteractionSourceGChat),
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
		ingestGroup := router.Group("/api/v1/ingest")
		ingestGroup.Use(ingestAuth)
		ingestGroup.POST("/events", ingestHandler.IngestEvents)
		logger.Info().Msg("event bus ingestion endpoint enabled")

		// Daemon recovery endpoint: the Mac daemon polls this on
		// startup to reconcile its local pending-notification table
		// against the Pi's current truth. Lives under composite auth
		// so the daemon's X-Mac-Host-ID + Bearer pair-key path
		// resolves; the frontend (global API key) can also reach it.
		meetingNoteRecoveryGroup := router.Group("/api/v1/meeting-notes")
		meetingNoteRecoveryGroup.Use(ingestAuth)
		meetingNoteRecoveryGroup.GET("/needs-attention", meetingNoteHandler.ListNeedsAttention)
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

		// Meeting-note conflict-resolution — user-driven, called from
		// the frontend with the global API key. Stays under the v1
		// APIKeyMiddleware group.
		meetingNotes := v1.Group("/meeting-notes")
		{
			meetingNotes.POST("/:id/resolve-link", meetingNoteHandler.ResolveLink)
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

		// Mac-daemon admin routes (under global API key middleware).
		// Pairing-token mint + revoke + list/get for the Mac settings UI.
		handlers.RegisterMacHostAdminRoutes(v1, macHostHandler)

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

			// Add contact task routes (manual tasks) if Todoist is configured
			if contactTaskHandler != nil {
				contacts.GET("/:id/tasks", contactTaskHandler.ListContactTasks)
				contacts.POST("/:id/tasks", contactTaskHandler.CreateManualTask)
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
					// Static anarlog-title discovery routes are declared
					// BEFORE the /:id param route so Gin's tree inserts the
					// static segment first and /imports/anarlog-title cannot
					// be shadowed by the :id match.
					if anarlogDiscoveryHandler != nil {
						imports.GET("/anarlog-title", anarlogDiscoveryHandler.ListAnarlogTitle)
						imports.POST("/anarlog-title/resolve", anarlogDiscoveryHandler.ResolveAnarlogTitle)
					}
					// Static suggestions routes are likewise declared BEFORE
					// the /:id param route so the literal `suggestions`
					// segment is not shadowed by the :id wildcard.
					if suggestionHandler != nil {
						imports.GET("/suggestions", suggestionHandler.ListSuggestions)
						imports.POST("/suggestions/:id/methods/resolve", suggestionHandler.ResolveMethodSuggestions)
						imports.POST("/suggestions/:id/methods/dismiss", suggestionHandler.DismissMethodSuggestions)
					}
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

			testHandler := handlers.NewTestHandler(database, testExternalRepo, contactService, testCalendarRepo, macHostRepo, meetingNoteRepoForIngest)
			testRoutes := v1.Group("/test")
			{
				testRoutes.POST("/seed/contacts", testHandler.SeedContacts)
				testRoutes.POST("/seed/external-contacts", testHandler.SeedExternalContacts)
				testRoutes.POST("/seed/method-suggestions", testHandler.SeedMethodSuggestions)
				testRoutes.POST("/seed/overdue-contacts", testHandler.SeedOverdueContacts)
				testRoutes.POST("/seed/calendar-events", testHandler.SeedCalendarEvents)
				testRoutes.POST("/seed/mac-hosts", testHandler.SeedMacHost)
				testRoutes.POST("/seed/meeting-notes", testHandler.SeedMeetingNotes)
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
		// telegramManager.Stop, riverClient.Stop, cancel).
		return 1
	}
	return 0
}
