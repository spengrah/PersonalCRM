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

// Phone call service enum constants. Frozen per spec §`phone_calls` source.
// Adding a new service requires both a daemon update and a Pi migration.
const (
	PhoneCallServiceVoice         = "voice"
	PhoneCallServiceFaceTimeAudio = "facetime_audio"
	PhoneCallServiceFaceTimeVideo = "facetime_video"
)

// Phone call direction constants. Mirrors interaction.direction values used
// for phone_calls.
const (
	PhoneCallDirectionInbound  = "inbound"
	PhoneCallDirectionOutbound = "outbound"
)

// PhoneCall is the in-memory representation of a phone_call staging row.
// Mirrors the spec §`phone_call` staging table. NO DeletedAt field: there
// is no deleted_at column on the staging table (no aggregator-driven
// lifecycle).
type PhoneCall struct {
	ID               uuid.UUID
	CallUniqueID     string
	PeerHandle       string
	PeerNormalized   string
	Service          string
	Direction        string
	Answered         *bool // NULLable; only set for inbound rows
	HasVoicemail     bool
	DurationSeconds  int32
	StartedAt        time.Time
	MatchedContactID *uuid.UUID
	InteractionID    *uuid.UUID
	MacHostID        *uuid.UUID
	ProcessedAt      *time.Time
	CreatedAt        time.Time
}

// UpsertPhoneCallParams is the input for UpsertCall. Fields map 1:1 to the
// columns in the upsert query. Answered is *bool because the column is
// three-state (true/false/NULL) — NULL outbound, true/false inbound.
type UpsertPhoneCallParams struct {
	CallUniqueID     string
	PeerHandle       string
	PeerNormalized   string
	Service          string
	Direction        string
	Answered         *bool
	HasVoicemail     bool
	DurationSeconds  int32
	StartedAt        time.Time
	MatchedContactID *uuid.UUID
	MacHostID        *uuid.UUID
}

// PhoneCallRepository wraps the sqlc-generated phone_call queries.
type PhoneCallRepository struct {
	queries db.Querier
}

// NewPhoneCallRepository creates a new phone_call repository.
func NewPhoneCallRepository(queries db.Querier) *PhoneCallRepository {
	return &PhoneCallRepository{queries: queries}
}

func convertDbPhoneCall(c *db.PhoneCall) PhoneCall {
	pc := PhoneCall{
		CallUniqueID:    c.CallUniqueID,
		PeerHandle:      c.PeerHandle,
		PeerNormalized:  c.PeerNormalized,
		Service:         c.Service,
		Direction:       c.Direction,
		HasVoicemail:    c.HasVoicemail,
		DurationSeconds: c.DurationSeconds,
	}
	if c.ID.Valid {
		pc.ID = uuid.UUID(c.ID.Bytes)
	}
	if c.Answered.Valid {
		v := c.Answered.Bool
		pc.Answered = &v
	}
	if c.StartedAt.Valid {
		pc.StartedAt = c.StartedAt.Time
	}
	if c.MatchedContactID.Valid {
		id := uuid.UUID(c.MatchedContactID.Bytes)
		pc.MatchedContactID = &id
	}
	if c.InteractionID.Valid {
		id := uuid.UUID(c.InteractionID.Bytes)
		pc.InteractionID = &id
	}
	if c.MacHostID.Valid {
		id := uuid.UUID(c.MacHostID.Bytes)
		pc.MacHostID = &id
	}
	if c.ProcessedAt.Valid {
		pc.ProcessedAt = &c.ProcessedAt.Time
	}
	if c.CreatedAt.Valid {
		pc.CreatedAt = c.CreatedAt.Time
	}
	return pc
}

func buildUpsertPhoneCallParams(params UpsertPhoneCallParams) db.UpsertPhoneCallParams {
	var matchedContactID pgtype.UUID
	if params.MatchedContactID != nil {
		matchedContactID = uuidToPgUUID(*params.MatchedContactID)
	}
	var macHostID pgtype.UUID
	if params.MacHostID != nil {
		macHostID = uuidToPgUUID(*params.MacHostID)
	}
	var answered pgtype.Bool
	if params.Answered != nil {
		answered = pgtype.Bool{Bool: *params.Answered, Valid: true}
	}
	return db.UpsertPhoneCallParams{
		CallUniqueID:     params.CallUniqueID,
		PeerHandle:       params.PeerHandle,
		PeerNormalized:   params.PeerNormalized,
		Service:          params.Service,
		Direction:        params.Direction,
		Answered:         answered,
		HasVoicemail:     params.HasVoicemail,
		DurationSeconds:  params.DurationSeconds,
		StartedAt:        pgtype.Timestamptz{Time: params.StartedAt, Valid: true},
		MatchedContactID: matchedContactID,
		MacHostID:        macHostID,
	}
}

// UpsertCall creates or updates a phone_call row by call_unique_id. Non-tx
// variant; used by tests.
func (r *PhoneCallRepository) UpsertCall(ctx context.Context, params UpsertPhoneCallParams) (*PhoneCall, error) {
	dbCall, err := r.queries.UpsertPhoneCall(ctx, buildUpsertPhoneCallParams(params))
	if err != nil {
		return nil, err
	}
	c := convertDbPhoneCall(dbCall)
	return &c, nil
}

// UpsertCallTx is the tx-bound variant of UpsertCall. Used by the ingest
// service so the staging-row write commits atomically with the interaction
// insert and the interaction.recorded event publish. Caller owns the tx
// lifecycle.
func (r *PhoneCallRepository) UpsertCallTx(ctx context.Context, tx pgx.Tx, params UpsertPhoneCallParams) (*PhoneCall, error) {
	dbCall, err := db.New(tx).UpsertPhoneCall(ctx, buildUpsertPhoneCallParams(params))
	if err != nil {
		return nil, err
	}
	c := convertDbPhoneCall(dbCall)
	return &c, nil
}

// GetCallByUniqueID retrieves a phone_call row by call_unique_id. Returns
// db.ErrNotFound on miss.
func (r *PhoneCallRepository) GetCallByUniqueID(ctx context.Context, callUniqueID string) (*PhoneCall, error) {
	dbCall, err := r.queries.GetPhoneCallByUniqueID(ctx, callUniqueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	c := convertDbPhoneCall(dbCall)
	return &c, nil
}

// MarkProcessedParams is the input for MarkProcessed/MarkProcessedTx.
// InteractionID is *uuid.UUID because missed-inbound-no-voicemail rows have
// no interaction (content-delivered cadence; spec §`phone_calls` source).
type MarkProcessedParams struct {
	ID            uuid.UUID
	InteractionID *uuid.UUID
}

// MarkProcessed sets processed_at = NOW() and optionally links the staging
// row to its derived interaction. Non-tx variant; used by tests.
func (r *PhoneCallRepository) MarkProcessed(ctx context.Context, params MarkProcessedParams) error {
	var interactionID pgtype.UUID
	if params.InteractionID != nil {
		interactionID = uuidToPgUUID(*params.InteractionID)
	}
	return r.queries.MarkPhoneCallProcessed(ctx, db.MarkPhoneCallProcessedParams{
		ID:            uuidToPgUUID(params.ID),
		InteractionID: interactionID,
	})
}

// MarkProcessedTx is the tx-bound variant of MarkProcessed.
func (r *PhoneCallRepository) MarkProcessedTx(ctx context.Context, tx pgx.Tx, params MarkProcessedParams) error {
	var interactionID pgtype.UUID
	if params.InteractionID != nil {
		interactionID = uuidToPgUUID(*params.InteractionID)
	}
	return db.New(tx).MarkPhoneCallProcessed(ctx, db.MarkPhoneCallProcessedParams{
		ID:            uuidToPgUUID(params.ID),
		InteractionID: interactionID,
	})
}

// HardDeleteByMacHost is a test-only helper that hard-deletes phone_call
// rows by mac_host_id. Used by integration tests for per-run cleanup;
// soft-delete is not available on this table (no deleted_at column).
func (r *PhoneCallRepository) HardDeleteByMacHost(ctx context.Context, macHostID uuid.UUID) error {
	return r.queries.HardDeletePhoneCallsByMacHost(ctx, uuidToPgUUID(macHostID))
}

// HardDeleteByUniqueID is a test-only helper that hard-deletes a single
// phone_call row by call_unique_id. Used by migration round-trip tests to
// clear the staging row before the down migration's row-bearing guard
// runs.
func (r *PhoneCallRepository) HardDeleteByUniqueID(ctx context.Context, callUniqueID string) error {
	return r.queries.HardDeletePhoneCallByUniqueID(ctx, callUniqueID)
}
