package handlers

import "github.com/gin-gonic/gin"

// ContactRouteDeps bundles the handlers backing the contact-scoped route
// surface. Contact CRUD, per-contact interactions and notes, and the
// merge preview/apply pair all live under /contacts and are served by
// distinct handlers; the interaction correction routes live under
// /interactions.
type ContactRouteDeps struct {
	Contact       *ContactHandler
	Interaction   *InteractionHandler
	Correction    *InteractionCorrectionHandler
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
//   - POST   /api/v1/contacts/:id/skip
//   - DELETE /api/v1/contacts/:id/skip
//   - GET    /api/v1/contacts/:id/interactions
//   - POST   /api/v1/contacts/:id/interactions
//   - GET    /api/v1/contacts/:id/notes
//   - POST   /api/v1/contacts/:id/methods
//   - PUT    /api/v1/contacts/:id/notes
//   - GET    /api/v1/contacts/:id/merge/preview
//   - POST   /api/v1/contacts/:id/merge
//   - GET    /api/v1/timeline-items/:kind/:id/content
//   - POST   /api/v1/interactions/merge
//   - POST   /api/v1/interactions/merge/preview
//   - POST   /api/v1/interactions/:id/removals
//   - GET    /api/v1/interactions/:id/participants
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
		contacts.POST("/:id/skip", deps.Contact.SkipCycle)
		contacts.DELETE("/:id/skip", deps.Contact.UndoSkip)
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

	v1.GET("/timeline-items/:kind/:id/content", deps.Interaction.GetTimelineItemContent)

	// Interaction routes (non-contact-scoped)
	interactions := v1.Group("/interactions")
	{
		// Corrections (IXN-016, IXN-017, IXN-018): the only HTTP way to merge
		// interactions or remove a participation.
		interactions.POST("/merge", deps.Correction.MergeInteractions)
		interactions.POST("/merge/preview", deps.Correction.PreviewMergeInteractions)
		interactions.POST("/:id/removals", deps.Correction.RemoveParticipations)
		// The attendees a removal can name (IXN-036).
		interactions.GET("/:id/participants", deps.Correction.ListParticipants)
	}
}
