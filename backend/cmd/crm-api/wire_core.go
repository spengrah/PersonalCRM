package main

import (
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// coreRepos holds the six core-entity repositories every downstream
// service depends on.
type coreRepos struct {
	Contact       *repository.ContactRepository
	ContactMethod *repository.ContactMethodRepository
	Note          *repository.NoteRepository
	Interaction   *repository.InteractionRepository
	ContactTask   *repository.ContactTaskRepository
	Enrichment    *repository.EnrichmentRepository
}

// buildCoreRepos constructs the core-entity repositories.
func buildCoreRepos(queries db.Querier) coreRepos {
	return coreRepos{
		Contact:       repository.NewContactRepository(queries),
		ContactMethod: repository.NewContactMethodRepository(queries),
		Note:          repository.NewNoteRepository(queries),
		Interaction:   repository.NewInteractionRepository(queries),
		ContactTask:   repository.NewContactTaskRepository(queries),
		Enrichment:    repository.NewEnrichmentRepository(queries),
	}
}

// ingestRepos holds the repositories + identity service the ingest path
// needs. The host-auth ingest path (raw_message.* envelopes from the Mac
// daemon) needs a tx-bound identity matcher so the per-event savepoint can
// roll back the identity write atomically with the staging-row insert on
// failure. The external-sync block constructs its own IdentityService for
// the providers that use it — IdentityService is stateless so the two
// instances are interchangeable.
type ingestRepos struct {
	Identity        *repository.IdentityRepository
	IdentityService *service.IdentityService
	MessagesMessage *repository.MessagesMessageRepository
	ExternalContact *repository.ExternalContactRepository
	MacHost         *repository.MacHostRepository
	MeetingNote     *repository.MeetingNoteRepository
	CalendarEvent   *repository.CalendarEventRepository
	PhoneCall       *repository.PhoneCallRepository
}

// buildIngestRepos constructs the repositories + identity service used by
// the IngestService and its inline event handlers. Constructed
// unconditionally because the IngestService is always wired even when
// external sync is disabled — the daemon can still call
// /api/v1/ingest/events on a host-auth path. Several of these instances
// are reused later by the external-sync / mac-host / calendar blocks; the
// repositories are stateless so pointing two instances at the same queries
// is safe.
func buildIngestRepos(queries db.Querier) ingestRepos {
	identityRepo := repository.NewIdentityRepository(queries)
	return ingestRepos{
		Identity:        identityRepo,
		IdentityService: service.NewIdentityService(identityRepo),
		MessagesMessage: repository.NewMessagesMessageRepository(queries),
		ExternalContact: repository.NewExternalContactRepository(queries),
		MacHost:         repository.NewMacHostRepository(queries),
		MeetingNote:     repository.NewMeetingNoteRepository(queries),
		CalendarEvent:   repository.NewCalendarEventRepository(queries),
		PhoneCall:       repository.NewPhoneCallRepository(queries),
	}
}

// graphCore holds the rematch service and the graph (SP1) store built
// around the core repos + event bus. ContactService is NOT built here —
// it depends on the event-bus consumers (cadence / knowledge / follow-up),
// so it is constructed by buildContactService once those exist.
type graphCore struct {
	RematchService *service.RematchService
	GraphNode      *repository.NodeRepository
	GraphAssertion *repository.AssertionRepository
	AssertService  *service.AssertService
}

// buildGraphCore constructs the rematch service (passed as the ContactService
// RematchRegistry constructor arg once ContactService is built) and the graph
// (SP1) write store. Handlers register later once their dependencies are
// constructed.
func buildGraphCore(database *db.Database, eventBus *events.Bus) graphCore {
	// Rematch service — the RematchRegistry ContactService takes as a ctor arg.
	rematchService := service.NewRematchService()

	// Graph (SP1) write API — the single validated assert() write path over the
	// node/entity/predicate/assertion store. SP1 ships the service + the daily
	// rollover worker (registered below); no HTTP handler yet (SP3/SP4 consume it).
	graphNodeRepo := repository.NewNodeRepository(database.Queries)
	graphEntityRepo := repository.NewEntityRepository(database.Queries)
	graphPredicateRepo := repository.NewPredicateRepository(database.Queries)
	graphAssertionRepo := repository.NewAssertionRepository(database.Queries)
	assertService := service.NewAssertService(
		database.Pool, graphNodeRepo, graphEntityRepo, graphPredicateRepo, graphAssertionRepo, eventBus,
	)

	return graphCore{
		RematchService: rematchService,
		GraphNode:      graphNodeRepo,
		GraphAssertion: graphAssertionRepo,
		AssertService:  assertService,
	}
}

// buildContactService constructs the ContactService with its consumer
// dependencies as constructor args (the INV-5 reorder: the cadence /
// knowledge / follow-up consumers and the AssertService are all built before
// the service). cadence + the assertSvc/knowledgeCache pair are the HARD-
// required deps (their nil-guards fire on the cadence/knowledge write paths);
// followUp is nil-tolerant. The merge-time task-close enqueuer is a
// cross-block dependency (the river client is decided here), so it stays a
// setter, called immediately after construction.
func buildContactService(
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	graph graphCore,
	consumers eventConsumers,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) *service.ContactService {
	contactService := service.NewContactService(
		database, core.Contact, core.ContactMethod, core.Interaction, core.ContactTask,
		eventBus, graph.RematchService,
		consumers.CadenceUpdater, graph.AssertService, consumers.KnowledgeCacheUpdater, consumers.FollowUpManager,
	)

	// Wire the merge-time task-close enqueuer. MergeContacts closes the
	// source contact's live automated tasks and enqueues the durable Todoist
	// close job for rows with a real external id; the mode gate matches the
	// close worker's cutover-only contract (enqueuing in mode 'off' would
	// only manufacture failing jobs — merge then closes locally with a WARN,
	// that mode's documented "completion disabled" semantics).
	contactService.SetTaskCloseEnqueuer(riverClient, cfg.EventBus.FollowUpMode == config.EventBusFollowUpModeCutover)

	return contactService
}

// messagingFoundation holds the message-store repositories, the
// source-neutral staging registry, and the venue resolver — the shared
// substrate the interaction recorders and aggregation engines build on.
type messagingFoundation struct {
	TelegramMessageRepo *repository.TelegramMessageRepository
	CommsMessageRepo    *repository.CommsMessageRepository
	StagingRegistry     *repository.StagingProcessorRegistry
	VenueRepo           *repository.VenueRepository
	VenueResolver       *repository.VenueResolverRegistry
}

// buildMessagingFoundation constructs the telegram + comms message repos,
// the staging-processor registry (InteractionRecorder dispatches
// MarkProcessedTx by env.Source to the right repository), and the venue
// resolver (populates interaction.venue_id). messagesMessageRepo and
// calendarRepo are reused from the ingest repos.
func buildMessagingFoundation(queries db.Querier, messagesMessageRepo *repository.MessagesMessageRepository, calendarRepo *repository.CalendarEventRepository) messagingFoundation {
	// Telegram message repo construction (hoisted above the
	// InteractionRecorder wiring so the consumer can mark messages
	// processed in the same tx as the interaction insert).
	telegramMessageRepo := repository.NewTelegramMessageRepository(queries)
	// Comms message repo (shared cross-source content store). Hoisted here
	// so the staging registry below can add the gchat session-scoped
	// processor; also reused by the email-consumer wiring + the gchat
	// aggregation engine further down.
	commsMessageRepo := repository.NewCommsMessageRepository(queries)

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

	// Venue resolution — populates interaction.venue_id (the shared-container
	// node an interaction happened in). The registry routes message.* sources
	// to per-source container readers and resolves gcal via the calendar 3-tuple;
	// the venue repo also serves the email + phone + anarlog recorders directly.
	// Wired into each recorder via its SetVenueResolver setter below.
	venueRepo := repository.NewVenueRepository(queries)
	venueResolver := repository.NewVenueResolverRegistry(
		venueRepo,
		map[string]repository.VenueContainerReader{
			repository.InteractionSourceTelegram: repository.NewTelegramVenueContainerReader(),
			repository.InteractionSourceMessages: repository.NewMessagesVenueContainerReader(),
			repository.InteractionSourceGChat:    repository.NewGChatVenueContainerReader(),
		},
		calendarRepo,
	)

	return messagingFoundation{
		TelegramMessageRepo: telegramMessageRepo,
		CommsMessageRepo:    commsMessageRepo,
		StagingRegistry:     stagingRegistry,
		VenueRepo:           venueRepo,
		VenueResolver:       venueResolver,
	}
}
