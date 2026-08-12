package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	pc.ID = c.ID
	pc.Answered = c.Answered
	pc.StartedAt = c.StartedAt
	pc.MatchedContactID = c.MatchedContactID
	pc.InteractionID = c.InteractionID
	pc.MacHostID = c.MacHostID
	pc.ProcessedAt = c.ProcessedAt
	if c.CreatedAt != nil {
		pc.CreatedAt = *c.CreatedAt
	}
	return pc
}

func buildUpsertPhoneCallParams(params UpsertPhoneCallParams) db.UpsertPhoneCallParams {
	return db.UpsertPhoneCallParams{
		CallUniqueID:     params.CallUniqueID,
		PeerHandle:       params.PeerHandle,
		PeerNormalized:   params.PeerNormalized,
		Service:          params.Service,
		Direction:        params.Direction,
		Answered:         params.Answered,
		HasVoicemail:     params.HasVoicemail,
		DurationSeconds:  params.DurationSeconds,
		StartedAt:        params.StartedAt,
		MatchedContactID: params.MatchedContactID,
		MacHostID:        params.MacHostID,
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

// GetCallByID retrieves a phone_call row by primary-key UUID. Returns
// db.ErrNotFound on miss. Non-tx variant for handler/service flows that
// don't need a long-running transaction.
func (r *PhoneCallRepository) GetCallByID(ctx context.Context, id uuid.UUID) (*PhoneCall, error) {
	dbCall, err := r.queries.GetPhoneCallByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	c := convertDbPhoneCall(dbCall)
	return &c, nil
}

// GetCallByIDTx is the tx-bound variant of GetCallByID.
func (r *PhoneCallRepository) GetCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*PhoneCall, error) {
	dbCall, err := db.New(tx).GetPhoneCallByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	c := convertDbPhoneCall(dbCall)
	return &c, nil
}

// FindLinkageCandidatesTx returns phone_call rows whose started_at
// falls inside the linkage window, projected into the LinkageCandidate
// sum shape. The Pi-side meeting_note linkage handler unions these with
// CalendarEventRepository.FindLinkageCandidatesTx so Step 3's overlap
// math covers both candidate dimensions per spec §Step 1.
func (r *PhoneCallRepository) FindLinkageCandidatesTx(ctx context.Context, tx pgx.Tx, windowStart, windowEnd time.Time) ([]LinkageCandidate, error) {
	rows, err := db.New(tx).FindPhoneCallsInWindow(ctx, db.FindPhoneCallsInWindowParams{
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	})
	if err != nil {
		return nil, err
	}
	out := make([]LinkageCandidate, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		c := convertDbPhoneCall(row)
		out = append(out, LinkageCandidate{
			Kind:            LinkedKindPhoneCall,
			ID:              c.ID,
			OccurredAt:      c.StartedAt,
			PeerContactID:   c.MatchedContactID,
			PeerNormalized:  c.PeerNormalized,
			Service:         c.Service,
			Direction:       c.Direction,
			DurationSeconds: c.DurationSeconds,
			Answered:        c.Answered,
			InteractionID:   c.InteractionID,
		})
	}
	return out, nil
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
	return r.queries.MarkPhoneCallProcessed(ctx, db.MarkPhoneCallProcessedParams{
		ID:            params.ID,
		InteractionID: params.InteractionID,
	})
}

// MarkProcessedTx is the tx-bound variant of MarkProcessed.
func (r *PhoneCallRepository) MarkProcessedTx(ctx context.Context, tx pgx.Tx, params MarkProcessedParams) error {
	return db.New(tx).MarkPhoneCallProcessed(ctx, db.MarkPhoneCallProcessedParams{
		ID:            params.ID,
		InteractionID: params.InteractionID,
	})
}

// HardDeleteByMacHost is a test-only helper that hard-deletes phone_call
// rows by mac_host_id. Used by integration tests for per-run cleanup;
// soft-delete is not available on this table (no deleted_at column).
func (r *PhoneCallRepository) HardDeleteByMacHost(ctx context.Context, macHostID uuid.UUID) error {
	return r.queries.HardDeletePhoneCallsByMacHost(ctx, &macHostID)
}

// HardDeleteByUniqueID is a test-only helper that hard-deletes a single
// phone_call row by call_unique_id. Used by migration round-trip tests to
// clear the staging row before the down migration's row-bearing guard
// runs.
func (r *PhoneCallRepository) HardDeleteByUniqueID(ctx context.Context, callUniqueID string) error {
	return r.queries.HardDeletePhoneCallByUniqueID(ctx, callUniqueID)
}
