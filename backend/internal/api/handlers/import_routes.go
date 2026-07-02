package handlers

import "github.com/gin-gonic/gin"

// ImportRouteDeps bundles the import-candidate handler plus the two
// optional discovery handlers whose static routes must register BEFORE
// the /:id param route. AnarlogDiscovery and Suggestions are gated
// internally (nil ⇒ their sub-routes are skipped) because they gate
// segments of the /imports group whose ORDER against /:id is
// load-bearing — keeping the ordering in one function is safer than
// splitting it across call sites.
type ImportRouteDeps struct {
	Import           *ImportHandler
	AnarlogDiscovery *AnarlogDiscoveryHandler
	Suggestions      *SuggestionHandler
}

// RegisterImportRoutes wires the import-candidate route surface onto a
// group whose middleware already enforces the global API key, in EXACT
// registration order (static segments before the /:id param route so
// Gin's tree cannot shadow them):
//
//   - GET  /api/v1/imports/candidates
//   - GET  /api/v1/imports/anarlog-title                   (only if AnarlogDiscovery != nil)
//   - POST /api/v1/imports/anarlog-title/resolve           (only if AnarlogDiscovery != nil)
//   - GET  /api/v1/imports/suggestions                     (only if Suggestions != nil)
//   - POST /api/v1/imports/suggestions/:id/methods/resolve (only if Suggestions != nil)
//   - POST /api/v1/imports/suggestions/:id/methods/dismiss (only if Suggestions != nil)
//   - GET  /api/v1/imports/:id
//   - POST /api/v1/imports/:id/import
//   - POST /api/v1/imports/:id/link
//   - POST /api/v1/imports/:id/ignore
//
// Caller gates the whole call on cfg.Features.EnableExternalSync &&
// syncHandler != nil && importHandler != nil.
func RegisterImportRoutes(v1 *gin.RouterGroup, deps ImportRouteDeps) {
	imports := v1.Group("/imports")
	{
		imports.GET("/candidates", deps.Import.ListImportCandidates)
		// Static anarlog-title discovery routes are declared
		// BEFORE the /:id param route so Gin's tree inserts the
		// static segment first and /imports/anarlog-title cannot
		// be shadowed by the :id match.
		if deps.AnarlogDiscovery != nil {
			imports.GET("/anarlog-title", deps.AnarlogDiscovery.ListAnarlogTitle)
			imports.POST("/anarlog-title/resolve", deps.AnarlogDiscovery.ResolveAnarlogTitle)
		}
		// Static suggestions routes are likewise declared BEFORE
		// the /:id param route so the literal `suggestions`
		// segment is not shadowed by the :id wildcard.
		if deps.Suggestions != nil {
			imports.GET("/suggestions", deps.Suggestions.ListSuggestions)
			imports.POST("/suggestions/:id/methods/resolve", deps.Suggestions.ResolveMethodSuggestions)
			imports.POST("/suggestions/:id/methods/dismiss", deps.Suggestions.DismissMethodSuggestions)
		}
		imports.GET("/:id", deps.Import.GetImportCandidate)
		imports.POST("/:id/import", deps.Import.ImportContact)
		imports.POST("/:id/link", deps.Import.LinkContact)
		imports.POST("/:id/ignore", deps.Import.IgnoreContact)
	}
}
