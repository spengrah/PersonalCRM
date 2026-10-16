package main

import (
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
)

// coreHandlers holds the always-on HTTP handlers consumed by route
// registration.
type coreHandlers struct {
	Contact           *handlers.ContactHandler
	Note              *handlers.NoteHandler
	Interaction       *handlers.InteractionHandler
	Correction        *handlers.InteractionCorrectionHandler
	TimelineAssertion *handlers.TimelineAssertionHandler
	Bench             *handlers.BenchHandler
	System            *handlers.SystemHandler
	Rematch           *handlers.RematchHandler
	ContactMethod     *handlers.ContactMethodHandler
}

// buildCoreHandlers constructs the contact / note / interaction / interaction
// correction / bench / system / rematch handlers. manualHandler is nil in interaction-mode off (the
// InteractionHandler tolerates a nil manual handler exactly as today).
func buildCoreHandlers(
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	cfg *config.Config,
	noteService *service.NoteService,
	manualHandler *service.ManualInteractionHandler,
	eventBus *events.Bus,
	consumers eventConsumers,
	judgments *judgment.Store,
) coreHandlers {
	contactRepo := core.Contact
	interactionRepo := core.Interaction
	rematchService := graph.RematchService

	// Contact methods are mutated by explicit operations rather than by a
	// desired set, so absence in the payload expresses nothing and a client
	// cannot destroy a method it never saw.
	contactMethodService := service.NewContactMethodService(database, eventBus, rematchService)
	contentService := service.NewInteractionContentService(
		interactionRepo,
		repository.NewCommsMessageRepository(database.Queries),
		repository.NewTelegramMessageRepository(database.Queries),
		repository.NewMessagesMessageRepository(database.Queries),
		repository.NewMeetingNoteRepository(database.Queries),
		repository.NewCalendarEventRepository(database.Queries),
		repository.NewPhoneCallRepository(database.Queries),
		contactRepo,
	)

	// The correction routes recompute cadence fields and follow-up tasks
	// through the same consumers every recording path uses.
	correctionService := service.NewInteractionCorrectionService(
		database.Pool,
		interactionRepo,
		graph.GraphNode,
		graph.GraphAssertion,
		contactRepo,
		graph.AssertService,
		consumers.CadenceUpdater,
		consumers.FollowUpManager,
		eventBus,
	)

	return coreHandlers{
		Contact:           handlers.NewContactHandler(contactService),
		Note:              handlers.NewNoteHandler(noteService),
		Interaction:       handlers.NewInteractionHandler(manualHandler, contentService),
		Correction:        handlers.NewInteractionCorrectionHandler(correctionService),
		TimelineAssertion: handlers.NewTimelineAssertionHandler(service.NewTimelineAssertionService(contentService, graph.GraphAssertion)),
		Bench:             handlers.NewBenchHandler(judgment.NewBench(judgments, contentService)),
		System:            handlers.NewSystemHandler(contactRepo, cfg.Runtime),
		Rematch:           handlers.NewRematchHandler(rematchService, contactService),
		ContactMethod:     handlers.NewContactMethodHandler(contactMethodService),
	}
}
