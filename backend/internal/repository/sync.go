package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// SyncStatus represents the status of a sync source. The legacy
// 'syncing' status value is no longer written by any Go code path —
// river_job state (available / running / completed / retryable) is the
// source of truth for "in-flight". The CHECK value remains in the
// schema and the ListDueSyncStates query still tolerates it on read so
// any historical row surfaces on the next scheduler tick. New code
// MUST NOT write 'syncing'.
type SyncStatus string

const (
	SyncStatusIdle     SyncStatus = "idle"
	SyncStatusError    SyncStatus = "error"
	SyncStatusDisabled SyncStatus = "disabled"
)

// DueAccount identifies a (source, account_id) pair whose sync is due.
// Returned by ListDueAccounts; consumed by the scheduler tick worker.
// Lives in the repository package so the worker package can depend on
// it without the repository depending on the worker (which would cause
// an import cycle).
type DueAccount struct {
	Source    string
	AccountID *string
}

// JobEnqueuer is the minimal *river.Client surface the repository needs
// to enqueue a job within a caller-supplied pgx.Tx. *river.Client[pgx.Tx]
// satisfies this via its InsertTx method. Tests can substitute a fake.
//
// Note: the repository does NOT construct concrete job-arg types. It
// accepts an opaque river.JobArgs value from the caller (see
// EnqueueAccountSyncIfNotInFlight's signature). That keeps the
// worker/args package a top-level dependency — the repository doesn't
// import scheduler.
type JobEnqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// SyncStrategy represents how a source syncs data
type SyncStrategy string

const (
	SyncStrategyContactDriven SyncStrategy = "contact_driven"
	SyncStrategyFetchAll      SyncStrategy = "fetch_all"
	SyncStrategyFetchFiltered SyncStrategy = "fetch_filtered"
	// SyncStrategyPush is used by Mac-daemon push providers (Messages,
	// iCloud Contacts; readers ship in later PRs). Push-strategy rows are
	// not polled by the scheduler — the service layer skips them at the
	// ListDueAccounts boundary.
	SyncStrategyPush SyncStrategy = "push"
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
	// pool is only required by repository methods that need to open an
	// explicit pgx.Tx (currently EnqueueAccountSyncIfNotInFlight). It is
	// populated by NewSyncRepositoryWithPool; NewSyncRepository keeps it
	// nil for call sites that don't need the transactional helpers.
	pool *pgxpool.Pool
}

// NewSyncRepository creates a new sync repository without a pool. Callers
// that need EnqueueAccountSyncIfNotInFlight must use
// NewSyncRepositoryWithPool instead.
func NewSyncRepository(queries db.Querier) *SyncRepository {
	return &SyncRepository{queries: queries}
}

// NewSyncRepositoryWithPool creates a new sync repository wired with a
// pgxpool reference so transactional helpers (e.g.,
// EnqueueAccountSyncIfNotInFlight) can open a pgx.Tx on the shared pool.
func NewSyncRepositoryWithPool(queries db.Querier, pool *pgxpool.Pool) *SyncRepository {
	return &SyncRepository{queries: queries, pool: pool}
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

// DeleteSyncStatesByAccountID deletes all sync states for a specific account
func (r *SyncRepository) DeleteSyncStatesByAccountID(ctx context.Context, accountID string) error {
	return r.queries.DeleteSyncStatesByAccountID(ctx, pgtype.Text{String: accountID, Valid: true})
}

// UpdateSyncStateMetadata updates just the metadata field of a sync state
func (r *SyncRepository) UpdateSyncStateMetadata(ctx context.Context, id uuid.UUID, metadata map[string]any) (*SyncState, error) {
	var metadataBytes []byte
	if metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
	}

	dbState, err := r.queries.UpdateSyncStateMetadata(ctx, db.UpdateSyncStateMetadataParams{
		ID:       uuidToPgUUID(id),
		Metadata: metadataBytes,
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

// ListDueAccounts returns the (source, account_id) pairs of sync states
// whose next_sync_at has come due. Thin wrapper over ListDueSyncStates
// for callers (the tick worker) that only need the dispatch keys.
// Filtering for registered providers is done at the service layer (see
// service.SyncService.ListDueAccounts) so this repo method stays pure
// DB access.
func (r *SyncRepository) ListDueAccounts(ctx context.Context, now time.Time) ([]DueAccount, error) {
	states, err := r.ListDueSyncStates(ctx, now)
	if err != nil {
		return nil, err
	}
	accounts := make([]DueAccount, len(states))
	for i, st := range states {
		accounts[i] = DueAccount{Source: st.Source, AccountID: st.AccountID}
	}
	return accounts, nil
}

// AbandonRunningLogsForState marks any pre-existing 'running' log rows for
// this sync_state as 'abandoned' with a fixed explanatory error_message.
// Called at the start of a retry attempt so that orphan rows from a prior
// crashed run don't accumulate. Requires migration 037.
func (r *SyncRepository) AbandonRunningLogsForState(ctx context.Context, stateID uuid.UUID) error {
	return r.queries.AbandonRunningLogsForState(ctx, uuidToPgUUID(stateID))
}

// EnqueueAccountSyncIfNotInFlight atomically claims-and-enqueues the
// caller-supplied job args for (source, accountID) iff no in-flight job
// for the same pair exists. Returns (enqueued=true, nil) when a job was
// inserted, (enqueued=false, nil) when a duplicate was skipped, and
// (false, err) on infrastructure errors.
//
// The `args` parameter is opaque river.JobArgs — typically a
// scheduler.SyncProviderAccountArgs, but the repository does not import
// scheduler (to keep Handler → Service → Repository layering clean).
// Callers (service.SyncService) are responsible for constructing a
// JobArgs whose JSON shape matches the CountInFlightSyncJobs dedup
// query's expectations (keys `source` and `account_id`).
//
// Atomicity story (see .ai/log/plan/event-bus-foundation-pr3-scheduler-river.md DD 1):
//  1. Begin a pgx.Tx on the shared pool.
//  2. Acquire a per-account advisory lock via
//     pg_advisory_xact_lock(hashtextextended($1||'|'||COALESCE($2,”), 0)).
//     The lock releases at transaction end.
//  3. Inside the lock, CountInFlightSyncJobs checks river_job for a live
//     job with matching (source, account_id) JSONB args.
//  4. If count>0, rollback and return (false, nil).
//  5. If count==0, InsertTx the new job and commit.
//
// Concurrent callers for the same (source, account_id) serialize on the
// advisory lock, so the pre-check cannot race with a concurrent insert.
// Different accounts enqueue in parallel.
func (r *SyncRepository) EnqueueAccountSyncIfNotInFlight(
	ctx context.Context,
	enqueuer JobEnqueuer,
	source string,
	accountID *string,
	args river.JobArgs,
) (bool, error) {
	if r.pool == nil {
		return false, errors.New("sync repository was not constructed with a pool; use NewSyncRepositoryWithPool")
	}
	if enqueuer == nil {
		return false, errors.New("nil JobEnqueuer passed to EnqueueAccountSyncIfNotInFlight")
	}
	// Validate args upfront, BEFORE opening a tx or acquiring the advisory
	// lock. In production args is never nil (the service always constructs
	// SyncProviderAccountArgs{...}), but the fail-fast guard belongs
	// outside the critical section.
	if args == nil {
		return false, errors.New("nil args passed to EnqueueAccountSyncIfNotInFlight")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin enqueue tx: %w", err)
	}
	// defer Rollback is safe even after a successful Commit per pgx docs.
	defer func() { _ = tx.Rollback(ctx) }()

	// Acquire the per-account advisory lock. The key is a stable string so
	// two concurrent callers for the same (source, account_id) hash to the
	// same bigint and serialize. COALESCE handles nil accountID.
	acct := ""
	if accountID != nil {
		acct = *accountID
	}
	lockKey := source + "|" + acct
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		lockKey,
	); err != nil {
		return false, fmt.Errorf("acquire enqueue advisory lock: %w", err)
	}

	// In-flight check against river_job, scoped to the same tx so the
	// advisory lock protects it from races. db.New(tx) wraps the pgx.Tx
	// as a sqlc Queries so the check runs inside the enqueue tx.
	cnt, err := db.New(tx).CountInFlightSyncJobs(ctx, db.CountInFlightSyncJobsParams{
		Source:    source,
		AccountID: stringToPgText(accountID),
	})
	if err != nil {
		return false, fmt.Errorf("count in-flight sync jobs: %w", err)
	}
	if cnt > 0 {
		// Duplicate skip. Commit the tx so the advisory lock releases
		// promptly (Rollback would also release it, but Commit is slightly
		// cheaper and makes the "no-op happy path" explicit).
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit enqueue dedup tx: %w", err)
		}
		return false, nil
	}

	if _, err := enqueuer.InsertTx(ctx, tx, args, nil); err != nil {
		return false, fmt.Errorf("insert sync_provider_account job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit enqueue tx: %w", err)
	}
	return true, nil
}

// Mac-daemon push-cursor methods.
//
// Push providers (Mac daemon's Messages, iCloud Contacts) commit their
// cursor via the daemon-side endpoints rather than the scheduler-tick
// loop. Cursors live in external_sync_state — one row per
// (source, account_id) where account_id is the mac_host.id stringified.
// cursor_epoch lives on mac_host (per-host concept; bumped on Pi
// restore) and is read in the same tx as the cursor commit with a row
// lock on mac_host.

// ErrCursorEpochMismatch is returned by CommitMacHostCursor when the
// caller-supplied epoch differs from the host's current cursor_epoch.
// The handler maps this to 409 with both current_epoch and
// current_cursor in the body so the daemon can fully reconcile its
// local cache in a single response.
type ErrCursorEpochMismatch struct {
	ServerEpoch   int64
	CurrentCursor string
}

func (e *ErrCursorEpochMismatch) Error() string {
	return fmt.Sprintf("cursor epoch mismatch: server has %d", e.ServerEpoch)
}

// ErrCursorBaseMismatch is returned by CommitMacHostCursor when the
// caller-supplied base_cursor differs from the cursor currently stored
// on the row (or when the daemon claims no row exists but one does).
// The handler maps this to 409 with both current_cursor and
// current_epoch in the body so the daemon can rebase against the
// server's value.
type ErrCursorBaseMismatch struct {
	CurrentCursor string
	CurrentEpoch  int64
}

func (e *ErrCursorBaseMismatch) Error() string {
	return fmt.Sprintf("cursor base mismatch: server has %q", e.CurrentCursor)
}

// GetMacHostSyncCursor returns the cursor stored for (source, hostID).
// Returns db.ErrNotFound if no row has been committed yet — handler
// treats that as an empty cursor.
func (r *SyncRepository) GetMacHostSyncCursor(ctx context.Context, source string, hostID uuid.UUID) (string, error) {
	row, err := r.queries.GetMacHostSyncState(ctx, db.GetMacHostSyncStateParams{
		Source:    source,
		AccountID: hostID.String(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", db.ErrNotFound
		}
		return "", fmt.Errorf("get mac_host sync cursor: %w", err)
	}
	if !row.SyncCursor.Valid {
		return "", nil
	}
	return row.SyncCursor.String, nil
}

// CommitMacHostCursorParams carries the inputs for the three-stage
// transactional CAS implemented by CommitMacHostCursor.
type CommitMacHostCursorParams struct {
	HostID       uuid.UUID
	Source       string
	BaseCursor   string // caller's snapshot of the cursor before this commit
	NewCursor    string // cursor to install
	ClaimedEpoch int64  // daemon's local cursor_epoch
}

// CommitMacHostCursor performs the three-stage transactional CAS for
// committing a push-strategy cursor:
//
//	Stage A: lock mac_host row, read host_epoch + revocation status,
//	         read the existing cursor row (if any). Branch on
//	         (epoch match, row presence, base match).
//	Stage B: INSERT path when no cursor row exists. Uses ON CONFLICT
//	         DO NOTHING to handle the concurrent-first-write race —
//	         loser re-reads inside the same tx and surfaces 409.
//	Stage C: UPDATE path when a cursor row exists. Predicate-conditional
//	         update with sync_cursor = base_cursor; zero rows returned
//	         means a concurrent writer slipped in.
//
// Returns:
//
//	nil                              — commit succeeded
//	db.ErrNotFound                   — host id missing or revoked
//	*ErrCursorEpochMismatch          — epoch mismatch
//	*ErrCursorBaseMismatch           — base mismatch (or first-write race)
//	other errors                     — wrapped DB errors
func (r *SyncRepository) CommitMacHostCursor(ctx context.Context, params CommitMacHostCursorParams) error {
	if r.pool == nil {
		return errors.New("CommitMacHostCursor requires a pool; use NewSyncRepositoryWithPool")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin commit-cursor tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)

	// Stage A: lock host + read epoch.
	epochRow, err := q.GetMacHostCursorEpoch(ctx, uuidToPgUUID(params.HostID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ErrNotFound
		}
		return fmt.Errorf("read host cursor_epoch: %w", err)
	}
	if epochRow.ApiKeyRevokedAt.Valid {
		// Revoked between auth check and commit. The middleware would
		// have caught this at the next request, but defending in depth
		// inside the tx prevents committing a cursor for a revoked host.
		return db.ErrNotFound
	}
	serverEpoch := epochRow.CursorEpoch

	// Stage A: read the existing cursor row (if any) — needed for both
	// the happy path (Stage C base check) AND the error path (so 409
	// responses can include the current_cursor alongside current_epoch).
	existing, err := q.GetMacHostSyncState(ctx, db.GetMacHostSyncStateParams{
		Source:    params.Source,
		AccountID: params.HostID.String(),
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read mac_host cursor row: %w", err)
	}
	currentCursor := ""
	cursorRowFound := err == nil
	if cursorRowFound && existing.SyncCursor.Valid {
		currentCursor = existing.SyncCursor.String
	}

	// Epoch check AFTER the cursor read so the 409 body can include both.
	if serverEpoch != params.ClaimedEpoch {
		return &ErrCursorEpochMismatch{
			ServerEpoch:   serverEpoch,
			CurrentCursor: currentCursor,
		}
	}

	if !cursorRowFound {
		// No existing row — daemon must have claimed base_cursor="".
		if params.BaseCursor != "" {
			return &ErrCursorBaseMismatch{
				CurrentCursor: "",
				CurrentEpoch:  serverEpoch,
			}
		}

		// Stage B: INSERT path. ON CONFLICT DO NOTHING handles the
		// concurrent-first-write race without aborting the tx (no
		// 23505 raised).
		inserted, insErr := q.InsertMacHostSyncCursor(ctx, db.InsertMacHostSyncCursorParams{
			Source:    params.Source,
			AccountID: pgtype.Text{String: params.HostID.String(), Valid: true},
			NewCursor: pgtype.Text{String: params.NewCursor, Valid: true},
		})
		if insErr != nil && !errors.Is(insErr, pgx.ErrNoRows) {
			return fmt.Errorf("insert mac_host cursor row: %w", insErr)
		}
		if insErr != nil || inserted == nil {
			// Concurrent writer won the first-insert race. Re-read to
			// surface the winner's cursor. The re-read uses the same
			// READ COMMITTED tx; the concurrent writer has committed
			// — its row is visible to us now.
			current, reReadErr := readCursorForConflict(ctx, q, params)
			if reReadErr != nil {
				return reReadErr
			}
			return &ErrCursorBaseMismatch{
				CurrentCursor: current,
				CurrentEpoch:  serverEpoch,
			}
		}
		return tx.Commit(ctx)
	}

	// Existing row — Stage C: predicate-conditional UPDATE.
	if currentCursor != params.BaseCursor {
		return &ErrCursorBaseMismatch{
			CurrentCursor: currentCursor,
			CurrentEpoch:  serverEpoch,
		}
	}

	updated, err := q.UpdateMacHostSyncCursor(ctx, db.UpdateMacHostSyncCursorParams{
		NewCursor:  pgtype.Text{String: params.NewCursor, Valid: true},
		ID:         existing.ID,
		BaseCursor: params.BaseCursor,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("update mac_host cursor: %w", err)
	}
	if err != nil || updated == nil {
		// Zero rows updated: another writer slipped in between Stage A's
		// read and our UPDATE. Re-read and surface 409 with the latest.
		current, reReadErr := readCursorForConflict(ctx, q, params)
		if reReadErr != nil {
			return reReadErr
		}
		return &ErrCursorBaseMismatch{
			CurrentCursor: current,
			CurrentEpoch:  serverEpoch,
		}
	}
	return tx.Commit(ctx)
}

// readCursorForConflict re-reads the cursor row inside the same tx
// after a race-loss and returns the current cursor string (or "" when
// no row is present). Surfaces only conflict-readable values.
func readCursorForConflict(ctx context.Context, q db.Querier, params CommitMacHostCursorParams) (string, error) {
	latest, err := q.GetMacHostSyncState(ctx, db.GetMacHostSyncStateParams{
		Source:    params.Source,
		AccountID: params.HostID.String(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("re-read mac_host cursor after CAS miss: %w", err)
	}
	if !latest.SyncCursor.Valid {
		return "", nil
	}
	return latest.SyncCursor.String, nil
}

// DeleteMacHostSyncStatesTx removes all push-strategy external_sync_state
// rows for the given host inside the caller-supplied tx. Used by the
// delete-host handler so revoke + cursor-cleanup are atomic.
func (r *SyncRepository) DeleteMacHostSyncStatesTx(ctx context.Context, tx pgx.Tx, hostID uuid.UUID) (int64, error) {
	n, err := db.New(tx).DeleteMacHostSyncStates(ctx, hostID.String())
	if err != nil {
		return 0, fmt.Errorf("delete mac_host sync states: %w", err)
	}
	return n, nil
}
