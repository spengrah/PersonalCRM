package main

import (
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
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

// contactCore holds the rematch service, contact service, and graph
// (SP1) store built around the core repos + event bus.
type contactCore struct {
	RematchService *service.RematchService
	ContactService *service.ContactService
	GraphNode      *repository.NodeRepository
	GraphEntity    *repository.EntityRepository
	GraphPredicate *repository.PredicateRepository
	GraphAssertion *repository.AssertionRepository
	AssertService  *service.AssertService
}

// buildContactGraphCore constructs the rematch service (above
// ContactService so it can be passed as the RematchRegistry constructor
// arg), the ContactService, and the graph (SP1) write store. Handlers
// register later once their dependencies are constructed.
func buildContactGraphCore(database *db.Database, repos coreRepos, eventBus *events.Bus) contactCore {
	// Rematch service — constructed above ContactService so it can be
	// passed as the RematchRegistry constructor arg.
	rematchService := service.NewRematchService()

	contactService := service.NewContactService(database, repos.Contact, repos.ContactMethod, repos.Interaction, repos.ContactTask, eventBus, rematchService)

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

	return contactCore{
		RematchService: rematchService,
		ContactService: contactService,
		GraphNode:      graphNodeRepo,
		GraphEntity:    graphEntityRepo,
		GraphPredicate: graphPredicateRepo,
		GraphAssertion: graphAssertionRepo,
		AssertService:  assertService,
	}
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
