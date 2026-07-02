package handlers

import "github.com/gin-gonic/gin"

// RegisterTestRoutes wires the test-only seeding/cleanup route surface
// onto a group whose middleware already enforces the global API key:
//
//   - POST /api/v1/test/seed/contacts
//   - POST /api/v1/test/seed/external-contacts
//   - POST /api/v1/test/seed/method-suggestions
//   - POST /api/v1/test/seed/overdue-contacts
//   - POST /api/v1/test/seed/calendar-events
//   - POST /api/v1/test/seed/mac-hosts
//   - POST /api/v1/test/seed/meeting-notes
//   - POST /api/v1/test/cleanup
//   - POST /api/v1/test/trigger-error
//
// Caller gates the whole call on CRM_ENV in {testing, test}.
func RegisterTestRoutes(v1 *gin.RouterGroup, handler *TestHandler) {
	testRoutes := v1.Group("/test")
	{
		testRoutes.POST("/seed/contacts", handler.SeedContacts)
		testRoutes.POST("/seed/external-contacts", handler.SeedExternalContacts)
		testRoutes.POST("/seed/method-suggestions", handler.SeedMethodSuggestions)
		testRoutes.POST("/seed/overdue-contacts", handler.SeedOverdueContacts)
		testRoutes.POST("/seed/calendar-events", handler.SeedCalendarEvents)
		testRoutes.POST("/seed/mac-hosts", handler.SeedMacHost)
		testRoutes.POST("/seed/meeting-notes", handler.SeedMeetingNotes)
		testRoutes.POST("/cleanup", handler.Cleanup)
		testRoutes.POST("/trigger-error", handler.TriggerError)
	}
}
