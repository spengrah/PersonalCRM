package handlers

import "github.com/gin-gonic/gin"

// RegisterTestRoutes wires the test-only seeding/cleanup route surface
// onto a group whose middleware already enforces the global API key:
//
//   - POST /api/v1/test/seed/declared   (declared seeding — see test_declared.go)
//   - POST /api/v1/test/cleanup         (dual-shape: prefix | namespaces)
//   - POST /api/v1/test/lock
//   - POST /api/v1/test/lock/:lease/renew
//   - DELETE /api/v1/test/lock/:lease
//
// Caller gates the whole call on CRM_ENV in {testing, test}.
func RegisterTestRoutes(v1 *gin.RouterGroup, handler *TestHandler) {
	testRoutes := v1.Group("/test")
	{
		// Declared seeding drives internal/synthetic/declare directly — the
		// documented layering exception, see the TestHandler doc comment.
		testRoutes.POST("/seed/declared", handler.SeedDeclared)
		testRoutes.POST("/cleanup", handler.Cleanup)
		testRoutes.POST("/lock", handler.AcquireLock)
		testRoutes.POST("/lock/:lease/renew", handler.RenewLock)
		testRoutes.DELETE("/lock/:lease", handler.ReleaseLock)
	}
}
