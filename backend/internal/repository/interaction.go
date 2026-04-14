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
	InteractionSourceManual   = "manual"
	InteractionSourceGCal     = "gcal"
	InteractionSourceTodoist  = "todoist"
	InteractionSourceTelegram = "telegram"
)

// Interaction direction constants
const (
	InteractionDirectionOutbound = "outbound"
	InteractionDirectionInbound  = "inbound"
	InteractionDirectionMutual   = "mutual"
)

// RecordInteractionRequest represents a request to record an interaction via the service layer
type RecordInteractionRequest struct {
	ContactID   uuid.UUID
	Source      string
	SourceRef   *string
	OccurredAt  time.Time
	Description *string
	Direction   string // "outbound", "inbound", "mutual" — defaults to "mutual" if empty
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
	Direction   string    `json:"direction"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateInteractionRequest represents the request to create an interaction
type CreateInteractionRequest struct {
	ContactID   uuid.UUID
	Source      string
	SourceRef   *string
	OccurredAt  time.Time
	Description *string
	Direction   string
}

func convertDbInteraction(dbInteraction *db.Interaction) Interaction {
	interaction := Interaction{
		Source:    dbInteraction.Source,
		Direction: dbInteraction.Direction,
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
		Direction:   stringToPgText(&req.Direction),
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

// FindInWindow finds an existing interaction within a time window for a contact and source
func (r *InteractionRepository) FindInWindow(ctx context.Context, contactID uuid.UUID, source string, occurredAt time.Time, window time.Duration) (*Interaction, error) {
	windowStart := occurredAt.Add(-window)
	windowEnd := occurredAt.Add(window)

	dbInteraction, err := r.queries.FindInteractionInWindow(ctx, db.FindInteractionInWindowParams{
		ContactID:    uuidToPgUUID(contactID),
		OccurredAt:   pgtype.Timestamptz{Time: windowStart, Valid: true},
		OccurredAt_2: pgtype.Timestamptz{Time: windowEnd, Valid: true},
		Source:       source,
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

// FindRecentOutboundTelegramInteraction finds the most recent outbound telegram interaction
// for a contact in a specific chat within a time window.
func (r *InteractionRepository) FindRecentOutboundTelegramInteraction(ctx context.Context, contactID uuid.UUID, sourceRefPrefix string, windowStart, windowEnd time.Time) (*Interaction, error) {
	dbInteraction, err := r.queries.FindRecentOutboundTelegramInteraction(ctx, db.FindRecentOutboundTelegramInteractionParams{
		ContactID:       uuidToPgUUID(contactID),
		SourceRefPrefix: pgtype.Text{String: sourceRefPrefix, Valid: true},
		WindowStart:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:       pgtype.Timestamptz{Time: windowEnd, Valid: true},
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

// FindRecentTelegramInteraction finds the most recent telegram interaction for a contact
// in a specific chat with a given direction. Used for incremental coalescing.
func (r *InteractionRepository) FindRecentTelegramInteraction(ctx context.Context, contactID uuid.UUID, direction, sourceRefPrefix string, windowStart, windowEnd time.Time) (*Interaction, error) {
	dbInteraction, err := r.queries.FindRecentTelegramInteraction(ctx, db.FindRecentTelegramInteractionParams{
		ContactID:       uuidToPgUUID(contactID),
		Direction:       direction,
		SourceRefPrefix: pgtype.Text{String: sourceRefPrefix, Valid: true},
		WindowStart:     pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:       pgtype.Timestamptz{Time: windowEnd, Valid: true},
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

// UpdateInteractionTimestamp extends an existing interaction's occurred_at and description.
func (r *InteractionRepository) UpdateInteractionTimestamp(ctx context.Context, id uuid.UUID, occurredAt time.Time, description *string) (*Interaction, error) {
	dbInteraction, err := r.queries.UpdateInteractionTimestamp(ctx, db.UpdateInteractionTimestampParams{
		ID:          uuidToPgUUID(id),
		OccurredAt:  pgtype.Timestamptz{Time: occurredAt, Valid: true},
		Description: stringToPgText(description),
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

// CreateInteractionTx creates a new interaction inside the caller's tx.
// Used by the InteractionRecorder consumer so the insert commits atomically
// with the interaction.recorded event row (spec §3.4.1).
func (r *InteractionRepository) CreateInteractionTx(ctx context.Context, tx pgx.Tx, req CreateInteractionRequest) (*Interaction, error) {
	dbInteraction, err := db.New(tx).CreateInteraction(ctx, db.CreateInteractionParams{
		ContactID:   uuidToPgUUID(req.ContactID),
		Source:      req.Source,
		SourceRef:   stringToPgText(req.SourceRef),
		OccurredAt:  pgtype.Timestamptz{Time: req.OccurredAt, Valid: true},
		Description: stringToPgText(req.Description),
		Direction:   stringToPgText(&req.Direction),
	})
	if err != nil {
		return nil, err
	}
	interaction := convertDbInteraction(dbInteraction)
	return &interaction, nil
}

// FindBySourceRefTx is the tx-threaded variant of FindBySourceRef. Used by
// the consumer's HandleEvent to dedup inside the caller's tx.
func (r *InteractionRepository) FindBySourceRefTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source string, sourceRef string) (*Interaction, error) {
	dbInteraction, err := db.New(tx).FindInteractionBySourceRef(ctx, db.FindInteractionBySourceRefParams{
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

// FindInWindowTx is the tx-threaded variant of FindInWindow. Used by the
// consumer for manual-kind dedup.
func (r *InteractionRepository) FindInWindowTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source string, occurredAt time.Time, window time.Duration) (*Interaction, error) {
	windowStart := occurredAt.Add(-window)
	windowEnd := occurredAt.Add(window)

	dbInteraction, err := db.New(tx).FindInteractionInWindow(ctx, db.FindInteractionInWindowParams{
		ContactID:    uuidToPgUUID(contactID),
		OccurredAt:   pgtype.Timestamptz{Time: windowStart, Valid: true},
		OccurredAt_2: pgtype.Timestamptz{Time: windowEnd, Valid: true},
		Source:       source,
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

// UpdateInteractionDirection updates the direction and occurred_at of an interaction (for reply bridging)
func (r *InteractionRepository) UpdateInteractionDirection(ctx context.Context, id uuid.UUID, direction string, occurredAt time.Time) (*Interaction, error) {
	dbInteraction, err := r.queries.UpdateInteractionDirection(ctx, db.UpdateInteractionDirectionParams{
		ID:         uuidToPgUUID(id),
		Direction:  direction,
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
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
