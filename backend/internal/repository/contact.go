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

type ContactRepository struct {
	queries db.Querier
}

func NewContactRepository(queries db.Querier) *ContactRepository {
	return &ContactRepository{queries: queries}
}

// Contact represents a contact entity
type Contact struct {
	ID            uuid.UUID  `json:"id"`
	FullName      string     `json:"full_name"`
	Email         *string    `json:"email,omitempty"`
	Phone         *string    `json:"phone,omitempty"`
	Location      *string    `json:"location,omitempty"`
	Birthday      *time.Time `json:"birthday,omitempty"`
	HowMet        *string    `json:"how_met,omitempty"`
	Cadence       *string    `json:"cadence,omitempty"`
	LastContacted *time.Time `json:"last_contacted,omitempty"`
	ProfilePhoto  *string    `json:"profile_photo,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// CreateContactRequest represents the request to create a contact
type CreateContactRequest struct {
	FullName      string     `json:"full_name"`
	Email         *string    `json:"email,omitempty"`
	Phone         *string    `json:"phone,omitempty"`
	Location      *string    `json:"location,omitempty"`
	Birthday      *time.Time `json:"birthday,omitempty"`
	HowMet        *string    `json:"how_met,omitempty"`
	Cadence       *string    `json:"cadence,omitempty"`
	LastContacted *time.Time `json:"last_contacted,omitempty"`
	ProfilePhoto  *string    `json:"profile_photo,omitempty"`
}

// UpdateContactRequest represents the request to update a contact
type UpdateContactRequest struct {
	FullName     string     `json:"full_name"`
	Email        *string    `json:"email,omitempty"`
	Phone        *string    `json:"phone,omitempty"`
	Location     *string    `json:"location,omitempty"`
	Birthday     *time.Time `json:"birthday,omitempty"`
	HowMet       *string    `json:"how_met,omitempty"`
	Cadence      *string    `json:"cadence,omitempty"`
	ProfilePhoto *string    `json:"profile_photo,omitempty"`
}

// ListContactsParams represents parameters for listing contacts
type ListContactsParams struct {
	Limit  int32 `json:"limit"`
	Offset int32 `json:"offset"`
}

// SearchContactsParams represents parameters for searching contacts
type SearchContactsParams struct {
	Query  string `json:"query"`
	Limit  int32  `json:"limit"`
	Offset int32  `json:"offset"`
}

// convertDbContact converts a database contact to a repository contact
func convertDbContact(dbContact *db.Contact) Contact {
	contact := Contact{
		FullName: dbContact.FullName,
	}

	// Convert UUID
	if dbContact.ID.Valid {
		contact.ID = uuid.UUID(dbContact.ID.Bytes)
	}

	// Convert timestamps
	if dbContact.CreatedAt.Valid {
		contact.CreatedAt = dbContact.CreatedAt.Time
	}
	if dbContact.UpdatedAt.Valid {
		contact.UpdatedAt = dbContact.UpdatedAt.Time
	}

	// Convert nullable fields
	if dbContact.Email.Valid {
		contact.Email = &dbContact.Email.String
	}
	if dbContact.Phone.Valid {
		contact.Phone = &dbContact.Phone.String
	}
	if dbContact.Location.Valid {
		contact.Location = &dbContact.Location.String
	}
	if dbContact.Birthday.Valid {
		birthday := dbContact.Birthday.Time
		contact.Birthday = &birthday
	}
	if dbContact.HowMet.Valid {
		contact.HowMet = &dbContact.HowMet.String
	}
	if dbContact.Cadence.Valid {
		contact.Cadence = &dbContact.Cadence.String
	}
	if dbContact.LastContacted.Valid {
		lastContacted := dbContact.LastContacted.Time
		contact.LastContacted = &lastContacted
	}
	if dbContact.ProfilePhoto.Valid {
		contact.ProfilePhoto = &dbContact.ProfilePhoto.String
	}

	return contact
}

// Helper functions to convert between types
func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func stringToPgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func timeToPgDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{Valid: false}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

// GetContact retrieves a contact by ID
func (r *ContactRepository) GetContact(ctx context.Context, id uuid.UUID) (*Contact, error) {
	dbContact, err := r.queries.GetContact(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// GetContactByEmail retrieves a contact by email
func (r *ContactRepository) GetContactByEmail(ctx context.Context, email string) (*Contact, error) {
	dbContact, err := r.queries.GetContactByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// ListContacts retrieves a paginated list of contacts
func (r *ContactRepository) ListContacts(ctx context.Context, params ListContactsParams) ([]Contact, error) {
	dbContacts, err := r.queries.ListContacts(ctx, db.ListContactsParams{
		Limit:  params.Limit,
		Offset: params.Offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// SearchContacts searches for contacts by query
func (r *ContactRepository) SearchContacts(ctx context.Context, params SearchContactsParams) ([]Contact, error) {
	dbContacts, err := r.queries.SearchContacts(ctx, db.SearchContactsParams{
		Column1: pgtype.Text{String: params.Query, Valid: true},
		Limit:   params.Limit,
		Offset:  params.Offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// CreateContact creates a new contact
func (r *ContactRepository) CreateContact(ctx context.Context, req CreateContactRequest) (*Contact, error) {
	dbContact, err := r.queries.CreateContact(ctx, db.CreateContactParams{
		FullName:      req.FullName,
		Email:         stringToPgText(req.Email),
		Phone:         stringToPgText(req.Phone),
		Location:      stringToPgText(req.Location),
		Birthday:      timeToPgDate(req.Birthday),
		HowMet:        stringToPgText(req.HowMet),
		Cadence:       stringToPgText(req.Cadence),
		LastContacted: timeToPgDate(req.LastContacted),
		ProfilePhoto:  stringToPgText(req.ProfilePhoto),
	})
	if err != nil {
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// UpdateContact updates an existing contact
func (r *ContactRepository) UpdateContact(ctx context.Context, id uuid.UUID, req UpdateContactRequest) (*Contact, error) {
	dbContact, err := r.queries.UpdateContact(ctx, db.UpdateContactParams{
		ID:           uuidToPgUUID(id),
		FullName:     req.FullName,
		Email:        stringToPgText(req.Email),
		Phone:        stringToPgText(req.Phone),
		Location:     stringToPgText(req.Location),
		Birthday:     timeToPgDate(req.Birthday),
		HowMet:       stringToPgText(req.HowMet),
		Cadence:      stringToPgText(req.Cadence),
		ProfilePhoto: stringToPgText(req.ProfilePhoto),
	})
	if err != nil {
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// UpdateContactLastContacted updates the last contacted date for a contact
func (r *ContactRepository) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time) error {
	return r.queries.UpdateContactLastContacted(ctx, db.UpdateContactLastContactedParams{
		ID:            uuidToPgUUID(id),
		LastContacted: pgtype.Date{Time: lastContacted, Valid: true},
	})
}

// SoftDeleteContact soft deletes a contact
func (r *ContactRepository) SoftDeleteContact(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteContact(ctx, uuidToPgUUID(id))
}

// HardDeleteContact permanently deletes a contact
func (r *ContactRepository) HardDeleteContact(ctx context.Context, id uuid.UUID) error {
	return r.queries.HardDeleteContact(ctx, uuidToPgUUID(id))
}

// CountContacts returns the total number of active contacts
func (r *ContactRepository) CountContacts(ctx context.Context) (int64, error) {
	return r.queries.CountContacts(ctx)
}
