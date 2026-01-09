package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/accelerated"
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
	ID            uuid.UUID       `json:"id"`
	FullName      string          `json:"full_name"`
	Methods       []ContactMethod `json:"methods,omitempty"`
	PrimaryMethod *ContactMethod  `json:"primary_method,omitempty"`
	Location      *string         `json:"location,omitempty"`
	Birthday      *time.Time      `json:"birthday,omitempty"`
	HowMet        *string         `json:"how_met,omitempty"`
	Cadence       *string         `json:"cadence,omitempty"`
	LastContacted *time.Time      `json:"last_contacted,omitempty"`
	ProfilePhoto  *string         `json:"profile_photo,omitempty"`
	Notes         *string         `json:"notes,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// CreateContactRequest represents the request to create a contact
type CreateContactRequest struct {
	FullName      string     `json:"full_name"`
	Location      *string    `json:"location,omitempty"`
	Birthday      *time.Time `json:"birthday,omitempty"`
	HowMet        *string    `json:"how_met,omitempty"`
	Cadence       *string    `json:"cadence,omitempty"`
	LastContacted *time.Time `json:"last_contacted,omitempty"`
	ProfilePhoto  *string    `json:"profile_photo,omitempty"`
	Notes         *string    `json:"notes,omitempty"`
}

// UpdateContactRequest represents the request to update a contact
type UpdateContactRequest struct {
	FullName     string     `json:"full_name"`
	Location     *string    `json:"location,omitempty"`
	Birthday     *time.Time `json:"birthday,omitempty"`
	HowMet       *string    `json:"how_met,omitempty"`
	Cadence      *string    `json:"cadence,omitempty"`
	ProfilePhoto *string    `json:"profile_photo,omitempty"`
	Notes        *string    `json:"notes,omitempty"`
}

// ListContactsParams represents parameters for listing contacts
type ListContactsParams struct {
	Limit  int32  `json:"limit"`
	Offset int32  `json:"offset"`
	Sort   string `json:"sort,omitempty"`
	Order  string `json:"order,omitempty"`
}

// SearchContactsParams represents parameters for searching contacts
type SearchContactsParams struct {
	Query  string `json:"query"`
	Limit  int32  `json:"limit"`
	Offset int32  `json:"offset"`
	Sort   string `json:"sort,omitempty"`
	Order  string `json:"order,omitempty"`
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
	if dbContact.Notes.Valid {
		contact.Notes = &dbContact.Notes.String
	}

	return contact
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

// ListContacts retrieves a paginated list of contacts
func (r *ContactRepository) ListContacts(ctx context.Context, params ListContactsParams) ([]Contact, error) {
	var (
		dbContacts []*db.Contact
		err        error
	)

	if params.Sort != "" {
		dbContacts, err = r.queries.ListContactsSorted(ctx, db.ListContactsSortedParams{
			SortField:  params.Sort,
			SortOrder:  params.Order,
			PageOffset: params.Offset,
			PageLimit:  params.Limit,
		})
	} else {
		dbContacts, err = r.queries.ListContacts(ctx, db.ListContactsParams{
			Limit:  params.Limit,
			Offset: params.Offset,
		})
	}
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
	var (
		dbContacts []*db.Contact
		err        error
	)

	if params.Sort != "" {
		dbContacts, err = r.queries.SearchContactsSorted(ctx, db.SearchContactsSortedParams{
			SearchQuery: params.Query,
			SortField:   params.Sort,
			SortOrder:   params.Order,
			PageOffset:  params.Offset,
			PageLimit:   params.Limit,
		})
	} else {
		dbContacts, err = r.queries.SearchContacts(ctx, db.SearchContactsParams{
			PlaintoTsquery: params.Query,
			Limit:          params.Limit,
			Offset:         params.Offset,
		})
	}
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
	// Use accelerated time for created_at to ensure consistency with time acceleration
	createdAt := accelerated.GetCurrentTime()

	dbContact, err := r.queries.CreateContact(ctx, db.CreateContactParams{
		FullName:      req.FullName,
		Location:      stringToPgText(req.Location),
		Birthday:      timeToPgDate(req.Birthday),
		HowMet:        stringToPgText(req.HowMet),
		Cadence:       stringToPgText(req.Cadence),
		LastContacted: timeToPgTimestamptz(req.LastContacted),
		ProfilePhoto:  stringToPgText(req.ProfilePhoto),
		Notes:         stringToPgText(req.Notes),
		CreatedAt:     pgtype.Timestamptz{Time: createdAt, Valid: true},
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
		Location:     stringToPgText(req.Location),
		Birthday:     timeToPgDate(req.Birthday),
		HowMet:       stringToPgText(req.HowMet),
		Cadence:      stringToPgText(req.Cadence),
		ProfilePhoto: stringToPgText(req.ProfilePhoto),
		Notes:        stringToPgText(req.Notes),
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
		LastContacted: pgtype.Timestamptz{Time: lastContacted, Valid: true},
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

// CountSearchContacts returns the total number of contacts matching a search query.
func (r *ContactRepository) CountSearchContacts(ctx context.Context, query string) (int64, error) {
	return r.queries.CountSearchContacts(ctx, query)
}

// ContactMatch represents a potential contact match with similarity score
type ContactMatch struct {
	Contact    Contact
	Similarity float64
}

// FindSimilarContacts finds contacts with similar names using fuzzy matching
// Returns contacts with similarity above the threshold, ordered by similarity (highest first)
func (r *ContactRepository) FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]ContactMatch, error) {
	rows, err := r.queries.FindSimilarContacts(ctx, db.FindSimilarContactsParams{
		SearchName:  name,
		Threshold:   float32(threshold),
		ResultLimit: limit,
	})
	if err != nil {
		return nil, err
	}

	matches := make([]ContactMatch, 0, len(rows))
	for _, row := range rows {
		// Convert UUID
		var contactID uuid.UUID
		if row.ID.Valid {
			contactID = uuid.UUID(row.ID.Bytes)
		}

		// Parse contact methods from JSON
		var methods []ContactMethod
		if len(row.MethodsJson) > 0 {
			// Unmarshal JSON into temporary struct
			var methodData []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(row.MethodsJson, &methodData); err == nil {
				methods = make([]ContactMethod, len(methodData))
				for i, m := range methodData {
					methods[i] = ContactMethod{
						Type:  m.Type,
						Value: m.Value,
					}
				}
			}
		}

		matches = append(matches, ContactMatch{
			Contact: Contact{
				ID:       contactID,
				FullName: row.FullName,
				Methods:  methods,
			},
			Similarity: float64(row.NameSimilarity),
		})
	}

	return matches, nil
}
