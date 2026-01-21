package service

import (
	"context"
	"strings"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// NoteService handles note business logic
type NoteService struct {
	noteRepo    *repository.NoteRepository
	contactRepo *repository.ContactRepository
}

// NewNoteService creates a new note service
func NewNoteService(noteRepo *repository.NoteRepository, contactRepo *repository.ContactRepository) *NoteService {
	return &NoteService{
		noteRepo:    noteRepo,
		contactRepo: contactRepo,
	}
}

// GetContactNotepad retrieves the notepad note for a contact
// Returns nil if no notepad note exists (not an error)
func (s *NoteService) GetContactNotepad(ctx context.Context, contactID uuid.UUID) (*repository.Note, error) {
	// Verify the contact exists
	_, err := s.contactRepo.GetContact(ctx, contactID)
	if err != nil {
		return nil, err
	}

	return s.noteRepo.GetContactNotepad(ctx, contactID)
}

// SaveContactNotepad saves the notepad note for a contact
// If body is empty or whitespace-only, deletes the note and returns nil
// Otherwise, creates or updates the note and returns the note
func (s *NoteService) SaveContactNotepad(ctx context.Context, contactID uuid.UUID, body string) (*repository.Note, error) {
	// Verify the contact exists
	_, err := s.contactRepo.GetContact(ctx, contactID)
	if err != nil {
		return nil, err
	}

	// Normalize the body - treat whitespace-only as empty
	trimmedBody := strings.TrimSpace(body)

	// Get existing note
	existingNote, err := s.noteRepo.GetContactNotepad(ctx, contactID)
	if err != nil {
		return nil, err
	}

	// If body is empty, delete the note if it exists
	if trimmedBody == "" {
		if existingNote != nil {
			if err := s.noteRepo.DeleteContactNotepad(ctx, contactID); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	// If note exists, update it
	if existingNote != nil {
		category := string(repository.NoteCategoryNotepad)
		return s.noteRepo.UpdateNote(ctx, existingNote.ID, trimmedBody, &category)
	}

	// Create new note
	return s.noteRepo.CreateNotepad(ctx, contactID, trimmedBody)
}
