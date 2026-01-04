package repository

import (
	"context"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ContactEnrichment represents a record of enrichment applied to a CRM contact
type ContactEnrichment struct {
	ID                uuid.UUID  `json:"id"`
	ContactID         uuid.UUID  `json:"contact_id"`
	Source            string     `json:"source"`
	AccountID         *string    `json:"account_id,omitempty"`
	Field             string     `json:"field"`
	ExternalContactID *uuid.UUID `json:"external_contact_id,omitempty"`
	OriginalValue     *string    `json:"original_value,omitempty"`
	EnrichedAt        time.Time  `json:"enriched_at"`
}

// CreateEnrichmentRequest holds parameters for creating an enrichment record
type CreateEnrichmentRequest struct {
	ContactID         uuid.UUID  `json:"contact_id"`
	Source            string     `json:"source"`
	AccountID         *string    `json:"account_id,omitempty"`
	Field             string     `json:"field"`
	ExternalContactID *uuid.UUID `json:"external_contact_id,omitempty"`
	OriginalValue     *string    `json:"original_value,omitempty"`
}

// EnrichmentRepository handles contact enrichment persistence
type EnrichmentRepository struct {
	queries db.Querier
}

// NewEnrichmentRepository creates a new enrichment repository
func NewEnrichmentRepository(queries db.Querier) *EnrichmentRepository {
	return &EnrichmentRepository{queries: queries}
}

// convertDbEnrichment converts a database enrichment to a repository model
func convertDbEnrichment(dbEnrichment *db.ContactEnrichment) *ContactEnrichment {
	enrichment := &ContactEnrichment{
		Source: dbEnrichment.Source,
		Field:  dbEnrichment.Field,
	}

	// Convert UUID
	if dbEnrichment.ID.Valid {
		enrichment.ID = uuid.UUID(dbEnrichment.ID.Bytes)
	}
	if dbEnrichment.ContactID.Valid {
		enrichment.ContactID = uuid.UUID(dbEnrichment.ContactID.Bytes)
	}
	if dbEnrichment.ExternalContactID.Valid {
		id := uuid.UUID(dbEnrichment.ExternalContactID.Bytes)
		enrichment.ExternalContactID = &id
	}

	// Convert optional strings
	if dbEnrichment.AccountID.Valid {
		enrichment.AccountID = &dbEnrichment.AccountID.String
	}
	if dbEnrichment.OriginalValue.Valid {
		enrichment.OriginalValue = &dbEnrichment.OriginalValue.String
	}

	// Convert timestamp
	if dbEnrichment.EnrichedAt.Valid {
		enrichment.EnrichedAt = dbEnrichment.EnrichedAt.Time
	}

	return enrichment
}

// Create records a new enrichment or updates an existing one
func (r *EnrichmentRepository) Create(ctx context.Context, req CreateEnrichmentRequest) (*ContactEnrichment, error) {
	params := db.CreateEnrichmentParams{
		ContactID: pgtype.UUID{Bytes: req.ContactID, Valid: true},
		Source:    req.Source,
		Field:     req.Field,
	}

	if req.AccountID != nil {
		params.AccountID = pgtype.Text{String: *req.AccountID, Valid: true}
	}
	if req.ExternalContactID != nil {
		params.ExternalContactID = pgtype.UUID{Bytes: *req.ExternalContactID, Valid: true}
	}
	if req.OriginalValue != nil {
		params.OriginalValue = pgtype.Text{String: *req.OriginalValue, Valid: true}
	}

	dbEnrichment, err := r.queries.CreateEnrichment(ctx, params)
	if err != nil {
		return nil, err
	}
	return convertDbEnrichment(dbEnrichment), nil
}

// HasEnrichment checks if a field has already been enriched for a contact
func (r *EnrichmentRepository) HasEnrichment(ctx context.Context, contactID uuid.UUID, field string) (bool, error) {
	return r.queries.HasEnrichmentForField(ctx, db.HasEnrichmentForFieldParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		Field:     field,
	})
}

// GetByField retrieves an enrichment by contact ID and field
func (r *EnrichmentRepository) GetByField(ctx context.Context, contactID uuid.UUID, field string) (*ContactEnrichment, error) {
	dbEnrichment, err := r.queries.GetEnrichmentByField(ctx, db.GetEnrichmentByFieldParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		Field:     field,
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return convertDbEnrichment(dbEnrichment), nil
}

// ListForContact retrieves all enrichments for a contact
func (r *EnrichmentRepository) ListForContact(ctx context.Context, contactID uuid.UUID) ([]ContactEnrichment, error) {
	dbEnrichments, err := r.queries.GetEnrichmentsForContact(ctx, pgtype.UUID{Bytes: contactID, Valid: true})
	if err != nil {
		return nil, err
	}

	enrichments := make([]ContactEnrichment, 0, len(dbEnrichments))
	for _, dbEnrichment := range dbEnrichments {
		enrichments = append(enrichments, *convertDbEnrichment(dbEnrichment))
	}
	return enrichments, nil
}

// ListBySource retrieves enrichments by source
func (r *EnrichmentRepository) ListBySource(ctx context.Context, source string, limit, offset int32) ([]ContactEnrichment, error) {
	dbEnrichments, err := r.queries.ListEnrichmentsBySource(ctx, db.ListEnrichmentsBySourceParams{
		Source: source,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	enrichments := make([]ContactEnrichment, 0, len(dbEnrichments))
	for _, dbEnrichment := range dbEnrichments {
		enrichments = append(enrichments, *convertDbEnrichment(dbEnrichment))
	}
	return enrichments, nil
}

// DeleteForContact removes all enrichments for a contact
func (r *EnrichmentRepository) DeleteForContact(ctx context.Context, contactID uuid.UUID) error {
	return r.queries.DeleteEnrichmentsForContact(ctx, pgtype.UUID{Bytes: contactID, Valid: true})
}

// Delete removes an enrichment by ID
func (r *EnrichmentRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteEnrichment(ctx, pgtype.UUID{Bytes: id, Valid: true})
}
