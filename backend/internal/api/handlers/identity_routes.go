package handlers

import "github.com/gin-gonic/gin"

// RegisterIdentityRoutes wires the identity-matching route surface onto
// a group whose middleware already enforces the global API key:
//
//   - GET    /api/v1/identities/unmatched
//   - GET    /api/v1/identities/:id
//   - POST   /api/v1/identities/:id/link
//   - POST   /api/v1/identities/:id/unlink
//   - DELETE /api/v1/identities/:id
//   - GET    /api/v1/contacts/:id/identities
//
// The cross-group /contacts/:id/identities route travels with its
// handler's domain; the local v1.Group("/contacts") re-creation is
// behavior-identical (same prefix + middleware chain as the base group).
// Caller gates the whole call on cfg.Features.EnableExternalSync &&
// syncHandler != nil.
func RegisterIdentityRoutes(v1 *gin.RouterGroup, handler *IdentityHandler) {
	identities := v1.Group("/identities")
	{
		identities.GET("/unmatched", handler.ListUnmatchedIdentities)
		identities.GET("/:id", handler.GetIdentity)
		identities.POST("/:id/link", handler.LinkIdentity)
		identities.POST("/:id/unlink", handler.UnlinkIdentity)
		identities.DELETE("/:id", handler.DeleteIdentity)
	}

	// Add identity route to contacts
	contacts := v1.Group("/contacts")
	contacts.GET("/:id/identities", handler.ListIdentitiesForContact)
}
