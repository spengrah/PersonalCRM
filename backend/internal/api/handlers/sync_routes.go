package handlers

import "github.com/gin-gonic/gin"

// RegisterSyncRoutes wires the external-sync status/trigger route surface
// onto a group whose middleware already enforces the global API key:
//
//   - GET   /api/v1/sync/status
//   - GET   /api/v1/sync/providers
//   - GET   /api/v1/sync/logs
//   - GET   /api/v1/sync/:source/status
//   - POST  /api/v1/sync/:source/trigger
//   - PATCH /api/v1/sync/states/:id/enable
//   - GET   /api/v1/sync/states/:id/logs
//
// Caller gates the whole call on cfg.Features.EnableExternalSync &&
// syncHandler != nil.
func RegisterSyncRoutes(v1 *gin.RouterGroup, handler *SyncHandler) {
	syncRoutes := v1.Group("/sync")
	{
		syncRoutes.GET("/status", handler.GetSyncStatus)
		syncRoutes.GET("/providers", handler.GetAvailableProviders)
		syncRoutes.GET("/logs", handler.GetRecentSyncLogs)
		// Source-based routes (by source name like "gmail", "calendar")
		syncRoutes.GET("/:source/status", handler.GetSyncState)
		syncRoutes.POST("/:source/trigger", handler.TriggerSync)
		// State-based routes (by sync state UUID)
		syncRoutes.PATCH("/states/:id/enable", handler.EnableSync)
		syncRoutes.GET("/states/:id/logs", handler.GetSyncLogs)
	}
}
