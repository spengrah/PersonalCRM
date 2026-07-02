package handlers

import "github.com/gin-gonic/gin"

// RegisterContactTaskRoutes wires the manual contact-task route surface
// onto a group whose middleware already enforces the global API key:
//
//   - GET    /api/v1/contacts/:id/tasks
//   - POST   /api/v1/contacts/:id/tasks
//   - DELETE /api/v1/contacts/:id/tasks/:taskId
//
// The local v1.Group("/contacts") re-creation is behavior-identical
// (same prefix + middleware chain as the base group). Caller gates the
// whole call on cfg.Features.EnableExternalSync && syncHandler != nil &&
// contactTaskHandler != nil.
func RegisterContactTaskRoutes(v1 *gin.RouterGroup, handler *ContactTaskHandler) {
	contacts := v1.Group("/contacts")
	contacts.GET("/:id/tasks", handler.ListContactTasks)
	contacts.POST("/:id/tasks", handler.CreateManualTask)
	contacts.DELETE("/:id/tasks/:taskId", handler.DeleteTaskLink)
}
