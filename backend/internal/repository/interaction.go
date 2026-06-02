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
	InteractionSourceManual          = "manual"
	InteractionSourceGCal            = "gcal"
	InteractionSourceTodoist         = "todoist"
	InteractionSourceTelegram        = "telegram"
	InteractionSourceMessages        = "messages"
	InteractionSourceAnarlogSessions = "anarlog_sessions"
	InteractionSourcePhoneCalls      = "phone_calls"
	InteractionSourceEmail           = "email"
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
	// writes when the caller passes publishesEvent=true (event-bus path,
	// so the snapshot can populate the V2 payload); nil on replay and
	// on the non-bus wrapper path.
	PrevCadence *ContactCadenceFields
	// CadenceAtEmit is the value of contact.cadence at capture time
	// (may be nil if the contact has no cadence). Non-nil only when
	// publishesEvent=true and cadence is set.
	CadenceAtEmit *string
	// FollowUpFn is the non-bus path's post-commit hook. Set only when
	// publishesEvent=false — the bus path runs FollowUpManager.HandleEvent
	// inline inside the recorder's tx and carries any refresh
	// post-commit closure through the recorder's own return. Nil
	// otherwise. Caller invokes AFTER tx commit.
	FollowUpFn func(context.Context)
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

// SoftDeleteInteractionTx is the tx-bound variant of SoftDeleteInteraction.
// Used by the meeting_note inline handler's re-sync diff path and the
// meeting_note.deleted cascade so the soft-delete commits atomically
// with the meeting_note row update.
func (r *InteractionRepository) SoftDeleteInteractionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return db.New(tx).SoftDeleteInteraction(ctx, uuidToPgUUID(id))
}

// ListSessionAttributedInteractionsTx returns all live interactions
// attributed to a specific anarlog session (source_ref starts with the
// `anarlog:<session-uuid>:` prefix). Used by the re-sync diff path to
// compute the (existing - desired) set that needs soft-deleting and by
// the meeting_note.deleted cascade. Caller owns the tx lifecycle.
func (r *InteractionRepository) ListSessionAttributedInteractionsTx(ctx context.Context, tx pgx.Tx, sourceRefPrefix string) ([]Interaction, error) {
	dbInteractions, err := db.New(tx).ListSessionAttributedInteractions(ctx, pgtype.Text{String: sourceRefPrefix, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]Interaction, len(dbInteractions))
	for i, dbInteraction := range dbInteractions {
		out[i] = convertDbInteraction(dbInteraction)
	}
	return out, nil
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

// FindInWindow finds an existing interaction within a time window for a
// contact, source, and direction. Direction is part of the dedup key
// because the manual logger lets users record any direction — an
// outbound recorded just before an inbound for the same contact must
// NOT collapse to a single row.
func (r *InteractionRepository) FindInWindow(ctx context.Context, contactID uuid.UUID, source, direction string, occurredAt time.Time, window time.Duration) (*Interaction, error) {
	windowStart := occurredAt.Add(-window)
	windowEnd := occurredAt.Add(window)

	dbInteraction, err := r.queries.FindInteractionInWindow(ctx, db.FindInteractionInWindowParams{
		ContactID:   uuidToPgUUID(contactID),
		Source:      source,
		Direction:   direction,
		WindowStart: pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: windowEnd, Valid: true},
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

// FindRecentInteractionBySourceAndDirection is the source-neutral version of
// FindRecentTelegramInteraction. The shared aggregator
// (backend/internal/messaging/aggregation) uses this for same-direction
// coalescing across sources (telegram, messages, whatsapp).
func (r *InteractionRepository) FindRecentInteractionBySourceAndDirection(
	ctx context.Context,
	contactID uuid.UUID,
	source, direction, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*Interaction, error) {
	dbInteraction, err := r.queries.FindRecentInteractionBySourceAndDirection(ctx, db.FindRecentInteractionBySourceAndDirectionParams{
		ContactID:       uuidToPgUUID(contactID),
		Source:          source,
		Direction:       direction,
		SourceRefPrefix: sourceRefPrefix,
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

// FindRecentOutboundInteractionBySource is the source-neutral version of
// FindRecentOutboundTelegramInteraction. Used by the shared aggregator for
// time-based reply bridging on inbound sessions.
func (r *InteractionRepository) FindRecentOutboundInteractionBySource(
	ctx context.Context,
	contactID uuid.UUID,
	source, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*Interaction, error) {
	dbInteraction, err := r.queries.FindRecentOutboundInteractionBySource(ctx, db.FindRecentOutboundInteractionBySourceParams{
		ContactID:       uuidToPgUUID(contactID),
		Source:          source,
		SourceRefPrefix: sourceRefPrefix,
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

// HardDeleteInteractionsBySourceRefPrefix is a test-only helper that
// hard-deletes interactions whose source matches and source_ref begins
// with prefix. Used by integration tests for per-run cleanup; soft-delete
// is unsafe because the (source, source_ref) partial unique index would
// otherwise block re-inserts on subsequent runs.
func (r *InteractionRepository) HardDeleteInteractionsBySourceRefPrefix(ctx context.Context, source, sourceRefPrefix string) error {
	return r.queries.HardDeleteInteractionsBySourceRefPrefix(ctx, db.HardDeleteInteractionsBySourceRefPrefixParams{
		Source:          source,
		SourceRefPrefix: pgtype.Text{String: sourceRefPrefix, Valid: true},
	})
}

// GetInteractionSourceCheckDef is a test-only helper returning the live
// rendered definition of the interaction_source_check CHECK constraint
// (via pg_get_constraintdef). The descriptor-vs-CHECK agreement test
// parses the returned ARRAY[...] string literals to assert the live
// CHECK set matches the InteractionSource* constants set-for-set.
// Read-only catalog access; production code never calls this.
func (r *InteractionRepository) GetInteractionSourceCheckDef(ctx context.Context) (string, error) {
	return r.queries.GetInteractionSourceCheckDef(ctx)
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

// AcquireSourceRefLockTx takes a transaction-scoped advisory lock keyed on
// the interaction aggregation source_ref, inside the caller's tx. The
// email-interaction consumer calls it before FindBySourceRefTx so all jobs
// for the same (contact, thread, local-day) aggregation key serialize: a
// second same-key job blocks until the first commits (releasing the lock),
// then proceeds with a fresh read. This makes the forward-only occurred_at
// read-compute-write atomic per key. Cross-key jobs hash to different keys
// and never block each other. The lock auto-releases on commit/rollback.
func (r *InteractionRepository) AcquireSourceRefLockTx(ctx context.Context, tx pgx.Tx, sourceRef string) error {
	return db.New(tx).AcquireSourceRefAggregateLock(ctx, sourceRef)
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
func (r *InteractionRepository) FindInWindowTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source, direction string, occurredAt time.Time, window time.Duration) (*Interaction, error) {
	windowStart := occurredAt.Add(-window)
	windowEnd := occurredAt.Add(window)

	dbInteraction, err := db.New(tx).FindInteractionInWindow(ctx, db.FindInteractionInWindowParams{
		ContactID:   uuidToPgUUID(contactID),
		Source:      source,
		Direction:   direction,
		WindowStart: pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: windowEnd, Valid: true},
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
