package handlers

import "github.com/gin-gonic/gin"

// RegisterMeetingNoteRoutes wires the user-driven meeting-note
// conflict-resolution endpoint onto a group whose middleware already
// enforces the global API key:
//
//   - POST /api/v1/meeting-notes/:id/resolve-link
//
// This is the frontend-facing resolve endpoint (global API key). The
// daemon-recovery GET /meeting-notes/needs-attention lives on the
// composite-auth ingest surface (see RegisterIngestRoutes) and is not
// registered here.
func RegisterMeetingNoteRoutes(v1 *gin.RouterGroup, handler *MeetingNoteHandler) {
	meetingNotes := v1.Group("/meeting-notes")
	{
		meetingNotes.POST("/:id/resolve-link", handler.ResolveLink)
	}
}
