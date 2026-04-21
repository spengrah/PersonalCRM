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

// ApplyInteractionRequest is the direct-invoke input for
// CadenceUpdater.ApplyInteraction. Used by ExtendInteraction and
// PromoteInteractionToMutual to route cadence writes through the
// consumer even though they don't emit interaction.recorded events
// themselves. Lives here so ContactService can define its cadence
// dependency as a narrow interface without importing the consumer
// package.
type ApplyInteractionRequest struct {
	ContactID  uuid.UUID
	Direction  string // InteractionDirection*
	Source     string // InteractionSource* — source="manual" takes unconditional branch
	OccurredAt time.Time
}

// RecordInteractionResult bundles the non-error returns of
// ContactService.RecordInteractionTx. Lives in repository (next to
// RecordInteractionRequest) so consumer.interactionWriter can reference
// it without depending on the service package. Fields carry the prior
// positional-return shape as named fields; see
// ContactService.RecordInteractionTx for per-field nilability contracts.
type RecordInteractionResult struct {
	// Interaction is the persisted row — either freshly-inserted OR the
	// existing dedup-hit row.
	Interaction *Interaction
	// IsReplay is true if FindBySourceRefTx / FindInWindowTx returned an
	// existing row; false on fresh insert. Consumers branch on this to
	// skip interaction.recorded emit on replay (spec §3.4.1).
	IsReplay bool
	// PrevCadence is the pre-cadence snapshot captured in-memory from
	// the contact row BEFORE the authoritative UPDATE. Non-nil on fresh
	// writes when the caller passes withShadow=true; nil on replay and
	// when withShadow=false.
	PrevCadence *ContactCadenceFields
	// CadenceAtEmit is the value of contact.cadence at capture time
	// (may be nil if the contact has no cadence). Non-nil only when
	// withShadow=true and cadence is set.
	CadenceAtEmit *string
	// FollowUpFn is the follow-up-manager closure on fresh writes with
	// a configured manager. Nil otherwise. Caller invokes AFTER tx commit.
	FollowUpFn func(context.Context)
	// ShadowDrainFn is the PR 7 cadence shadow observation drain.
	// Non-nil on fresh writes when the shadow observer is wired AND
	// withShadow is true. Caller invokes AFTER the interaction.recorded
	// event is published, passing recordedEnv.ID so direct and consumer
	// observations share a matching event_id (plan Decision 6).
	ShadowDrainFn CadenceShadowDrainFn
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

// UpdateInteractionDirectionTx is the tx-threaded variant.
func (r *InteractionRepository) UpdateInteractionDirectionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, direction string, occurredAt time.Time) (*Interaction, error) {
	dbInteraction, err := db.New(tx).UpdateInteractionDirection(ctx, db.UpdateInteractionDirectionParams{
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

// UpdateInteractionTimestampTx is the tx-threaded variant of
// UpdateInteractionTimestamp (same-direction coalescing).
func (r *InteractionRepository) UpdateInteractionTimestampTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, occurredAt time.Time, description *string) (*Interaction, error) {
	dbInteraction, err := db.New(tx).UpdateInteractionTimestamp(ctx, db.UpdateInteractionTimestampParams{
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

// HasResponseAfter reports whether any inbound or mutual interaction
// exists for the contact with occurred_at strictly greater than
// outreachAt. Used by the follow-up consumer's out-of-order guard so an
// outbound event arriving after a later response has already landed
// does not produce a stale follow-up.
func (r *InteractionRepository) HasResponseAfter(ctx context.Context, contactID uuid.UUID, outreachAt time.Time) (bool, error) {
	hasResp, err := r.queries.HasResponseAfter(ctx, db.HasResponseAfterParams{
		ContactID:  uuidToPgUUID(contactID),
		OutreachAt: pgtype.Timestamptz{Time: outreachAt, Valid: true},
	})
	if err != nil {
		return false, err
	}
	return hasResp, nil
}

// HasResponseAfterTx is the tx-threaded variant of HasResponseAfter.
// The consumer worker holds its tx across guard evaluation to read its
// own prior writes within the worker's unit of work.
func (r *InteractionRepository) HasResponseAfterTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, outreachAt time.Time) (bool, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	hasResp, err := q.HasResponseAfter(ctx, db.HasResponseAfterParams{
		ContactID:  uuidToPgUUID(contactID),
		OutreachAt: pgtype.Timestamptz{Time: outreachAt, Valid: true},
	})
	if err != nil {
		return false, err
	}
	return hasResp, nil
}
