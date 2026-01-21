package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// NoteCategory defines the category types for notes
type NoteCategory string

const (
	// NoteCategoryNotepad is the category for the single "scratchpad" note per contact
	NoteCategoryNotepad NoteCategory = "notepad"
)

// Note represents a note entity
type Note struct {
	ID        uuid.UUID `json:"id"`
	ContactID uuid.UUID `json:"contact_id"`
	Body      string    `json:"body"`
	Category  *string   `json:"category,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NoteRepository handles note data access
type NoteRepository struct {
	queries db.Querier
}

// NewNoteRepository creates a new note repository
func NewNoteRepository(queries db.Querier) *NoteRepository {
	return &NoteRepository{queries: queries}
}

// convertDbNote converts a sqlc generated note to our domain model
func convertDbNote(dbNote *db.Note) Note {
	note := Note{
		Body: dbNote.Body,
	}

	if dbNote.ID.Valid {
		note.ID = uuid.UUID(dbNote.ID.Bytes)
	}

	if dbNote.ContactID.Valid {
		note.ContactID = uuid.UUID(dbNote.ContactID.Bytes)
	}

	if dbNote.Category.Valid {
		note.Category = &dbNote.Category.String
	}

	if dbNote.CreatedAt.Valid {
		note.CreatedAt = dbNote.CreatedAt.Time
	}

	if dbNote.UpdatedAt.Valid {
		note.UpdatedAt = dbNote.UpdatedAt.Time
	}

	return note
}

// GetContactNotepad retrieves the notepad note for a contact
// Returns nil if no notepad note exists
func (r *NoteRepository) GetContactNotepad(ctx context.Context, contactID uuid.UUID) (*Note, error) {
	category := string(NoteCategoryNotepad)
	dbNote, err := r.queries.GetContactNoteByCategory(ctx, db.GetContactNoteByCategoryParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		Category:  pgtype.Text{String: category, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	note := convertDbNote(dbNote)
	return &note, nil
}

// CreateNotepad creates a new notepad note for a contact
func (r *NoteRepository) CreateNotepad(ctx context.Context, contactID uuid.UUID, body string) (*Note, error) {
	category := string(NoteCategoryNotepad)
	dbNote, err := r.queries.CreateNote(ctx, db.CreateNoteParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		Body:      body,
		Category:  pgtype.Text{String: category, Valid: true},
	})
	if err != nil {
		return nil, err
	}

	note := convertDbNote(dbNote)
	return &note, nil
}

// UpdateNote updates an existing note
func (r *NoteRepository) UpdateNote(ctx context.Context, noteID uuid.UUID, body string, category *string) (*Note, error) {
	var categoryText pgtype.Text
	if category != nil {
		categoryText = pgtype.Text{String: *category, Valid: true}
	}

	dbNote, err := r.queries.UpdateNote(ctx, db.UpdateNoteParams{
		ID:       pgtype.UUID{Bytes: noteID, Valid: true},
		Body:     body,
		Category: categoryText,
	})
	if err != nil {
		return nil, err
	}

	note := convertDbNote(dbNote)
	return &note, nil
}

// DeleteContactNotepad deletes the notepad note for a contact
func (r *NoteRepository) DeleteContactNotepad(ctx context.Context, contactID uuid.UUID) error {
	category := string(NoteCategoryNotepad)
	return r.queries.DeleteContactNoteByCategory(ctx, db.DeleteContactNoteByCategoryParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		Category:  pgtype.Text{String: category, Valid: true},
	})
}

// GetNote retrieves a note by ID
func (r *NoteRepository) GetNote(ctx context.Context, id uuid.UUID) (*Note, error) {
	dbNote, err := r.queries.GetNote(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	note := convertDbNote(dbNote)
	return &note, nil
}

// DeleteNote deletes a note by ID
func (r *NoteRepository) DeleteNote(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteNote(ctx, pgtype.UUID{Bytes: id, Valid: true})
}
