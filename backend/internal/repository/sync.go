package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SyncStatus represents the status of a sync source
type SyncStatus string

const (
	SyncStatusIdle     SyncStatus = "idle"
	SyncStatusSyncing  SyncStatus = "syncing"
	SyncStatusError    SyncStatus = "error"
	SyncStatusDisabled SyncStatus = "disabled"
)

// SyncStrategy represents how a source syncs data
type SyncStrategy string

const (
	SyncStrategyContactDriven SyncStrategy = "contact_driven"
	SyncStrategyFetchAll      SyncStrategy = "fetch_all"
	SyncStrategyFetchFiltered SyncStrategy = "fetch_filtered"
)

// SyncState represents the current state of a sync source
type SyncState struct {
	ID                   uuid.UUID      `json:"id"`
	Source               string         `json:"source"`
	AccountID            *string        `json:"account_id,omitempty"`
	Enabled              bool           `json:"enabled"`
	Status               SyncStatus     `json:"status"`
	Strategy             SyncStrategy   `json:"strategy"`
	LastSyncAt           *time.Time     `json:"last_sync_at,omitempty"`
	LastSuccessfulSyncAt *time.Time     `json:"last_successful_sync_at,omitempty"`
	NextSyncAt           *time.Time     `json:"next_sync_at,omitempty"`
	SyncCursor           *string        `json:"sync_cursor,omitempty"`
	ErrorMessage         *string        `json:"error_message,omitempty"`
	ErrorCount           int32          `json:"error_count"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// SyncLog represents a sync run audit log entry
type SyncLog struct {
	ID             uuid.UUID      `json:"id"`
	SyncStateID    uuid.UUID      `json:"sync_state_id"`
	Source         string         `json:"source"`
	AccountID      *string        `json:"account_id,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
	Status         string         `json:"status"`
	ItemsProcessed int32          `json:"items_processed"`
	ItemsMatched   int32          `json:"items_matched"`
	ItemsCreated   int32          `json:"items_created"`
	ErrorMessage   *string        `json:"error_message,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// CreateSyncStateRequest holds parameters for creating a sync state
type CreateSyncStateRequest struct {
	Source     string         `json:"source"`
	AccountID  *string        `json:"account_id,omitempty"`
	Enabled    bool           `json:"enabled"`
	Status     SyncStatus     `json:"status,omitempty"`
	Strategy   SyncStrategy   `json:"strategy,omitempty"`
	NextSyncAt *time.Time     `json:"next_sync_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// SyncRepository handles sync state and log persistence
type SyncRepository struct {
	queries db.Querier
}

// NewSyncRepository creates a new sync repository
func NewSyncRepository(queries db.Querier) *SyncRepository {
	return &SyncRepository{queries: queries}
}

// convertDbSyncState converts a database sync state to a repository sync state
func convertDbSyncState(dbState *db.ExternalSyncState) SyncState {
	state := SyncState{
		Source:     dbState.Source,
		Enabled:    dbState.Enabled,
		Status:     SyncStatus(dbState.Status),
		Strategy:   SyncStrategy(dbState.Strategy),
		ErrorCount: dbState.ErrorCount,
	}

	// Convert UUID
	if dbState.ID.Valid {
		state.ID = uuid.UUID(dbState.ID.Bytes)
	}

	// Convert timestamps
	if dbState.CreatedAt.Valid {
		state.CreatedAt = dbState.CreatedAt.Time
	}
	if dbState.UpdatedAt.Valid {
		state.UpdatedAt = dbState.UpdatedAt.Time
	}
	if dbState.LastSyncAt.Valid {
		state.LastSyncAt = &dbState.LastSyncAt.Time
	}
	if dbState.LastSuccessfulSyncAt.Valid {
		state.LastSuccessfulSyncAt = &dbState.LastSuccessfulSyncAt.Time
	}
	if dbState.NextSyncAt.Valid {
		state.NextSyncAt = &dbState.NextSyncAt.Time
	}

	// Convert nullable fields
	if dbState.AccountID.Valid {
		state.AccountID = &dbState.AccountID.String
	}
	if dbState.SyncCursor.Valid {
		state.SyncCursor = &dbState.SyncCursor.String
	}
	if dbState.ErrorMessage.Valid {
		state.ErrorMessage = &dbState.ErrorMessage.String
	}

	// Convert JSONB metadata
	if len(dbState.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbState.Metadata, &metadata); err == nil {
			state.Metadata = metadata
		}
	}

	return state
}

// convertDbSyncLog converts a database sync log to a repository sync log
func convertDbSyncLog(dbLog *db.ExternalSyncLog) SyncLog {
	log := SyncLog{
		Source: dbLog.Source,
		Status: dbLog.Status,
	}

	// Convert UUIDs
	if dbLog.ID.Valid {
		log.ID = uuid.UUID(dbLog.ID.Bytes)
	}
	if dbLog.SyncStateID.Valid {
		log.SyncStateID = uuid.UUID(dbLog.SyncStateID.Bytes)
	}

	// Convert timestamps
	if dbLog.StartedAt.Valid {
		log.StartedAt = dbLog.StartedAt.Time
	}
	if dbLog.CompletedAt.Valid {
		log.CompletedAt = &dbLog.CompletedAt.Time
	}
	if dbLog.CreatedAt.Valid {
		log.CreatedAt = dbLog.CreatedAt.Time
	}

	// Convert nullable int fields
	if dbLog.ItemsProcessed.Valid {
		log.ItemsProcessed = dbLog.ItemsProcessed.Int32
	}
	if dbLog.ItemsMatched.Valid {
		log.ItemsMatched = dbLog.ItemsMatched.Int32
	}
	if dbLog.ItemsCreated.Valid {
		log.ItemsCreated = dbLog.ItemsCreated.Int32
	}

	// Convert nullable fields
	if dbLog.AccountID.Valid {
		log.AccountID = &dbLog.AccountID.String
	}
	if dbLog.ErrorMessage.Valid {
		log.ErrorMessage = &dbLog.ErrorMessage.String
	}

	// Convert JSONB metadata
	if len(dbLog.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbLog.Metadata, &metadata); err == nil {
			log.Metadata = metadata
		}
	}

	return log
}

// GetSyncState retrieves a sync state by ID
func (r *SyncRepository) GetSyncState(ctx context.Context, id uuid.UUID) (*SyncState, error) {
	dbState, err := r.queries.GetSyncState(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// GetSyncStateBySource retrieves a sync state by source and account ID
func (r *SyncRepository) GetSyncStateBySource(ctx context.Context, source string, accountID *string) (*SyncState, error) {
	dbState, err := r.queries.GetSyncStateBySource(ctx, db.GetSyncStateBySourceParams{
		Source:    source,
		AccountID: stringToPgText(accountID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// ListSyncStates retrieves all sync states
func (r *SyncRepository) ListSyncStates(ctx context.Context) ([]SyncState, error) {
	dbStates, err := r.queries.ListSyncStates(ctx)
	if err != nil {
		return nil, err
	}

	states := make([]SyncState, len(dbStates))
	for i, dbState := range dbStates {
		states[i] = convertDbSyncState(dbState)
	}

	return states, nil
}

// ListEnabledSyncStates retrieves all enabled sync states
func (r *SyncRepository) ListEnabledSyncStates(ctx context.Context) ([]SyncState, error) {
	dbStates, err := r.queries.ListEnabledSyncStates(ctx)
	if err != nil {
		return nil, err
	}

	states := make([]SyncState, len(dbStates))
	for i, dbState := range dbStates {
		states[i] = convertDbSyncState(dbState)
	}

	return states, nil
}

// ListDueSyncStates retrieves sync states that are due for syncing
func (r *SyncRepository) ListDueSyncStates(ctx context.Context, now time.Time) ([]SyncState, error) {
	dbStates, err := r.queries.ListDueSyncStates(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if err != nil {
		return nil, err
	}

	states := make([]SyncState, len(dbStates))
	for i, dbState := range dbStates {
		states[i] = convertDbSyncState(dbState)
	}

	return states, nil
}

// CreateSyncState creates a new sync state
func (r *SyncRepository) CreateSyncState(ctx context.Context, req CreateSyncStateRequest) (*SyncState, error) {
	// Set defaults
	status := req.Status
	if status == "" {
		status = SyncStatusIdle
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = SyncStrategyContactDriven
	}

	// Convert metadata to JSON
	var metadataBytes []byte
	if req.Metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(req.Metadata)
		if err != nil {
			return nil, err
		}
	}

	dbState, err := r.queries.CreateSyncState(ctx, db.CreateSyncStateParams{
		Source:     req.Source,
		AccountID:  stringToPgText(req.AccountID),
		Enabled:    req.Enabled,
		Status:     string(status),
		Strategy:   string(strategy),
		NextSyncAt: timeToPgTimestamptz(req.NextSyncAt),
		Metadata:   metadataBytes,
	})
	if err != nil {
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// UpdateSyncStateStatus updates the status of a sync state
func (r *SyncRepository) UpdateSyncStateStatus(ctx context.Context, id uuid.UUID, status SyncStatus, errorMessage *string) (*SyncState, error) {
	dbState, err := r.queries.UpdateSyncStateStatus(ctx, db.UpdateSyncStateStatusParams{
		ID:           uuidToPgUUID(id),
		Status:       string(status),
		ErrorMessage: stringToPgText(errorMessage),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// UpdateSyncStateSuccess updates a sync state after a successful sync
func (r *SyncRepository) UpdateSyncStateSuccess(ctx context.Context, id uuid.UUID, nextSyncAt time.Time, cursor *string) (*SyncState, error) {
	dbState, err := r.queries.UpdateSyncStateSuccess(ctx, db.UpdateSyncStateSuccessParams{
		ID:         uuidToPgUUID(id),
		NextSyncAt: pgtype.Timestamptz{Time: nextSyncAt, Valid: true},
		SyncCursor: stringToPgText(cursor),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// UpdateSyncStateEnabled enables or disables a sync state
func (r *SyncRepository) UpdateSyncStateEnabled(ctx context.Context, id uuid.UUID, enabled bool) (*SyncState, error) {
	dbState, err := r.queries.UpdateSyncStateEnabled(ctx, db.UpdateSyncStateEnabledParams{
		ID:      uuidToPgUUID(id),
		Enabled: enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	state := convertDbSyncState(dbState)
	return &state, nil
}

// DeleteSyncState deletes a sync state
func (r *SyncRepository) DeleteSyncState(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteSyncState(ctx, uuidToPgUUID(id))
}

// CreateSyncLog creates a new sync log entry
func (r *SyncRepository) CreateSyncLog(ctx context.Context, state *SyncState) (*SyncLog, error) {
	// Convert metadata to JSON
	var metadataBytes []byte
	if state.Metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(state.Metadata)
		if err != nil {
			return nil, err
		}
	}

	dbLog, err := r.queries.CreateSyncLog(ctx, db.CreateSyncLogParams{
		SyncStateID: uuidToPgUUID(state.ID),
		Source:      state.Source,
		AccountID:   stringToPgText(state.AccountID),
		Metadata:    metadataBytes,
	})
	if err != nil {
		return nil, err
	}

	log := convertDbSyncLog(dbLog)
	return &log, nil
}

// CompleteSyncLogResult contains the result data for completing a sync log
type CompleteSyncLogResult struct {
	Status         string
	ItemsProcessed int32
	ItemsMatched   int32
	ItemsCreated   int32
	ErrorMessage   *string
}

// CompleteSyncLog completes a sync log entry
func (r *SyncRepository) CompleteSyncLog(ctx context.Context, logID uuid.UUID, result CompleteSyncLogResult) (*SyncLog, error) {
	dbLog, err := r.queries.CompleteSyncLog(ctx, db.CompleteSyncLogParams{
		ID:             uuidToPgUUID(logID),
		Status:         result.Status,
		ItemsProcessed: pgtype.Int4{Int32: result.ItemsProcessed, Valid: true},
		ItemsMatched:   pgtype.Int4{Int32: result.ItemsMatched, Valid: true},
		ItemsCreated:   pgtype.Int4{Int32: result.ItemsCreated, Valid: true},
		ErrorMessage:   stringToPgText(result.ErrorMessage),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	log := convertDbSyncLog(dbLog)
	return &log, nil
}

// GetSyncLog retrieves a sync log by ID
func (r *SyncRepository) GetSyncLog(ctx context.Context, id uuid.UUID) (*SyncLog, error) {
	dbLog, err := r.queries.GetSyncLog(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	log := convertDbSyncLog(dbLog)
	return &log, nil
}

// ListSyncLogsByState retrieves sync logs for a specific state
func (r *SyncRepository) ListSyncLogsByState(ctx context.Context, stateID uuid.UUID, limit, offset int32) ([]SyncLog, error) {
	dbLogs, err := r.queries.ListSyncLogsByState(ctx, db.ListSyncLogsByStateParams{
		SyncStateID: uuidToPgUUID(stateID),
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		return nil, err
	}

	logs := make([]SyncLog, len(dbLogs))
	for i, dbLog := range dbLogs {
		logs[i] = convertDbSyncLog(dbLog)
	}

	return logs, nil
}

// ListRecentSyncLogs retrieves the most recent sync logs
func (r *SyncRepository) ListRecentSyncLogs(ctx context.Context, limit int32) ([]SyncLog, error) {
	dbLogs, err := r.queries.ListRecentSyncLogs(ctx, limit)
	if err != nil {
		return nil, err
	}

	logs := make([]SyncLog, len(dbLogs))
	for i, dbLog := range dbLogs {
		logs[i] = convertDbSyncLog(dbLog)
	}

	return logs, nil
}

// CountSyncLogsByState returns the count of sync logs for a specific state
func (r *SyncRepository) CountSyncLogsByState(ctx context.Context, stateID uuid.UUID) (int64, error) {
	return r.queries.CountSyncLogsByState(ctx, uuidToPgUUID(stateID))
}

// DeleteOldSyncLogs deletes sync logs older than the given time
func (r *SyncRepository) DeleteOldSyncLogs(ctx context.Context, before time.Time) error {
	return r.queries.DeleteOldSyncLogs(ctx, pgtype.Timestamptz{Time: before, Valid: true})
}
