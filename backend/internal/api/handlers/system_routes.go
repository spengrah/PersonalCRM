package handlers

import "github.com/gin-gonic/gin"

// RegisterSystemRoutes wires the system time/acceleration route surface
// onto a group whose middleware already enforces the global API key:
//
//   - GET  /api/v1/system/time
//   - POST /api/v1/system/time/acceleration
//
// The data-exchange routes (/export, /import) are served by the same
// SystemHandler but register through RegisterDataExchangeRoutes so each
// call site keeps its original position in run()'s registration order.
func RegisterSystemRoutes(v1 *gin.RouterGroup, handler *SystemHandler) {
	system := v1.Group("/system")
	{
		system.GET("/time", handler.GetSystemTime)
		system.POST("/time/acceleration", handler.SetTimeAcceleration)
	}
}

// RegisterDataExchangeRoutes wires the full-database export/import route
// surface onto a group whose middleware already enforces the global API
// key:
//
//   - POST /api/v1/export
//   - POST /api/v1/import
func RegisterDataExchangeRoutes(v1 *gin.RouterGroup, handler *SystemHandler) {
	v1.POST("/export", handler.ExportData)
	v1.POST("/import", handler.ImportData)
}
