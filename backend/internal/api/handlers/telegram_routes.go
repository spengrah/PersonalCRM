package handlers

import "github.com/gin-gonic/gin"

// RegisterTelegramRoutes wires the Telegram auth + chats route surface
// onto a group whose middleware already enforces the global API key:
//
//   - POST   /api/v1/telegram/auth/start
//   - POST   /api/v1/telegram/auth/verify-code
//   - POST   /api/v1/telegram/auth/verify-password
//   - POST   /api/v1/telegram/auth/cancel
//   - DELETE /api/v1/telegram/auth
//   - GET    /api/v1/telegram/auth/status
//   - GET    /api/v1/telegram/chats
//   - PATCH  /api/v1/telegram/chats/:chat_id
//
// Caller gates the whole call on telegramHandler != nil.
func RegisterTelegramRoutes(v1 *gin.RouterGroup, handler *TelegramHandler) {
	tgRoutes := v1.Group("/telegram")
	{
		tgAuth := tgRoutes.Group("/auth")
		{
			tgAuth.POST("/start", handler.StartAuth)
			tgAuth.POST("/verify-code", handler.VerifyCode)
			tgAuth.POST("/verify-password", handler.VerifyPassword)
			tgAuth.POST("/cancel", handler.CancelAuth)
			tgAuth.DELETE("", handler.Disconnect)
			tgAuth.GET("/status", handler.GetStatus)
		}
		tgChats := tgRoutes.Group("/chats")
		{
			tgChats.GET("", handler.ListChats)
			tgChats.PATCH("/:chat_id", handler.UpdateChatStatus)
		}
	}
}
