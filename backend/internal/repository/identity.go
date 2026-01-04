package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/identity"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MatchType represents how an identity was matched to a contact
type MatchType string

const (
	MatchTypeExact     MatchType = "exact"
	MatchTypeFuzzy     MatchType = "fuzzy"
	MatchTypeManual    MatchType = "manual"
	MatchTypeUnmatched MatchType = "unmatched"
)

// ExternalIdentity represents an external identifier matched to a CRM contact
type ExternalIdentity struct {
	ID              uuid.UUID               `json:"id"`
	Identifier      string                  `json:"identifier"`
	IdentifierType  identity.IdentifierType `json:"identifier_type"`
	RawIdentifier   *string                 `json:"raw_identifier,omitempty"`
	Source          string                  `json:"source"`
	SourceID        *string                 `json:"source_id,omitempty"`
	ContactID       *uuid.UUID              `json:"contact_id,omitempty"`
	MatchType       MatchType               `json:"match_type"`
	MatchConfidence *float64                `json:"match_confidence,omitempty"`
	DisplayName     *string                 `json:"display_name,omitempty"`
	LastSeenAt      *time.Time              `json:"last_seen_at,omitempty"`
	MessageCount    int32                   `json:"message_count"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
}

// ContactMethodMatch represents a contact method that matched an external identifier
type ContactMethodMatch struct {
	ID          uuid.UUID `json:"id"`
	ContactID   uuid.UUID `json:"contact_id"`
	ContactName string    `json:"contact_name"`
	Type        string    `json:"type"`
	Value       string    `json:"value"`
	IsPrimary   bool      `json:"is_primary"`
}

// UpsertIdentityRequest holds parameters for creating/updating an identity
type UpsertIdentityRequest struct {
	Identifier      string
	IdentifierType  identity.IdentifierType
	RawIdentifier   *string
	Source          string
	SourceID        *string
	ContactID       *uuid.UUID
	MatchType       MatchType
	MatchConfidence *float64
	DisplayName     *string
	LastSeenAt      *time.Time
	MessageCount    int32
}

// LinkIdentityRequest holds parameters for linking an identity to a contact
type LinkIdentityRequest struct {
	IdentityID      uuid.UUID
	ContactID       uuid.UUID
	MatchType       MatchType
	MatchConfidence *float64
}

// IdentityRepository handles external identity persistence
type IdentityRepository struct {
	queries db.Querier
}

// NewIdentityRepository creates a new identity repository
func NewIdentityRepository(queries db.Querier) *IdentityRepository {
	return &IdentityRepository{queries: queries}
}

// convertDbIdentity converts a database identity to a repository identity
func convertDbIdentity(dbIdentity *db.ExternalIdentity) ExternalIdentity {
	ident := ExternalIdentity{
		Identifier:     dbIdentity.Identifier,
		IdentifierType: identity.IdentifierType(dbIdentity.IdentifierType),
		Source:         dbIdentity.Source,
		MessageCount:   dbIdentity.MessageCount.Int32,
	}

	// Convert UUID
	if dbIdentity.ID.Valid {
		ident.ID = uuid.UUID(dbIdentity.ID.Bytes)
	}

	// Convert timestamps
	if dbIdentity.CreatedAt.Valid {
		ident.CreatedAt = dbIdentity.CreatedAt.Time
	}
	if dbIdentity.UpdatedAt.Valid {
		ident.UpdatedAt = dbIdentity.UpdatedAt.Time
	}
	if dbIdentity.LastSeenAt.Valid {
		ident.LastSeenAt = &dbIdentity.LastSeenAt.Time
	}

	// Convert nullable fields
	if dbIdentity.RawIdentifier.Valid {
		ident.RawIdentifier = &dbIdentity.RawIdentifier.String
	}
	if dbIdentity.SourceID.Valid {
		ident.SourceID = &dbIdentity.SourceID.String
	}
	if dbIdentity.ContactID.Valid {
		contactID := uuid.UUID(dbIdentity.ContactID.Bytes)
		ident.ContactID = &contactID
	}
	if dbIdentity.MatchType.Valid {
		ident.MatchType = MatchType(dbIdentity.MatchType.String)
	}
	if dbIdentity.MatchConfidence.Valid {
		ident.MatchConfidence = &dbIdentity.MatchConfidence.Float64
	}
	if dbIdentity.DisplayName.Valid {
		ident.DisplayName = &dbIdentity.DisplayName.String
	}

	return ident
}

// convertDbContactMethodMatch converts a database contact method match to repository type
func convertDbContactMethodMatch(row *db.FindMethodsByNormalizedValueRow) ContactMethodMatch {
	match := ContactMethodMatch{
		Type:        row.Type,
		Value:       row.Value,
		ContactName: row.ContactName,
	}

	if row.ID.Valid {
		match.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.ContactID.Valid {
		match.ContactID = uuid.UUID(row.ContactID.Bytes)
	}
	if row.IsPrimary.Valid {
		match.IsPrimary = row.IsPrimary.Bool
	}

	return match
}

// GetByID retrieves an identity by ID
func (r *IdentityRepository) GetByID(ctx context.Context, id uuid.UUID) (*ExternalIdentity, error) {
	dbIdentity, err := r.queries.GetIdentityByID(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}

// GetByIdentifier retrieves an identity by identifier, type, and source
func (r *IdentityRepository) GetByIdentifier(ctx context.Context, idType identity.IdentifierType, identifier, source string) (*ExternalIdentity, error) {
	dbIdentity, err := r.queries.GetIdentityByIdentifier(ctx, db.GetIdentityByIdentifierParams{
		IdentifierType: string(idType),
		Identifier:     identifier,
		Source:         source,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}

// FindByIdentifier finds all identities matching the given identifier and type (across sources)
func (r *IdentityRepository) FindByIdentifier(ctx context.Context, idType identity.IdentifierType, identifier string) ([]ExternalIdentity, error) {
	dbIdentities, err := r.queries.FindIdentitiesByIdentifier(ctx, db.FindIdentitiesByIdentifierParams{
		IdentifierType: string(idType),
		Identifier:     identifier,
	})
	if err != nil {
		return nil, err
	}

	identities := make([]ExternalIdentity, len(dbIdentities))
	for i, dbIdentity := range dbIdentities {
		identities[i] = convertDbIdentity(dbIdentity)
	}

	return identities, nil
}

// Upsert creates or updates an identity
func (r *IdentityRepository) Upsert(ctx context.Context, req UpsertIdentityRequest) (*ExternalIdentity, error) {
	params := db.UpsertIdentityParams{
		Identifier:     req.Identifier,
		IdentifierType: string(req.IdentifierType),
		Source:         req.Source,
	}

	// Set optional fields
	if req.RawIdentifier != nil {
		params.RawIdentifier = pgtype.Text{String: *req.RawIdentifier, Valid: true}
	}
	if req.SourceID != nil {
		params.SourceID = pgtype.Text{String: *req.SourceID, Valid: true}
	}
	if req.ContactID != nil {
		params.ContactID = uuidToPgUUID(*req.ContactID)
	}
	if req.MatchType != "" {
		params.MatchType = pgtype.Text{String: string(req.MatchType), Valid: true}
	}
	if req.MatchConfidence != nil {
		params.MatchConfidence = pgtype.Float8{Float64: *req.MatchConfidence, Valid: true}
	}
	if req.DisplayName != nil {
		params.DisplayName = pgtype.Text{String: *req.DisplayName, Valid: true}
	}
	if req.LastSeenAt != nil {
		params.LastSeenAt = pgtype.Timestamptz{Time: *req.LastSeenAt, Valid: true}
	}
	if req.MessageCount > 0 {
		params.MessageCount = pgtype.Int4{Int32: req.MessageCount, Valid: true}
	}

	dbIdentity, err := r.queries.UpsertIdentity(ctx, params)
	if err != nil {
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}

// LinkToContact links an identity to a contact
func (r *IdentityRepository) LinkToContact(ctx context.Context, req LinkIdentityRequest) (*ExternalIdentity, error) {
	params := db.LinkIdentityToContactParams{
		ID:        uuidToPgUUID(req.IdentityID),
		ContactID: uuidToPgUUID(req.ContactID),
		MatchType: pgtype.Text{String: string(req.MatchType), Valid: true},
	}

	if req.MatchConfidence != nil {
		params.MatchConfidence = pgtype.Float8{Float64: *req.MatchConfidence, Valid: true}
	}

	dbIdentity, err := r.queries.LinkIdentityToContact(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}

// UnlinkFromContact unlinks an identity from its contact
func (r *IdentityRepository) UnlinkFromContact(ctx context.Context, id uuid.UUID) (*ExternalIdentity, error) {
	dbIdentity, err := r.queries.UnlinkIdentityFromContact(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}

// ListUnmatched lists unmatched identities with pagination
func (r *IdentityRepository) ListUnmatched(ctx context.Context, limit, offset int32) ([]ExternalIdentity, error) {
	dbIdentities, err := r.queries.ListUnmatchedIdentities(ctx, db.ListUnmatchedIdentitiesParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	identities := make([]ExternalIdentity, len(dbIdentities))
	for i, dbIdentity := range dbIdentities {
		identities[i] = convertDbIdentity(dbIdentity)
	}

	return identities, nil
}

// CountUnmatched returns the count of unmatched identities
func (r *IdentityRepository) CountUnmatched(ctx context.Context) (int64, error) {
	return r.queries.CountUnmatchedIdentities(ctx)
}

// ListForContact lists all identities for a contact
func (r *IdentityRepository) ListForContact(ctx context.Context, contactID uuid.UUID) ([]ExternalIdentity, error) {
	dbIdentities, err := r.queries.ListIdentitiesForContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		return nil, err
	}

	identities := make([]ExternalIdentity, len(dbIdentities))
	for i, dbIdentity := range dbIdentities {
		identities[i] = convertDbIdentity(dbIdentity)
	}

	return identities, nil
}

// ListBySource lists identities for a source with pagination
func (r *IdentityRepository) ListBySource(ctx context.Context, source string, limit, offset int32) ([]ExternalIdentity, error) {
	dbIdentities, err := r.queries.ListIdentitiesBySource(ctx, db.ListIdentitiesBySourceParams{
		Source: source,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	identities := make([]ExternalIdentity, len(dbIdentities))
	for i, dbIdentity := range dbIdentities {
		identities[i] = convertDbIdentity(dbIdentity)
	}

	return identities, nil
}

// CountBySource returns the count of identities for a source
func (r *IdentityRepository) CountBySource(ctx context.Context, source string) (int64, error) {
	return r.queries.CountIdentitiesBySource(ctx, source)
}

// Delete removes an identity
func (r *IdentityRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteIdentity(ctx, uuidToPgUUID(id))
}

// DeleteForContact removes all identities for a contact
func (r *IdentityRepository) DeleteForContact(ctx context.Context, contactID uuid.UUID) error {
	return r.queries.DeleteIdentitiesForContact(ctx, uuidToPgUUID(contactID))
}

// FindContactMethodsByValue finds contact methods matching the given value and types
func (r *IdentityRepository) FindContactMethodsByValue(ctx context.Context, types []string, normalizedValue string) ([]ContactMethodMatch, error) {
	rows, err := r.queries.FindMethodsByNormalizedValue(ctx, db.FindMethodsByNormalizedValueParams{
		Column1: types,
		Value:   normalizedValue,
	})
	if err != nil {
		return nil, err
	}

	matches := make([]ContactMethodMatch, len(rows))
	for i, row := range rows {
		matches[i] = convertDbContactMethodMatch(row)
	}

	return matches, nil
}

// BulkLinkToContact links multiple identities to a contact
func (r *IdentityRepository) BulkLinkToContact(ctx context.Context, identityIDs []uuid.UUID, contactID uuid.UUID, matchType MatchType, confidence *float64) error {
	pgUUIDs := make([]pgtype.UUID, len(identityIDs))
	for i, id := range identityIDs {
		pgUUIDs[i] = uuidToPgUUID(id)
	}

	params := db.BulkLinkIdentitiesToContactParams{
		Column1:   pgUUIDs,
		ContactID: uuidToPgUUID(contactID),
		MatchType: pgtype.Text{String: string(matchType), Valid: true},
	}

	if confidence != nil {
		params.MatchConfidence = pgtype.Float8{Float64: *confidence, Valid: true}
	}

	return r.queries.BulkLinkIdentitiesToContact(ctx, params)
}

// UpdateMessageCount updates the message count for an identity
func (r *IdentityRepository) UpdateMessageCount(ctx context.Context, id uuid.UUID, deltaCount int32, lastSeenAt time.Time) (*ExternalIdentity, error) {
	dbIdentity, err := r.queries.UpdateIdentityMessageCount(ctx, db.UpdateIdentityMessageCountParams{
		ID:           uuidToPgUUID(id),
		MessageCount: pgtype.Int4{Int32: deltaCount, Valid: true},
		LastSeenAt:   pgtype.Timestamptz{Time: lastSeenAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	ident := convertDbIdentity(dbIdentity)
	return &ident, nil
}
