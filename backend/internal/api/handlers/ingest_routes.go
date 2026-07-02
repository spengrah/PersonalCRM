package handlers

import "github.com/gin-gonic/gin"

// IngestRouteDeps bundles the composite auth middleware and the two
// handlers backing the event-bus ingest surface. Auth is the pre-built
// IngestAuthMiddleware (branches per-request between MacHost bearer and
// global API key); the caller constructs it and passes it in so this
// package stays free of auth-construction concerns.
type IngestRouteDeps struct {
	Auth        gin.HandlerFunc
	Ingest      *IngestHandler
	MeetingNote *MeetingNoteHandler
}

// RegisterIngestRoutes wires the event-bus ingest surface directly onto
// the bare router as SIBLINGS of /api/v1 (not inside it) so the composite
// IngestAuthMiddleware can branch per-request:
//
//   - POST /api/v1/ingest/events               (composite auth)
//   - GET  /api/v1/meeting-notes/needs-attention (composite auth)
//
// gin route trees reject duplicate registration of the same prefix under
// different middleware groups, so the composite dispatch is the minimum
// seam to support both auth paths on one URL. Caller gates the whole call
// on cfg.Features.EnableEventBusIngest.
func RegisterIngestRoutes(router *gin.Engine, deps IngestRouteDeps) {
	ingestGroup := router.Group("/api/v1/ingest")
	ingestGroup.Use(deps.Auth)
	ingestGroup.POST("/events", deps.Ingest.IngestEvents)

	// Daemon recovery endpoint: the Mac daemon polls this on startup to
	// reconcile its local pending-notification table against the Pi's
	// current truth. Lives under composite auth so the daemon's
	// X-Mac-Host-ID + Bearer pair-key path resolves; the frontend
	// (global API key) can also reach it.
	meetingNoteRecoveryGroup := router.Group("/api/v1/meeting-notes")
	meetingNoteRecoveryGroup.Use(deps.Auth)
	meetingNoteRecoveryGroup.GET("/needs-attention", deps.MeetingNote.ListNeedsAttention)
}
