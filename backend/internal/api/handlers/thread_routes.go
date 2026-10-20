package handlers

import "github.com/gin-gonic/gin"

// RegisterThreadRoutes wires the Threads tab's routes onto a group whose
// middleware already enforces the global API key:
//
//   - GET  /api/v1/threads
//   - GET  /api/v1/threads/:thread_id
//   - GET  /api/v1/review-runs
//   - POST /api/v1/threads/:thread_id/flag-labels
//   - POST /api/v1/threads/:thread_id/clearing-labels
//   - POST /api/v1/threads/:thread_id/close
//   - POST /api/v1/judgments/:id/confirm-nones
func RegisterThreadRoutes(v1 *gin.RouterGroup, handler *ThreadHandler) {
	v1.GET("/threads", handler.ListThreads)
	v1.GET("/threads/:thread_id", handler.GetThread)
	v1.GET("/review-runs", handler.ListReviewRuns)
	v1.POST("/threads/:thread_id/flag-labels", handler.LabelFlag)
	v1.POST("/threads/:thread_id/clearing-labels", handler.LabelClearing)
	v1.POST("/threads/:thread_id/close", handler.CloseThread)
	v1.POST("/judgments/:id/confirm-nones", handler.ConfirmNones)
}
