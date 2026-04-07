package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

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
	ID                uuid.UUID       `json:"id"`
	FullName          string          `json:"full_name"`
	Methods           []ContactMethod `json:"methods,omitempty"`
	PrimaryMethod     *ContactMethod  `json:"primary_method,omitempty"`
	Location          *string         `json:"location,omitempty"`
	Birthday          *time.Time      `json:"birthday,omitempty"`
	HowMet            *string         `json:"how_met,omitempty"`
	Cadence           *string         `json:"cadence,omitempty"`
	LastContacted     *time.Time      `json:"last_contacted,omitempty"`
	ContactBy         *time.Time      `json:"contact_by,omitempty"`
	LastInteractionAt *time.Time      `json:"last_interaction_at,omitempty"`
	LastOutreachAt    *time.Time      `json:"last_outreach_at,omitempty"`
	LastResponseAt    *time.Time      `json:"last_response_at,omitempty"`
	ProfilePhoto      *string         `json:"profile_photo,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
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
}

// UpdateContactRequest represents the request to update a contact
type UpdateContactRequest struct {
	FullName     string     `json:"full_name"`
	Location     *string    `json:"location,omitempty"`
	Birthday     *time.Time `json:"birthday,omitempty"`
	HowMet       *string    `json:"how_met,omitempty"`
	Cadence      *string    `json:"cadence,omitempty"`
	ContactBy    *time.Time `json:"contact_by,omitempty"`
	ProfilePhoto *string    `json:"profile_photo,omitempty"`
}

// ListContactsParams represents parameters for listing contacts
type ListContactsParams struct {
	Limit          int32  `json:"limit"`
	Offset         int32  `json:"offset"`
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
}

// SearchContactsParams represents parameters for searching contacts
type SearchContactsParams struct {
	Query          string `json:"query"`
	Limit          int32  `json:"limit"`
	Offset         int32  `json:"offset"`
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
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
		lastContacted := dbContact.LastContacted.Time.UTC()
		contact.LastContacted = &lastContacted
	}
	if dbContact.ProfilePhoto.Valid {
		contact.ProfilePhoto = &dbContact.ProfilePhoto.String
	}
	if dbContact.ContactBy.Valid {
		contactBy := dbContact.ContactBy.Time
		contact.ContactBy = &contactBy
	}
	if dbContact.LastInteractionAt.Valid {
		t := dbContact.LastInteractionAt.Time.UTC()
		contact.LastInteractionAt = &t
	}
	if dbContact.LastOutreachAt.Valid {
		t := dbContact.LastOutreachAt.Time.UTC()
		contact.LastOutreachAt = &t
	}
	if dbContact.LastResponseAt.Valid {
		t := dbContact.LastResponseAt.Time.UTC()
		contact.LastResponseAt = &t
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
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SortField:      params.Sort,
			SortOrder:      params.Order,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	} else {
		dbContacts, err = r.queries.ListContacts(ctx, db.ListContactsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
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
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Query,
			SortField:      params.Sort,
			SortOrder:      params.Order,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	} else {
		dbContacts, err = r.queries.SearchContacts(ctx, db.SearchContactsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Query,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
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

	// Calculate contact_by if cadence is set
	var contactBy pgtype.Date
	if req.Cadence != nil && *req.Cadence != "" {
		if cadenceType, err := cadence.ParseCadence(*req.Cadence); err == nil {
			// Use created_at as base since last_contacted is typically nil for new contacts
			base := createdAt
			if req.LastContacted != nil {
				base = *req.LastContacted
			}
			contactByTime := cadence.CalculateContactBy(base, cadenceType)
			contactBy = pgtype.Date{Time: contactByTime, Valid: true}
		}
	}

	dbContact, err := r.queries.CreateContact(ctx, db.CreateContactParams{
		FullName:      req.FullName,
		Location:      stringToPgText(req.Location),
		Birthday:      timeToPgDate(req.Birthday),
		HowMet:        stringToPgText(req.HowMet),
		Cadence:       stringToPgText(req.Cadence),
		LastContacted: timeToPgTimestamptz(req.LastContacted),
		ProfilePhoto:  stringToPgText(req.ProfilePhoto),
		CreatedAt:     pgtype.Timestamptz{Time: createdAt, Valid: true},
		ContactBy:     contactBy,
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
		ContactBy:    timeToPgDate(req.ContactBy),
	})
	if err != nil {
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// UpdateContactLastContacted updates the last contacted date and contact_by for a contact.
// contactBy should be the newly calculated next due date based on lastContacted + cadence.
func (r *ContactRepository) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time, contactBy *time.Time) error {
	return r.queries.UpdateContactLastContacted(ctx, db.UpdateContactLastContactedParams{
		ID:            uuidToPgUUID(id),
		LastContacted: pgtype.Timestamptz{Time: lastContacted, Valid: true},
		ContactBy:     timeToPgDate(contactBy),
	})
}

// UpdateContactLastContactedIfLater updates last_contacted to the later of the current value or the provided value.
// This prevents last_contacted from moving backward when events are processed out of order.
// The contact_by date is automatically recalculated in SQL using the contact's existing cadence.
func (r *ContactRepository) UpdateContactLastContactedIfLater(ctx context.Context, id uuid.UUID, lastContacted time.Time) error {
	return r.queries.UpdateContactLastContactedIfLater(ctx, db.UpdateContactLastContactedIfLaterParams{
		ID:            uuidToPgUUID(id),
		LastContacted: pgtype.Timestamptz{Time: lastContacted, Valid: true},
	})
}

// UpdateContactBy updates just the contact_by field (used by Todoist sync for deadline changes)
func (r *ContactRepository) UpdateContactBy(ctx context.Context, id uuid.UUID, contactBy time.Time) error {
	return r.queries.UpdateContactBy(ctx, db.UpdateContactByParams{
		ID:        uuidToPgUUID(id),
		ContactBy: timeToPgDate(&contactBy),
	})
}

// UpdateContactOutreachAt updates only last_outreach_at (for outbound interactions)
func (r *ContactRepository) UpdateContactOutreachAt(ctx context.Context, id uuid.UUID, outreachAt time.Time, isManual bool) error {
	return r.queries.UpdateContactOutreachAt(ctx, db.UpdateContactOutreachAtParams{
		ID:         uuidToPgUUID(id),
		OutreachAt: pgtype.Timestamptz{Time: outreachAt, Valid: true},
		IsManual:   isManual,
	})
}

// UpdateContactResponseFields updates last_contacted, last_interaction_at, last_response_at, contact_by (for inbound interactions)
func (r *ContactRepository) UpdateContactResponseFields(ctx context.Context, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return r.queries.UpdateContactResponseFields(ctx, db.UpdateContactResponseFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
	})
}

// UpdateContactMutualFields updates all direction fields + last_contacted + contact_by (for mutual interactions)
func (r *ContactRepository) UpdateContactMutualFields(ctx context.Context, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return r.queries.UpdateContactMutualFields(ctx, db.UpdateContactMutualFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
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
func (r *ContactRepository) CountContacts(ctx context.Context, cadenceFilter string, followupFilter string) (int64, error) {
	return r.queries.CountContacts(ctx, db.CountContactsParams{
		CadenceFilter:  cadenceFilter,
		FollowupFilter: followupFilter,
	})
}

// CountSearchContacts returns the total number of contacts matching a search query.
func (r *ContactRepository) CountSearchContacts(ctx context.Context, query string, cadenceFilter string, followupFilter string) (int64, error) {
	return r.queries.CountSearchContacts(ctx, db.CountSearchContactsParams{
		CadenceFilter:  cadenceFilter,
		FollowupFilter: followupFilter,
		SearchQuery:    query,
	})
}

// ListContactIDsParams represents parameters for listing contact IDs
type ListContactIDsParams struct {
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	Search         string `json:"search,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
}

// ListContactIDs retrieves a list of contact IDs with optional sorting and search.
// This is a lightweight method for navigation purposes.
func (r *ContactRepository) ListContactIDs(ctx context.Context, params ListContactIDsParams) ([]uuid.UUID, error) {
	var (
		dbIDs []pgtype.UUID
		err   error
	)

	hasSearch := params.Search != ""
	hasSort := params.Sort != ""

	switch {
	case hasSearch && hasSort:
		dbIDs, err = r.queries.SearchContactIDsSorted(ctx, db.SearchContactIDsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Search,
			SortField:      params.Sort,
			SortOrder:      params.Order,
		})
	case hasSearch:
		dbIDs, err = r.queries.SearchContactIDs(ctx, db.SearchContactIDsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Search,
		})
	case hasSort:
		dbIDs, err = r.queries.ListContactIDsSorted(ctx, db.ListContactIDsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SortField:      params.Sort,
			SortOrder:      params.Order,
		})
	default:
		dbIDs, err = r.queries.ListContactIDs(ctx, db.ListContactIDsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
		})
	}

	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, len(dbIDs))
	for i, dbID := range dbIDs {
		if dbID.Valid {
			ids[i] = uuid.UUID(dbID.Bytes)
		}
	}

	return ids, nil
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
			if err := json.Unmarshal(row.MethodsJson, &methodData); err != nil {
				logger.Warn().Err(err).Str("contact_id", contactID.String()).Msg("failed to unmarshal contact methods JSON")
			} else {
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

// BatchContactInput represents input for batch matching
type BatchContactInput struct {
	CandidateID   string // External contact ID for result grouping
	CandidateName string // Name to match against
}

// BatchContactMatch represents matches for a single candidate
type BatchContactMatch struct {
	CandidateID string         // External contact ID
	Matches     []ContactMatch // Matching contacts ordered by similarity
}

// FindSimilarContactsBatch finds similar contacts for multiple names in one query.
// Returns matches grouped by candidate ID, preserving order of inputs.
// Candidates with empty names should be filtered out before calling this method.
func (r *ContactRepository) FindSimilarContactsBatch(
	ctx context.Context,
	inputs []BatchContactInput,
	threshold float64,
	limitPerCandidate int32,
) ([]BatchContactMatch, error) {
	if len(inputs) == 0 {
		return []BatchContactMatch{}, nil
	}

	// Build input arrays
	names := make([]string, len(inputs))
	ids := make([]string, len(inputs))
	for i, input := range inputs {
		names[i] = input.CandidateName
		ids[i] = input.CandidateID
	}

	rows, err := r.queries.FindSimilarContactsBatch(ctx, db.FindSimilarContactsBatchParams{
		CandidateNames:    names,
		CandidateIds:      ids,
		Threshold:         float32(threshold),
		LimitPerCandidate: limitPerCandidate,
	})
	if err != nil {
		return nil, err
	}

	// Group results by candidate ID
	resultMap := make(map[string][]ContactMatch)
	for _, row := range rows {
		// Convert UUID
		var contactID uuid.UUID
		if row.ContactID.Valid {
			contactID = uuid.UUID(row.ContactID.Bytes)
		}

		// Parse contact methods from JSON
		var methods []ContactMethod
		if len(row.MethodsJson) > 0 {
			var methodData []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(row.MethodsJson, &methodData); err != nil {
				logger.Warn().Err(err).Str("contact_id", contactID.String()).Str("candidate_id", row.CandidateID).Msg("failed to unmarshal contact methods JSON in batch")
			} else {
				methods = make([]ContactMethod, len(methodData))
				for i, m := range methodData {
					methods[i] = ContactMethod{
						Type:  m.Type,
						Value: m.Value,
					}
				}
			}
		}

		match := ContactMatch{
			Contact: Contact{
				ID:       contactID,
				FullName: row.ContactName,
				Methods:  methods,
			},
			Similarity: float64(row.NameSimilarity),
		}

		resultMap[row.CandidateID] = append(resultMap[row.CandidateID], match)
	}

	// Preserve input order and include empty results for candidates with no matches
	results := make([]BatchContactMatch, len(inputs))
	for i, input := range inputs {
		matches := resultMap[input.CandidateID]
		if matches == nil {
			matches = []ContactMatch{}
		}
		results[i] = BatchContactMatch{
			CandidateID: input.CandidateID,
			Matches:     matches,
		}
	}

	return results, nil
}

// ListOverdueContacts retrieves contacts whose contact_by date is before today.
// The today parameter should be the current date in server timezone (use cadence.Today()).
func (r *ContactRepository) ListOverdueContacts(ctx context.Context, today time.Time, limit int32) ([]Contact, error) {
	dbContacts, err := r.queries.ListOverdueContacts(ctx, db.ListOverdueContactsParams{
		Today:      pgtype.Date{Time: today, Valid: true},
		LimitCount: limit,
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

// ListContactsWithContactBy retrieves contacts that have a contact_by date set.
// Used primarily in testing mode to do in-memory timestamp filtering.
func (r *ContactRepository) ListContactsWithContactBy(ctx context.Context, limit int32) ([]Contact, error) {
	dbContacts, err := r.queries.ListContactsWithContactBy(ctx, limit)
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}
