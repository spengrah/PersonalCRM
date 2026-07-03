package main

import (
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// eventConsumers holds the event-bus consumers + their shared collaborators
// constructed before ContactService (INV-5 order). The CadenceUpdater,
// KnowledgeCacheUpdater, and FollowUpManager are the sole writers of their
// respective columns; the holders carry deferred wiring (Telegram aggregation
// engine, Todoist settings) populated later. The InteractionRecorder is NOT
// held here — it takes ContactService as a ctor arg, so it is built by
// buildInteractionRecorder after buildContactService.
type eventConsumers struct {
	AggregatorReenqueuerHolder *deferredAggregatorReenqueuer
	CadenceUpdater             *consumer.CadenceUpdater
	KnowledgeCacheUpdater      *consumer.KnowledgeCacheUpdater
	TodoistClientFactory       todoist.ClientFactory
	FollowUpMode               string
	FollowUpSettingsHolder     *followUpSettingsRef
	FollowUpSettings           consumer.TodoistSettingsFunc
	FollowUpManager            *consumer.FollowUpManager
}

// buildDomainConsumers constructs the CadenceUpdater, KnowledgeCacheUpdater,
// and FollowUpManager in their required order (each downstream consumer takes
// the earlier ones as constructor args). These are built BEFORE ContactService
// (INV-5) so they can be passed to NewContactService as constructor args. The
// Todoist client factory + writes-enabled boot log and the deferred holders
// are created here too. The InteractionRecorder is built separately
// (buildInteractionRecorder) after ContactService exists. No River workers are
// registered here — that happens in registerCoreConsumerWorkers /
// registerModeWorkers.
func buildDomainConsumers(
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	graph graphCore,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) eventConsumers {
	contactRepo := core.Contact
	contactTaskRepo := core.ContactTask
	interactionRepo := core.Interaction
	graphAssertionRepo := graph.GraphAssertion
	graphNodeRepo := graph.GraphNode

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

	// CadenceUpdater must be constructed BEFORE ContactService so it can be
	// passed as a NewContactService arg (inline-invoked after bus.PublishTx on
	// fresh writes). Wired here even though its worker is registered further
	// down, so the construction order matches the runtime dispatch order.
	// contactRepo.SetPool is called at the first writer-path construction so
	// the cadence updater can open its own tx if ever needed outside the
	// caller's tx (defensive — the current path always runs in the caller's tx).
	contactRepo.SetPool(database.Pool)
	eventClaimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		eventClaimRepo,
		contactRepo,
		database.Queries,
		consumer.CadenceModeFromConfig(cfg.EventBus.CadenceMode),
		cfg.EventBus.UnsafeAllowOffMode,
	)

	// Knowledge-cache consumer (the location/birthday/how_met authority flip):
	// the sole writer of those three derived cache columns. ContactService emits
	// lives_in/birthday/how_met assertions through AssertService and calls
	// knowledgeCacheUpdater.RefreshTx inline (no read-path gap on a user edit);
	// the registered KnowledgeCacheUpdaterWorker (below) handles assertion.accepted
	// /superseded events from any other producer (extractors, rollover, retraction).
	// Built before ContactService so it (with graph.AssertService) is passed as
	// the knowledge-writer ctor pair.
	knowledgeCacheUpdater := consumer.NewKnowledgeCacheUpdater(graphAssertionRepo, graphNodeRepo, contactRepo)

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
	// BEFORE ContactService because it is passed to NewContactService as the
	// followUp arg (and to the InteractionRecorder for inline-invoke on fresh
	// writes). Todoist settings are looked up via a deferred holder populated
	// later in the external-sync branch; until populated, Todoist-dependent
	// post-commit paths (refresh item_update, close retries) degrade to
	// local-only writes with a logged warning.
	followUpMode := consumer.FollowUpModeFromConfig(cfg.EventBus.FollowUpMode)
	followUpSettingsHolder := &followUpSettingsRef{}
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

	return eventConsumers{
		AggregatorReenqueuerHolder: aggregatorReenqueuerHolder,
		CadenceUpdater:             cadenceUpdater,
		KnowledgeCacheUpdater:      knowledgeCacheUpdater,
		TodoistClientFactory:       todoistClientFactory,
		FollowUpMode:               followUpMode,
		FollowUpSettingsHolder:     followUpSettingsHolder,
		FollowUpSettings:           followUpSettings,
		FollowUpManager:            followUpManager,
	}
}

// buildInteractionRecorder constructs the InteractionRecorder — the consumer
// that delegates the write to ContactService.RecordInteractionTx, marks
// telegram_messages processed (for message.* kinds), and emits
// interaction.recorded (all inside the caller's tx). After emitting
// interaction.recorded, the recorder inline-invokes cadenceUpdater.HandleEvent
// + followUpManager.HandleEvent on fresh writes so cadence + follow-up state
// apply synchronously and queued re-deliveries become durable no-ops via
// event_consumer_claim. Built AFTER ContactService (it takes contactService as
// a ctor arg), unlike the sole-writer consumers built in buildDomainConsumers.
func buildInteractionRecorder(
	contactService *service.ContactService,
	messaging messagingFoundation,
	ingest ingestRepos,
	consumers eventConsumers,
	eventBus *events.Bus,
) *consumer.InteractionRecorder {
	interactionRecorder := consumer.NewInteractionRecorder(
		contactService,
		messaging.StagingRegistry,
		eventBus,
		consumers.CadenceUpdater,
		consumers.FollowUpManager,
		// ingest.CalendarEvent satisfies calendarEventLocker: the
		// calendar.attended branch takes a FOR SHARE lock on the backing
		// calendar_event so a concurrent decline DELETE cannot strand a
		// false interaction.
		ingest.CalendarEvent,
	)
	// Populate interaction.venue_id for message.* (telegram/messages/gchat) and
	// gcal interactions this recorder writes.
	interactionRecorder.SetVenueResolver(messaging.VenueResolver)
	return interactionRecorder
}

// registerCoreConsumerWorkers registers the three always-on core consumer
// workers (InteractionRecorder, CalendarDecline, EmailInteraction). Each is
// registered UNCONDITIONALLY — river rejects unknown job kinds at dequeue
// time, so having the worker present with mode=off costs nothing (no events
// route to it when pubBus is nil). Mode gating happens at the publisher
// sites via pubBus and at the manual-handler level via manualHandler.
func registerCoreConsumerWorkers(
	reg *riverRegistrar,
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	interactionRecorder *consumer.InteractionRecorder,
	messaging messagingFoundation,
	consumers eventConsumers,
	eventBus *events.Bus,
) {
	interactionRepo := core.Interaction
	contactRepo := core.Contact
	commsMessageRepo := messaging.CommsMessageRepo
	venueRepo := messaging.VenueRepo
	aggregatorReenqueuerHolder := consumers.AggregatorReenqueuerHolder
	cadenceUpdater := consumers.CadenceUpdater
	followUpManager := consumers.FollowUpManager

	// aggregatorReenqueuerHolder is wired in here; the actual telegram
	// entry is filled by the cfg.Features.EnableTelegramSync branch
	// further down. Until that branch runs (or in test/no-telegram
	// modes), the holder dispatches to a logged-warn no-op.
	addWorker(reg, consumer.NewInteractionRecorderWorker(eventBus, database.Pool, interactionRecorder, aggregatorReenqueuerHolder))

	// Calendar decline consumer: when a stored calendar_event is
	// declined / cancelled / user-removed upstream, the publisher removes
	// the row + emits calendar.declined per matched contact; this consumer
	// soft-deletes the derived gcal interaction and recomputes the contact's
	// date columns. Registered unconditionally — no events route to it when
	// the publisher (CalendarSyncProvider) is in off mode.
	calendarDeclineHandler := consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo)
	addWorker(reg, consumer.NewCalendarDeclineHandlerWorker(eventBus, database.Pool, calendarDeclineHandler))

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
	// Populate interaction.venue_id with the email-thread venue on the create
	// branch. The venue repo resolves directly (email carries thread_id).
	emailInteractionConsumer.SetVenueResolver(venueRepo)
	addWorker(reg, consumer.NewEmailInteractionConsumerWorker(eventBus, database.Pool, emailInteractionConsumer))
}

// resolveInteractionMode applies the interaction-mode wiring gate. Cutover
// is the normal operating posture; off is the emergency-override retained so
// rollback can silence publisher-driven paths without a code change. A
// deploy in off mode does NOT restore any pre-cutover direct path — rollback
// is `git revert`. Returns pubBus as a CONCRETE *events.Bus (nil in off
// mode) so it is threaded by concrete type through the provider wiring, and
// the manual-interaction handler (nil in off mode).
func resolveInteractionMode(cfg *config.Config, database *db.Database, interactionRecorder *consumer.InteractionRecorder, eventBus *events.Bus) (*events.Bus, *service.ManualInteractionHandler) {
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

	return pubBus, manualHandler
}

// registerModeWorkers registers the cadence / knowledge-cache / follow-up /
// Todoist follow-up workers (all config-blind; HandleEvent short-circuits on
// mode=off) and emits the follow-up + cadence mode boot logs.
func registerModeWorkers(
	reg *riverRegistrar,
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	consumers eventConsumers,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) {
	contactTaskRepo := core.ContactTask
	cadenceUpdater := consumers.CadenceUpdater
	knowledgeCacheUpdater := consumers.KnowledgeCacheUpdater
	followUpManager := consumers.FollowUpManager
	followUpMode := consumers.FollowUpMode
	followUpSettings := consumers.FollowUpSettings
	todoistClientFactory := consumers.TodoistClientFactory

	// CadenceUpdater is constructed above (alongside InteractionRecorder).
	// Register its river worker unconditionally — events.consumerJobsForKind
	// always enqueues a cadence_updater job for interaction.recorded. In
	// cutover mode the inline recorder path claims the event first, so
	// this worker is almost always a durable no-op on re-delivery. In
	// mode=off HandleEvent short-circuits before any DB write.
	addWorker(reg, consumer.NewCadenceUpdaterWorker(eventBus, database.Pool, cadenceUpdater))

	// KnowledgeCacheUpdater worker: refreshes the contact.location/birthday/how_met
	// cache columns on assertion.accepted / assertion.superseded events (the bus
	// routes both kinds here; the worker no-ops unless the predicate is one of the
	// three cutover predicates). Covers supersession / closure / retraction from
	// any producer; the inline RefreshTx in ContactService handles direct edits.
	addWorker(reg, consumer.NewKnowledgeCacheUpdaterWorker(eventBus, database.Pool, knowledgeCacheUpdater))

	// FollowUpManager + river workers. Routing is config-blind
	// (events.consumerJobsForKind always enqueues cadence + follow-up
	// jobs for interaction.recorded); HandleEvent short-circuits on
	// mode=off without DB writes. The Todoist create / close / refresh
	// workers are registered so river knows their kinds even when
	// Todoist isn't wired — in that case the settings func returns an
	// ErrNoTodoistAccount-equivalent error and the worker returns a
	// retryable failure for river to back off.
	addWorker(reg, consumer.NewFollowUpManagerWorker(eventBus, database.Pool, followUpManager))
	addWorker(reg, consumer.NewTodoistFollowUpCreateJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoistClientFactory, riverClient, database.Pool,
	))
	addWorker(reg, consumer.NewTodoistFollowUpCloseJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoistClientFactory,
	))
	addWorker(reg, consumer.NewTodoistFollowUpRefreshJobWorker(
		followUpMode, contactTaskRepo, followUpSettings, todoistClientFactory,
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
}

// registerRematchDispatcher registers the rematch-dispatcher consumer worker.
// Always-on (no mode flag): a registered River worker that returned nil in
// kill-switch mode would permanently ack queued jobs, so rollback is
// `git revert` only. Rematch handlers themselves (calendar, telegram) are
// registered elsewhere once their deps are constructed.
func registerRematchDispatcher(reg *riverRegistrar, graph graphCore, database *db.Database, eventBus *events.Bus) {
	rematchService := graph.RematchService

	rematchDispatcher := consumer.NewRematchDispatcher(rematchService)
	addWorker(reg, consumer.NewRematchDispatcherWorker(eventBus, database.Pool, rematchDispatcher))
	logger.Info().Msg("event-bus RematchDispatcher: cutover active")
}
