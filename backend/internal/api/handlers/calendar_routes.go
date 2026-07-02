package handlers

import "github.com/gin-gonic/gin"

// RegisterCalendarRoutes wires the calendar-event route surface onto a
// group whose middleware already enforces the global API key:
//
//   - GET /api/v1/contacts/:id/events
//   - GET /api/v1/contacts/:id/events/upcoming
//   - GET /api/v1/events/upcoming
//
// The local v1.Group("/contacts") re-creation is behavior-identical
// (same prefix + middleware chain as the base group). Shared by two call
// sites: the external-sync block (gated on calendarHandler != nil) and
// the test-only fallback that constructs a read-only calendar handler
// when OAuth is unconfigured.
func RegisterCalendarRoutes(v1 *gin.RouterGroup, handler *CalendarHandler) {
	contacts := v1.Group("/contacts")
	contacts.GET("/:id/events", handler.ListEventsForContact)
	contacts.GET("/:id/events/upcoming", handler.ListUpcomingEventsForContact)

	events := v1.Group("/events")
	{
		events.GET("/upcoming", handler.ListUpcomingEvents)
	}
}
