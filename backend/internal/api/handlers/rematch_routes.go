package handlers

import "github.com/gin-gonic/gin"

// RegisterRematchRoutes wires the rematch job/rescan route surface onto
// a group whose middleware already enforces the global API key:
//
//   - GET  /api/v1/rematch/jobs/:jobID
//   - POST /api/v1/rematch/contacts/:id/rescan
//
// Registered unconditionally; the rematch service no-ops when no
// providers registered a handler (e.g. telegram-disabled deployments
// still get calendar).
func RegisterRematchRoutes(v1 *gin.RouterGroup, handler *RematchHandler) {
	rematchRoutes := v1.Group("/rematch")
	{
		rematchRoutes.GET("/jobs/:jobID", handler.GetJob)
		rematchRoutes.POST("/contacts/:id/rescan", handler.Rescan)
	}
}
