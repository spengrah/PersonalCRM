package handlers

import (
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// NoteHandler handles note-related HTTP requests
type NoteHandler struct {
	noteService *service.NoteService
	validator   *validator.Validate
}

// NewNoteHandler creates a new note handler
func NewNoteHandler(noteService *service.NoteService) *NoteHandler {
	return &NoteHandler{
		noteService: noteService,
		validator:   validator.New(),
	}
}

// NoteResponse represents a note in API responses
type NoteResponse struct {
	ID        string    `json:"id"`
	ContactID string    `json:"contact_id"`
	Body      string    `json:"body"`
	Category  *string   `json:"category,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SaveNoteRequest represents the request body for saving a note
type SaveNoteRequest struct {
	Body string `json:"body" validate:"max=50000"`
}

// noteToResponse converts a repository note to an API response
func noteToResponse(note *repository.Note) NoteResponse {
	return NoteResponse{
		ID:        note.ID.String(),
		ContactID: note.ContactID.String(),
		Body:      note.Body,
		Category:  note.Category,
		CreatedAt: note.CreatedAt,
		UpdatedAt: note.UpdatedAt,
	}
}

// GetContactNotepad retrieves the notepad note for a contact
// @Summary Get contact notepad
// @Description Get the notepad note for a contact
// @Tags notes
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=NoteResponse} "Note retrieved successfully"
// @Success 204 "No note exists for this contact"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid contact ID"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/notes [get]
func (h *NoteHandler) GetContactNotepad(c *gin.Context) {
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	note, err := h.noteService.GetContactNotepad(c.Request.Context(), contactID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to retrieve note")
		return
	}

	if note == nil {
		c.Status(http.StatusNoContent)
		return
	}

	response := noteToResponse(note)
	api.SendSuccess(c, http.StatusOK, response, nil)
}

// SaveContactNotepad saves the notepad note for a contact
// @Summary Save contact notepad
// @Description Create or update the notepad note for a contact. If body is empty or whitespace-only, deletes the note.
// @Tags notes
// @Accept json
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Param note body SaveNoteRequest true "Note content"
// @Success 200 {object} api.APIResponse{data=NoteResponse} "Note saved successfully"
// @Success 204 "Note deleted (empty body)"
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Invalid request"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/notes [put]
func (h *NoteHandler) SaveContactNotepad(c *gin.Context) {
	idStr := c.Param("id")
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	var req SaveNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendValidationError(c, "Validation failed", err.Error())
		return
	}

	note, err := h.noteService.SaveContactNotepad(c.Request.Context(), contactID, req.Body)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to save note")
		return
	}

	if note == nil {
		// Note was deleted (empty body)
		c.Status(http.StatusNoContent)
		return
	}

	response := noteToResponse(note)
	api.SendSuccess(c, http.StatusOK, response, nil)
}
