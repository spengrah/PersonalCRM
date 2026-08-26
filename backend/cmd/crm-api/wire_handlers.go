package main

import (
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
)

// coreHandlers holds the always-on HTTP handlers consumed by route
// registration.
type coreHandlers struct {
	Contact       *handlers.ContactHandler
	Note          *handlers.NoteHandler
	Interaction   *handlers.InteractionHandler
	System        *handlers.SystemHandler
	Rematch       *handlers.RematchHandler
	ContactMethod *handlers.ContactMethodHandler
}

// buildCoreHandlers constructs the contact / note / interaction / system /
// rematch handlers. manualHandler is nil in interaction-mode off (the
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

	return coreHandlers{
		Contact:       handlers.NewContactHandler(contactService),
		Note:          handlers.NewNoteHandler(noteService),
		Interaction:   handlers.NewInteractionHandler(interactionRepo, manualHandler, contentService),
		System:        handlers.NewSystemHandler(contactRepo, cfg.Runtime),
		Rematch:       handlers.NewRematchHandler(rematchService, contactService),
		ContactMethod: handlers.NewContactMethodHandler(contactMethodService),
	}
}
