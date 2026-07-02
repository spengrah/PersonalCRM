package main

import (
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// eventConsumers holds the event-bus consumers + their shared collaborators
// constructed before the interaction-mode gate. The InteractionRecorder,
// CadenceUpdater, KnowledgeCacheUpdater, and FollowUpManager are the sole
// writers of their respective columns; the holders carry deferred wiring
// (Telegram aggregation engine, Todoist settings) populated later.
type eventConsumers struct {
	AggregatorReenqueuerHolder *deferredAggregatorReenqueuer
	EventClaimRepo             *repository.EventConsumerClaimRepository
	CadenceUpdater             *consumer.CadenceUpdater
	KnowledgeCacheUpdater      *consumer.KnowledgeCacheUpdater
	TodoistClientFactory       todoist.ClientFactory
	FollowUpMode               string
	FollowUpSettingsHolder     *followUpSettingsRef
	FollowUpSettings           consumer.TodoistSettingsFunc
	FollowUpManager            *consumer.FollowUpManager
	InteractionRecorder        *consumer.InteractionRecorder
}

// buildEventConsumers constructs the CadenceUpdater, KnowledgeCacheUpdater,
// FollowUpManager, and InteractionRecorder in their required order (each
// downstream consumer takes the earlier ones as constructor args), wiring
// them into ContactService via its setters exactly as before. The Todoist
// client factory + writes-enabled boot log and the deferred holders are
// created here too. No River workers are registered here — that happens in
// registerCoreConsumerWorkers / registerModeWorkers.
func buildEventConsumers(
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	graph contactCore,
	ingest ingestRepos,
	messaging messagingFoundation,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) eventConsumers {
	contactRepo := core.Contact
	contactTaskRepo := core.ContactTask
	interactionRepo := core.Interaction
	contactService := graph.ContactService
	assertService := graph.AssertService
	graphAssertionRepo := graph.GraphAssertion
	graphNodeRepo := graph.GraphNode
	stagingRegistry := messaging.StagingRegistry
	venueResolver := messaging.VenueResolver
	calendarRepoForIngest := ingest.CalendarEvent

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

	// Knowledge-cache consumer (the location/birthday/how_met authority flip):
	// the sole writer of those three derived cache columns. ContactService emits
	// lives_in/birthday/how_met assertions through AssertService and calls
	// knowledgeCacheUpdater.RefreshTx inline (no read-path gap on a user edit);
	// the registered KnowledgeCacheUpdaterWorker (below) handles assertion.accepted
	// /superseded events from any other producer (extractors, rollover, retraction).
	knowledgeCacheUpdater := consumer.NewKnowledgeCacheUpdater(graphAssertionRepo, graphNodeRepo, contactRepo)
	contactService.SetKnowledgeWriter(assertService, knowledgeCacheUpdater)

	// Todoist client factory, built once with the running CRM_ENV so the
	// outbound-write guard (Spec C) applies at every production write site.
	// Non-prod instances holding real OAuth tokens (a restored prod DB, a
	// partial env copy) refuse Todoist writes; see todoist.ErrNonProdWriteRefused.
	todoistClientFactory := todoist.NewClientFactory(cfg.Runtime.CRMEnvironment)
	if config.IsProductionCRMEnv(cfg.Runtime.CRMEnvironment) {
		// Logged at Warn so it survives prod's LOG_LEVEL=warn threshold — this
		// is the operator-facing signal that writes are live. Expected on every
		// prod boot; the stable "event" field lets alerting route it as
		// informational.
		logger.Warn().
			Str("event", "todoist_writes_enabled").
			Str("crm_env", cfg.Runtime.CRMEnvironment).
			Bool("todoist_writes_enabled", true).
			Msg("Todoist outbound writes ENABLED (production CRM_ENV)")
	} else {
		logger.Info().
			Str("event", "todoist_writes_enabled").
			Str("crm_env", cfg.Runtime.CRMEnvironment).
			Bool("todoist_writes_enabled", false).
			Msg("Todoist outbound writes refused (non-production CRM_ENV)")
	}

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
		todoistClientFactory,
		cfg.CORS.FrontendURL,
		cfg.Watchdog,
	)
	// Wire the consumer as the sole follow-up writer on the direct path.
	// Non-bus callers (Todoist completion, Promote/Extend) route through
	// FollowUpManager.ApplyInteraction via ContactService.
	contactService.SetFollowUpConsumer(followUpManager)

	// Wire the merge-time task-close enqueuer. MergeContacts closes the
	// source contact's live automated tasks and enqueues the durable Todoist
	// close job for rows with a real external id; the mode gate matches the
	// close worker's cutover-only contract (enqueuing in mode 'off' would
	// only manufacture failing jobs — merge then closes locally with a WARN,
	// that mode's documented "completion disabled" semantics).
	contactService.SetTaskCloseEnqueuer(riverClient, cfg.EventBus.FollowUpMode == config.EventBusFollowUpModeCutover)

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
	// Populate interaction.venue_id for message.* (telegram/messages/gchat) and
	// gcal interactions this recorder writes.
	interactionRecorder.SetVenueResolver(venueResolver)

	return eventConsumers{
		AggregatorReenqueuerHolder: aggregatorReenqueuerHolder,
		EventClaimRepo:             eventClaimRepo,
		CadenceUpdater:             cadenceUpdater,
		KnowledgeCacheUpdater:      knowledgeCacheUpdater,
		TodoistClientFactory:       todoistClientFactory,
		FollowUpMode:               followUpMode,
		FollowUpSettingsHolder:     followUpSettingsHolder,
		FollowUpSettings:           followUpSettings,
		FollowUpManager:            followUpManager,
		InteractionRecorder:        interactionRecorder,
	}
}
