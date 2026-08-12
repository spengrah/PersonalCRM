package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	enrichment.ID = dbEnrichment.ID
	enrichment.ContactID = dbEnrichment.ContactID
	enrichment.ExternalContactID = dbEnrichment.ExternalContactID

	// Convert optional strings
	enrichment.AccountID = dbEnrichment.AccountID
	enrichment.OriginalValue = dbEnrichment.OriginalValue

	// Convert timestamp
	if dbEnrichment.EnrichedAt != nil {
		enrichment.EnrichedAt = *dbEnrichment.EnrichedAt
	}

	return enrichment
}

// Create records a new enrichment or updates an existing one
func (r *EnrichmentRepository) Create(ctx context.Context, req CreateEnrichmentRequest) (*ContactEnrichment, error) {
	params := db.CreateEnrichmentParams{
		ContactID:         req.ContactID,
		Source:            req.Source,
		Field:             req.Field,
		AccountID:         req.AccountID,
		ExternalContactID: req.ExternalContactID,
		OriginalValue:     req.OriginalValue,
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
		ContactID: contactID,
		Field:     field,
	})
}

// GetByField retrieves an enrichment by contact ID and field
func (r *EnrichmentRepository) GetByField(ctx context.Context, contactID uuid.UUID, field string) (*ContactEnrichment, error) {
	dbEnrichment, err := r.queries.GetEnrichmentByField(ctx, db.GetEnrichmentByFieldParams{
		ContactID: contactID,
		Field:     field,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return convertDbEnrichment(dbEnrichment), nil
}

// ListForContact retrieves all enrichments for a contact
func (r *EnrichmentRepository) ListForContact(ctx context.Context, contactID uuid.UUID) ([]ContactEnrichment, error) {
	dbEnrichments, err := r.queries.GetEnrichmentsForContact(ctx, contactID)
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
	return r.queries.DeleteEnrichmentsForContact(ctx, contactID)
}

// Delete removes an enrichment by ID
func (r *EnrichmentRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteEnrichment(ctx, id)
}
