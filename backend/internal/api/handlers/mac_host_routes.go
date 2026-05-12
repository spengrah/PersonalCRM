package handlers

import (
	"personal-crm/backend/internal/auth"

	"github.com/gin-gonic/gin"
)

// MacHostRouteDeps bundles the wiring the route-registration helper
// needs. Both main.go and the integration tests construct one of these
// and call RegisterMacHostRoutes — the helper is the single source of
// truth for the daemon-auth route surface (public Pair + host-bearer
// daemon endpoints). Admin routes stay on the caller's v1 group via
// RegisterMacHostAdminRoutes below.
type MacHostRouteDeps struct {
	HostRepo    auth.MacHostKeyValidator
	Handler     *MacHostHandler
	AuthLimiter auth.MacHostAuthLimiterConfig
}

// RegisterMacHostRoutes wires the public + daemon-auth Mac-daemon route
// surface onto the given router:
//
//   - POST /api/v1/host                          (public, IP rate-limited inside handler)
//   - POST /api/v1/host/:id/heartbeat            (host bearer auth)
//   - GET  /api/v1/host/:id/sync/:source/cursor  (host bearer auth)
//   - POST /api/v1/host/:id/sync/:source/cursor  (host bearer auth)
//   - GET  /api/v1/host/:id/sync/:source/known-ids (host bearer auth)
//
// Caller is responsible for adding the admin routes via
// RegisterMacHostAdminRoutes against their own global-API-key-protected
// /api/v1 group; the two layers cannot live in the same registration
// pass because gin route trees reject duplicate registrations on the
// same /api/v1 prefix.
func RegisterMacHostRoutes(router *gin.Engine, deps MacHostRouteDeps) {
	// Public pairing endpoint — registered directly on the router so
	// the daemon's first request bypasses the global API key.
	router.POST("/api/v1/host", deps.Handler.Pair)

	macDaemon := router.Group("/api/v1")
	macDaemon.Use(auth.MacHostAuthMiddleware(deps.HostRepo, auth.DefaultPasswordComparator, deps.AuthLimiter))
	{
		macDaemon.POST("/host/:id/heartbeat", deps.Handler.Heartbeat)
		macDaemon.GET("/host/:id/sync/:source/cursor", deps.Handler.GetCursor)
		macDaemon.POST("/host/:id/sync/:source/cursor", deps.Handler.CommitCursor)
		macDaemon.GET("/host/:id/sync/:source/known-ids", deps.Handler.KnownIDs)
	}
}

// RegisterMacHostAdminRoutes wires the admin Mac-daemon route surface
// onto a group whose middleware already enforces the global API key.
// Caller must pass the same /api/v1 group it has applied
// auth.APIKeyMiddleware on; this helper does NOT add the middleware.
func RegisterMacHostAdminRoutes(v1 *gin.RouterGroup, handler *MacHostHandler) {
	host := v1.Group("/host")
	{
		host.GET("", handler.ListHosts)
		host.GET("/:id", handler.GetHostAdmin)
		host.DELETE("/:id", handler.DeleteHost)
		host.POST("/pairing-token", handler.CreatePairingToken)
	}
}
