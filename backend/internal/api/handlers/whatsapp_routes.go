package handlers

import "github.com/gin-gonic/gin"

// RegisterWhatsAppRoutes wires the WhatsApp pairing + status route surface onto
// a group whose middleware already enforces the global API key:
//
//   - POST   /api/v1/whatsapp/auth/start
//   - POST   /api/v1/whatsapp/auth/cancel
//   - DELETE /api/v1/whatsapp/auth
//   - GET    /api/v1/whatsapp/auth/status
//
// Caller gates the whole call on whatsappHandler != nil. When the feature is
// off the handler does not exist, the routes are not registered, and gin's own
// 404 answers — which is exactly what the settings page reads as
// "configuration required".
func RegisterWhatsAppRoutes(v1 *gin.RouterGroup, handler *WhatsAppHandler) {
	waRoutes := v1.Group("/whatsapp")
	{
		waAuth := waRoutes.Group("/auth")
		{
			waAuth.POST("/start", handler.StartPairing)
			waAuth.POST("/cancel", handler.CancelPairing)
			waAuth.DELETE("", handler.Disconnect)
			waAuth.GET("/status", handler.GetStatus)
		}
	}
}
