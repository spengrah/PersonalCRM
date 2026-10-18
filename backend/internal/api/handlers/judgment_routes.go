package handlers

import "github.com/gin-gonic/gin"

// RegisterJudgmentRoutes wires the bench's judgment routes onto a group whose
// middleware already enforces the global API key:
//
//   - GET  /api/v1/judgment-runs
//   - GET  /api/v1/judgments
//   - POST /api/v1/judgment-outcomes/:id/review
//   - POST /api/v1/judgments/:id/confirm-nones
func RegisterJudgmentRoutes(v1 *gin.RouterGroup, handler *JudgmentHandler) {
	v1.GET("/judgment-runs", handler.ListJudgmentRuns)
	v1.GET("/judgments", handler.ListJudgments)
	v1.POST("/judgment-outcomes/:id/review", handler.ReviewJudgmentOutcome)
	v1.POST("/judgments/:id/confirm-nones", handler.ConfirmNones)
}
