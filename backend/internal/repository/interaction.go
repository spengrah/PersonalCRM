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

// Interaction source constants
const (
	InteractionSourceManual  = "manual"
	InteractionSourceGCal    = "gcal"
	InteractionSourceTodoist = "todoist"
)

// RecordInteractionRequest represents a request to record an interaction via the service layer
type RecordInteractionRequest struct {
	ContactID   uuid.UUID
	Source      string
	SourceRef   *string
	OccurredAt  time.Time
	Description *string
}

// InteractionRepository handles interaction persistence
type InteractionRepository struct {
	queries db.Querier
}

// NewInteractionRepository creates a new InteractionRepository
func NewInteractionRepository(queries db.Querier) *InteractionRepository {
	return &InteractionRepository{queries: queries}
}

// Interaction represents an interaction with a contact
type Interaction struct {
	ID          uuid.UUID `json:"id"`
	ContactID   uuid.UUID `json:"contact_id"`
	Source      string    `json:"source"`
	SourceRef   *string   `json:"source_ref,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateInteractionRequest represents the request to create an interaction
type CreateInteractionRequest struct {
	ContactID   uuid.UUID
	Source      string
	SourceRef   *string
	OccurredAt  time.Time
	Description *string
}

func convertDbInteraction(dbInteraction *db.Interaction) Interaction {
	interaction := Interaction{
		Source: dbInteraction.Source,
	}

	if dbInteraction.ID.Valid {
		interaction.ID = uuid.UUID(dbInteraction.ID.Bytes)
	}
	if dbInteraction.ContactID.Valid {
		interaction.ContactID = uuid.UUID(dbInteraction.ContactID.Bytes)
	}
	if dbInteraction.OccurredAt.Valid {
		interaction.OccurredAt = dbInteraction.OccurredAt.Time.UTC()
	}
	if dbInteraction.CreatedAt.Valid {
		interaction.CreatedAt = dbInteraction.CreatedAt.Time.UTC()
	}
	if dbInteraction.SourceRef.Valid {
		interaction.SourceRef = &dbInteraction.SourceRef.String
	}
	if dbInteraction.Description.Valid {
		interaction.Description = &dbInteraction.Description.String
	}

	return interaction
}

// GetInteraction retrieves an interaction by ID
func (r *InteractionRepository) GetInteraction(ctx context.Context, id uuid.UUID) (*Interaction, error) {
	dbInteraction, err := r.queries.GetInteraction(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	interaction := convertDbInteraction(dbInteraction)
	return &interaction, nil
}

// ListContactInteractions retrieves paginated interactions for a contact
func (r *InteractionRepository) ListContactInteractions(ctx context.Context, contactID uuid.UUID, limit, offset int32) ([]Interaction, error) {
	dbInteractions, err := r.queries.ListContactInteractions(ctx, db.ListContactInteractionsParams{
		ContactID: uuidToPgUUID(contactID),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		return nil, err
	}

	interactions := make([]Interaction, len(dbInteractions))
	for i, dbInteraction := range dbInteractions {
		interactions[i] = convertDbInteraction(dbInteraction)
	}

	return interactions, nil
}

// CountContactInteractions returns the total number of interactions for a contact
func (r *InteractionRepository) CountContactInteractions(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.CountContactInteractions(ctx, uuidToPgUUID(contactID))
}

// CreateInteraction creates a new interaction record
func (r *InteractionRepository) CreateInteraction(ctx context.Context, req CreateInteractionRequest) (*Interaction, error) {
	dbInteraction, err := r.queries.CreateInteraction(ctx, db.CreateInteractionParams{
		ContactID:   uuidToPgUUID(req.ContactID),
		Source:      req.Source,
		SourceRef:   stringToPgText(req.SourceRef),
		OccurredAt:  pgtype.Timestamptz{Time: req.OccurredAt, Valid: true},
		Description: stringToPgText(req.Description),
	})
	if err != nil {
		return nil, err
	}

	interaction := convertDbInteraction(dbInteraction)
	return &interaction, nil
}

// SoftDeleteInteraction soft-deletes an interaction
func (r *InteractionRepository) SoftDeleteInteraction(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteInteraction(ctx, uuidToPgUUID(id))
}

// FindBySourceRef finds an existing interaction by contact, source, and source_ref
func (r *InteractionRepository) FindBySourceRef(ctx context.Context, contactID uuid.UUID, source string, sourceRef string) (*Interaction, error) {
	dbInteraction, err := r.queries.FindInteractionBySourceRef(ctx, db.FindInteractionBySourceRefParams{
		ContactID: uuidToPgUUID(contactID),
		Source:    source,
		SourceRef: pgtype.Text{String: sourceRef, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	interaction := convertDbInteraction(dbInteraction)
	return &interaction, nil
}

// FindInWindow finds an existing interaction within a time window for a contact
func (r *InteractionRepository) FindInWindow(ctx context.Context, contactID uuid.UUID, occurredAt time.Time, window time.Duration) (*Interaction, error) {
	windowStart := occurredAt.Add(-window)
	windowEnd := occurredAt.Add(window)

	dbInteraction, err := r.queries.FindInteractionInWindow(ctx, db.FindInteractionInWindowParams{
		ContactID:    uuidToPgUUID(contactID),
		OccurredAt:   pgtype.Timestamptz{Time: windowStart, Valid: true},
		OccurredAt_2: pgtype.Timestamptz{Time: windowEnd, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	interaction := convertDbInteraction(dbInteraction)
	return &interaction, nil
}
