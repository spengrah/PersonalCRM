package handlers

import "github.com/gin-gonic/gin"

// RegisterTodoistRoutes wires the Todoist settings route surface onto a
// group whose middleware already enforces the global API key:
//
//   - GET   /api/v1/todoist/settings
//   - PATCH /api/v1/todoist/settings
//   - GET   /api/v1/todoist/projects
//   - GET   /api/v1/todoist/labels
//
// Caller gates the whole call on todoistHandler != nil.
func RegisterTodoistRoutes(v1 *gin.RouterGroup, handler *TodoistHandler) {
	todoistRoutes := v1.Group("/todoist")
	{
		todoistRoutes.GET("/settings", handler.GetSettings)
		todoistRoutes.PATCH("/settings", handler.UpdateSettings)
		todoistRoutes.GET("/projects", handler.ListProjects)
		todoistRoutes.GET("/labels", handler.ListLabels)
	}
}
