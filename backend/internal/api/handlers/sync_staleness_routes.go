package handlers

import "github.com/gin-gonic/gin"

// RegisterSyncStalenessRoutes wires the sync-staleness breach endpoint
// onto a group whose middleware already enforces the global API key:
//
//   - GET /api/v1/sync/staleness
//
// Registered unconditionally, OUTSIDE the EnableExternalSync-gated /sync
// group: heartbeat/push breaches must be visible even with external sync
// off. The static 2-segment path coexists with the sync group's
// 3-segment param routes (e.g. /sync/:source/status).
func RegisterSyncStalenessRoutes(v1 *gin.RouterGroup, handler *StalenessHandler) {
	v1.GET("/sync/staleness", handler.GetActiveBreaches)
}
