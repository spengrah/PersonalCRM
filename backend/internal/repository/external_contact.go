package repository

import (
	"context"
	"encoding/json"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MatchStatus represents the match status of an external contact
type MatchStatus string

const (
	MatchStatusMatched   MatchStatus = "matched"
	MatchStatusUnmatched MatchStatus = "unmatched"
	MatchStatusIgnored   MatchStatus = "ignored"
	MatchStatusImported  MatchStatus = "imported"
)

// EmailEntry represents an email in an external contact
type EmailEntry struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// PhoneEntry represents a phone number in an external contact
type PhoneEntry struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// AddressEntry represents an address in an external contact
type AddressEntry struct {
	Formatted string `json:"formatted"`
	Type      string `json:"type,omitempty"`
}

// ExternalContact represents an external contact from Google/iCloud
type ExternalContact struct {
	ID            uuid.UUID      `json:"id"`
	Source        string         `json:"source"`
	SourceID      string         `json:"source_id"`
	AccountID     *string        `json:"account_id,omitempty"`
	DisplayName   *string        `json:"display_name,omitempty"`
	FirstName     *string        `json:"first_name,omitempty"`
	LastName      *string        `json:"last_name,omitempty"`
	Emails        []EmailEntry   `json:"emails"`
	Phones        []PhoneEntry   `json:"phones"`
	Addresses     []AddressEntry `json:"addresses"`
	Organization  *string        `json:"organization,omitempty"`
	JobTitle      *string        `json:"job_title,omitempty"`
	Birthday      *time.Time     `json:"birthday,omitempty"`
	PhotoURL      *string        `json:"photo_url,omitempty"`
	CRMContactID  *uuid.UUID     `json:"crm_contact_id,omitempty"`
	MatchStatus   MatchStatus    `json:"match_status"`
	DuplicateOfID *uuid.UUID     `json:"duplicate_of_id,omitempty"`
	Etag          *string        `json:"etag,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	SyncedAt      *time.Time     `json:"synced_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// UpsertExternalContactRequest holds parameters for creating/updating an external contact
type UpsertExternalContactRequest struct {
	Source       string         `json:"source"`
	SourceID     string         `json:"source_id"`
	AccountID    *string        `json:"account_id,omitempty"`
	DisplayName  *string        `json:"display_name,omitempty"`
	FirstName    *string        `json:"first_name,omitempty"`
	LastName     *string        `json:"last_name,omitempty"`
	Emails       []EmailEntry   `json:"emails,omitempty"`
	Phones       []PhoneEntry   `json:"phones,omitempty"`
	Addresses    []AddressEntry `json:"addresses,omitempty"`
	Organization *string        `json:"organization,omitempty"`
	JobTitle     *string        `json:"job_title,omitempty"`
	Birthday     *time.Time     `json:"birthday,omitempty"`
	PhotoURL     *string        `json:"photo_url,omitempty"`
	Etag         *string        `json:"etag,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SyncedAt     *time.Time     `json:"synced_at,omitempty"`
}

// ExternalContactRepository handles external contact persistence
type ExternalContactRepository struct {
	queries db.Querier
}

// NewExternalContactRepository creates a new external contact repository
func NewExternalContactRepository(queries db.Querier) *ExternalContactRepository {
	return &ExternalContactRepository{queries: queries}
}

// convertDbExternalContact converts a database external contact to a repository model
func convertDbExternalContact(dbContact *db.ExternalContact) (*ExternalContact, error) {
	contact := &ExternalContact{
		Source:      dbContact.Source,
		SourceID:    dbContact.SourceID,
		MatchStatus: MatchStatus(dbContact.MatchStatus.String),
	}

	// Convert UUID
	if dbContact.ID.Valid {
		contact.ID = uuid.UUID(dbContact.ID.Bytes)
	}

	// Convert optional strings
	if dbContact.AccountID.Valid {
		contact.AccountID = &dbContact.AccountID.String
	}
	if dbContact.DisplayName.Valid {
		contact.DisplayName = &dbContact.DisplayName.String
	}
	if dbContact.FirstName.Valid {
		contact.FirstName = &dbContact.FirstName.String
	}
	if dbContact.LastName.Valid {
		contact.LastName = &dbContact.LastName.String
	}
	if dbContact.Organization.Valid {
		contact.Organization = &dbContact.Organization.String
	}
	if dbContact.JobTitle.Valid {
		contact.JobTitle = &dbContact.JobTitle.String
	}
	if dbContact.PhotoUrl.Valid {
		contact.PhotoURL = &dbContact.PhotoUrl.String
	}
	if dbContact.Etag.Valid {
		contact.Etag = &dbContact.Etag.String
	}

	// Convert birthday
	if dbContact.Birthday.Valid {
		t := dbContact.Birthday.Time
		contact.Birthday = &t
	}

	// Convert CRM contact ID
	if dbContact.CrmContactID.Valid {
		id := uuid.UUID(dbContact.CrmContactID.Bytes)
		contact.CRMContactID = &id
	}

	// Convert duplicate of ID
	if dbContact.DuplicateOfID.Valid {
		id := uuid.UUID(dbContact.DuplicateOfID.Bytes)
		contact.DuplicateOfID = &id
	}

	// Parse JSONB fields
	if len(dbContact.Emails) > 0 {
		if err := json.Unmarshal(dbContact.Emails, &contact.Emails); err != nil {
			contact.Emails = []EmailEntry{}
		}
	} else {
		contact.Emails = []EmailEntry{}
	}

	if len(dbContact.Phones) > 0 {
		if err := json.Unmarshal(dbContact.Phones, &contact.Phones); err != nil {
			contact.Phones = []PhoneEntry{}
		}
	} else {
		contact.Phones = []PhoneEntry{}
	}

	if len(dbContact.Addresses) > 0 {
		if err := json.Unmarshal(dbContact.Addresses, &contact.Addresses); err != nil {
			contact.Addresses = []AddressEntry{}
		}
	} else {
		contact.Addresses = []AddressEntry{}
	}

	if len(dbContact.Metadata) > 0 {
		if err := json.Unmarshal(dbContact.Metadata, &contact.Metadata); err != nil {
			contact.Metadata = map[string]any{}
		}
	} else {
		contact.Metadata = map[string]any{}
	}

	// Convert timestamps
	if dbContact.SyncedAt.Valid {
		contact.SyncedAt = &dbContact.SyncedAt.Time
	}
	if dbContact.CreatedAt.Valid {
		contact.CreatedAt = dbContact.CreatedAt.Time
	}
	if dbContact.UpdatedAt.Valid {
		contact.UpdatedAt = dbContact.UpdatedAt.Time
	}

	return contact, nil
}

// GetByID retrieves an external contact by ID
func (r *ExternalContactRepository) GetByID(ctx context.Context, id uuid.UUID) (*ExternalContact, error) {
	dbContact, err := r.queries.GetExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// GetBySource retrieves an external contact by source and source_id
func (r *ExternalContactRepository) GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*ExternalContact, error) {
	var accountIDText pgtype.Text
	if accountID != nil {
		accountIDText = pgtype.Text{String: *accountID, Valid: true}
	}

	dbContact, err := r.queries.GetExternalContactBySource(ctx, db.GetExternalContactBySourceParams{
		Source:    source,
		SourceID:  sourceID,
		AccountID: accountIDText,
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// Upsert creates or updates an external contact
func (r *ExternalContactRepository) Upsert(ctx context.Context, req UpsertExternalContactRequest) (*ExternalContact, error) {
	// Marshal JSONB fields
	emailsJSON, _ := json.Marshal(req.Emails)
	if req.Emails == nil {
		emailsJSON = []byte("[]")
	}
	phonesJSON, _ := json.Marshal(req.Phones)
	if req.Phones == nil {
		phonesJSON = []byte("[]")
	}
	addressesJSON, _ := json.Marshal(req.Addresses)
	if req.Addresses == nil {
		addressesJSON = []byte("[]")
	}
	metadataJSON, _ := json.Marshal(req.Metadata)
	if req.Metadata == nil {
		metadataJSON = []byte("{}")
	}

	params := db.UpsertExternalContactParams{
		Source:      req.Source,
		SourceID:    req.SourceID,
		Emails:      emailsJSON,
		Phones:      phonesJSON,
		Addresses:   addressesJSON,
		Metadata:    metadataJSON,
		MatchStatus: pgtype.Text{String: string(MatchStatusUnmatched), Valid: true},
	}

	if req.AccountID != nil {
		params.AccountID = pgtype.Text{String: *req.AccountID, Valid: true}
	}
	if req.DisplayName != nil {
		params.DisplayName = pgtype.Text{String: *req.DisplayName, Valid: true}
	}
	if req.FirstName != nil {
		params.FirstName = pgtype.Text{String: *req.FirstName, Valid: true}
	}
	if req.LastName != nil {
		params.LastName = pgtype.Text{String: *req.LastName, Valid: true}
	}
	if req.Organization != nil {
		params.Organization = pgtype.Text{String: *req.Organization, Valid: true}
	}
	if req.JobTitle != nil {
		params.JobTitle = pgtype.Text{String: *req.JobTitle, Valid: true}
	}
	if req.PhotoURL != nil {
		params.PhotoUrl = pgtype.Text{String: *req.PhotoURL, Valid: true}
	}
	if req.Etag != nil {
		params.Etag = pgtype.Text{String: *req.Etag, Valid: true}
	}
	if req.Birthday != nil {
		params.Birthday = pgtype.Date{Time: *req.Birthday, Valid: true}
	}
	if req.SyncedAt != nil {
		params.SyncedAt = pgtype.Timestamptz{Time: *req.SyncedAt, Valid: true}
	}

	dbContact, err := r.queries.UpsertExternalContact(ctx, params)
	if err != nil {
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// ListUnmatched returns unmatched external contacts for a source
func (r *ExternalContactRepository) ListUnmatched(ctx context.Context, source string, limit, offset int32) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListUnmatchedExternalContacts(ctx, db.ListUnmatchedExternalContactsParams{
		Source: source,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// ListAllUnmatched returns all unmatched external contacts across sources
func (r *ExternalContactRepository) ListAllUnmatched(ctx context.Context, limit, offset int32) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListAllUnmatchedExternalContacts(ctx, db.ListAllUnmatchedExternalContactsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// CountUnmatched returns the count of unmatched contacts for a source
func (r *ExternalContactRepository) CountUnmatched(ctx context.Context, source string) (int64, error) {
	return r.queries.CountUnmatchedExternalContacts(ctx, source)
}

// CountAllUnmatched returns the count of all unmatched contacts
func (r *ExternalContactRepository) CountAllUnmatched(ctx context.Context) (int64, error) {
	return r.queries.CountAllUnmatchedExternalContacts(ctx)
}

// UpdateMatch updates the CRM contact ID and match status
func (r *ExternalContactRepository) UpdateMatch(ctx context.Context, id uuid.UUID, crmContactID *uuid.UUID, status MatchStatus) (*ExternalContact, error) {
	var crmContactIDPg pgtype.UUID
	if crmContactID != nil {
		crmContactIDPg = pgtype.UUID{Bytes: *crmContactID, Valid: true}
	}

	dbContact, err := r.queries.UpdateExternalContactMatch(ctx, db.UpdateExternalContactMatchParams{
		ID:           pgtype.UUID{Bytes: id, Valid: true},
		CrmContactID: crmContactIDPg,
		MatchStatus:  pgtype.Text{String: string(status), Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// MarkAsDuplicate marks an external contact as a duplicate of another
func (r *ExternalContactRepository) MarkAsDuplicate(ctx context.Context, id, duplicateOfID uuid.UUID) error {
	return r.queries.UpdateExternalContactDuplicate(ctx, db.UpdateExternalContactDuplicateParams{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		DuplicateOfID: pgtype.UUID{Bytes: duplicateOfID, Valid: true},
	})
}

// Ignore marks an external contact as ignored
func (r *ExternalContactRepository) Ignore(ctx context.Context, id uuid.UUID) error {
	return r.queries.IgnoreExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// FindByNormalizedEmail finds external contacts by normalized email
func (r *ExternalContactRepository) FindByNormalizedEmail(ctx context.Context, email string) ([]ExternalContact, error) {
	dbContacts, err := r.queries.FindExternalContactsByNormalizedEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// ListForCRMContact returns external contacts linked to a CRM contact
func (r *ExternalContactRepository) ListForCRMContact(ctx context.Context, crmContactID uuid.UUID) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListExternalContactsForCRMContact(ctx, pgtype.UUID{Bytes: crmContactID, Valid: true})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// Delete removes an external contact
func (r *ExternalContactRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
}
