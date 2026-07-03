package main

import (
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/service"
)

// coreHandlers holds the always-on HTTP handlers consumed by route
// registration.
type coreHandlers struct {
	Contact     *handlers.ContactHandler
	Note        *handlers.NoteHandler
	Interaction *handlers.InteractionHandler
	System      *handlers.SystemHandler
	Rematch     *handlers.RematchHandler
}

// buildCoreHandlers constructs the contact / note / interaction / system /
// rematch handlers. manualHandler is nil in interaction-mode off (the
// InteractionHandler tolerates a nil manual handler exactly as today).
func buildCoreHandlers(
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	cfg *config.Config,
	noteService *service.NoteService,
	manualHandler *service.ManualInteractionHandler,
) coreHandlers {
	contactRepo := core.Contact
	interactionRepo := core.Interaction
	rematchService := graph.RematchService

	return coreHandlers{
		Contact:     handlers.NewContactHandler(contactService),
		Note:        handlers.NewNoteHandler(noteService),
		Interaction: handlers.NewInteractionHandler(interactionRepo, manualHandler),
		System:      handlers.NewSystemHandler(contactRepo, cfg.Runtime),
		Rematch:     handlers.NewRematchHandler(rematchService, contactService),
	}
}
