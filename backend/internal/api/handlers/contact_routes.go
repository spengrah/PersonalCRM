package handlers

import "github.com/gin-gonic/gin"

// ContactRouteDeps bundles the handlers backing the contact-scoped route
// surface. Contact CRUD, per-contact interactions and notes, and the
// merge preview/apply pair all live under /contacts and are served by
// three distinct handlers, so the deps struct carries all three.
type ContactRouteDeps struct {
	Contact       *ContactHandler
	Interaction   *InteractionHandler
	Note          *NoteHandler
	ContactMethod *ContactMethodHandler
}

// RegisterContactRoutes wires the unconditional contact + interaction
// route surface onto a group whose middleware already enforces the
// global API key:
//
//   - POST   /api/v1/contacts
//   - GET    /api/v1/contacts/overdue
//   - GET    /api/v1/contacts
//   - GET    /api/v1/contacts/:id
//   - PUT    /api/v1/contacts/:id
//   - DELETE /api/v1/contacts/:id
//   - GET    /api/v1/contacts/:id/interactions
//   - POST   /api/v1/contacts/:id/interactions
//   - GET    /api/v1/contacts/:id/notes
//   - POST   /api/v1/contacts/:id/methods
//   - PUT    /api/v1/contacts/:id/notes
//   - GET    /api/v1/contacts/:id/merge/preview
//   - POST   /api/v1/contacts/:id/merge
//   - DELETE /api/v1/interactions/:id
//   - GET    /api/v1/interactions/:id/content
//
// Caller must pass the same /api/v1 group it has applied
// auth.APIKeyMiddleware on; this helper does NOT add the middleware.
func RegisterContactRoutes(v1 *gin.RouterGroup, deps ContactRouteDeps) {
	contacts := v1.Group("/contacts")
	{
		contacts.POST("", deps.Contact.CreateContact)
		contacts.GET("/overdue", deps.Contact.ListOverdueContacts)
		contacts.GET("", deps.Contact.ListContacts)
		contacts.GET("/:id", deps.Contact.GetContact)
		contacts.PUT("/:id", deps.Contact.UpdateContact)
		contacts.DELETE("/:id", deps.Contact.DeleteContact)
		contacts.GET("/:id/interactions", deps.Interaction.ListContactInteractions)
		contacts.POST("/:id/interactions", deps.Interaction.CreateInteraction)
		// POST with OPERATIONS, deliberately never PUT with a desired set: a
		// PUT taking the full list is wholesale replace wearing a sub-resource
		// costume, where absence would again imply deletion.
		contacts.POST("/:id/methods", deps.ContactMethod.ApplyOperations)
		contacts.GET("/:id/notes", deps.Note.GetContactNotepad)
		contacts.PUT("/:id/notes", deps.Note.SaveContactNotepad)
		// Merge routes
		contacts.GET("/:id/merge/preview", deps.Contact.GetMergePreview)
		contacts.POST("/:id/merge", deps.Contact.MergeContacts)
	}

	// Interaction routes (non-contact-scoped)
	interactions := v1.Group("/interactions")
	{
		interactions.GET("/:id/content", deps.Interaction.GetInteractionContent)
		interactions.DELETE("/:id", deps.Interaction.DeleteInteraction)
	}
}
