package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
)

// ErrAccountRequired is returned by TriggerSync when an account-scoped
// provider (Config().RequiresAccount) is triggered with a nil/empty account
// ID. Bootstrapping a sync_state row here would create a permanently-erroring
// row, so we reject instead. Handlers can map it to a 4xx via errors.Is.
var ErrAccountRequired = errors.New("account ID required for this source")

// AccountIDMissing reports whether an account ID is absent for the purpose of
// account-scoped provider validation: nil, empty, or whitespace-only. A
// padded-but-nonempty ID (e.g. " acct ") is intentionally NOT treated as
// missing — normalizing caller input is out of scope here. Exported so the
// sync HTTP handler shares this single definition.
func AccountIDMissing(accountID *string) bool {
	return accountID == nil || strings.TrimSpace(*accountID) == ""
}

// Backoff intervals for error retries (exponential backoff)
var backoffIntervals = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
}

// SyncService handles external data sync operations
type SyncService struct {
	syncRepo    *repository.SyncRepository
	contactRepo *repository.ContactRepository
	registry    *sync.ProviderRegistry
	// enqueuer is set at boot by SetRiverEnqueuer. When non-nil, TriggerSync
	// dispatches via a river job instead of running the sync inline. When
	// nil (e.g., early-boot or tests that don't construct a river client)
	// TriggerSync falls back to the synchronous runSyncForState path.
	enqueuer repository.JobEnqueuer
	// emailAccountLister is set at boot by SetEmailAccountLister (cutover
	// only). When nil (tests / no-Google builds / off-mode)
	// ReconcileEmailSyncStates is a no-op. See email_reconcile.go.
	emailAccountLister GoogleAccountLister
	// gchatAccountLister is set at boot by SetGChatAccountLister. When nil
	// (tests / no-Google builds) ReconcileGChatSyncStates is a no-op. A
	// dedicated field (vs. reusing emailAccountLister) keeps gchat enablement
	// independently nil-gated. See gchat_reconcile.go.
	gchatAccountLister GoogleAccountLister
}

// NewSyncService creates a new sync service
func NewSyncService(
	syncRepo *repository.SyncRepository,
	contactRepo *repository.ContactRepository,
	registry *sync.ProviderRegistry,
) *SyncService {
	return &SyncService{
		syncRepo:    syncRepo,
		contactRepo: contactRepo,
		registry:    registry,
	}
}

// SetRiverEnqueuer stores the river client (as a JobEnqueuer interface
// value) that TriggerSync and EnqueueAccountSyncIfNotInFlight will use to
// dispatch jobs. *river.Client[pgx.Tx] satisfies JobEnqueuer via its
// InsertTx method. Safe to call once at boot; not safe to call
// concurrently with in-flight TriggerSync requests.
func (s *SyncService) SetRiverEnqueuer(e repository.JobEnqueuer) {
	s.enqueuer = e
}

// EnqueueAccountSyncIfNotInFlight delegates to the repository's
// atomic-claim transaction. See repository.EnqueueAccountSyncIfNotInFlight
// for the correctness argument. The service is the only layer that
// imports scheduler — it constructs SyncProviderAccountArgs here and
// passes it to the repo as opaque river.JobArgs so the repo stays free
// of the scheduler package.
func (s *SyncService) EnqueueAccountSyncIfNotInFlight(
	ctx context.Context, source string, accountID *string,
) (bool, error) {
	if s.enqueuer == nil {
		return false, errors.New("sync service has no river enqueuer; wiring incomplete")
	}
	args := scheduler.SyncProviderAccountArgs{Source: source, AccountID: accountID}
	return s.syncRepo.EnqueueAccountSyncIfNotInFlight(ctx, s.enqueuer, source, accountID, args)
}

// ListDueAccounts returns the (source, account_id) pairs of due sync
// states. Called by the SchedulerTickWorker to enumerate enqueue targets.
// Returns []repository.DueAccount so the service directly satisfies
// scheduler.SyncServiceForTick without requiring an adapter (and the
// scheduler package imports repository, not the reverse).
//
// Rows whose `source` is not in the provider registry are filtered out
// here so the tick does not enqueue poison jobs: without this filter, a
// stale external_sync_state row for an unconfigured provider (e.g.
// OAuth was removed) would cycle enqueue → worker-fail-unknown-source
// → river discard → next tick, burning retry budgets forever.
// Operators can disable or delete those rows explicitly; the scheduler
// does not touch them.
func (s *SyncService) ListDueAccounts(ctx context.Context) ([]repository.DueAccount, error) {
	now := accelerated.GetCurrentTime()
	all, err := s.syncRepo.ListDueAccounts(ctx, now)
	if err != nil {
		return nil, err
	}
	accounts := make([]repository.DueAccount, 0, len(all))
	for _, acct := range all {
		provider, ok := s.registry.Get(acct.Source)
		if !ok {
			logger.Debug().
				Str("source", acct.Source).
				Msg("scheduler tick: no provider registered; skipping due account")
			continue
		}
		// Push-strategy providers (Mac daemon's Messages, iCloud Contacts;
		// readers ship in later PRs) commit cursors via daemon-side
		// endpoints rather than the scheduler tick. Skip them so a stale
		// next_sync_at doesn't enqueue a no-op job whose worker would
		// have nothing to do.
		if provider.Config().Strategy == repository.SyncStrategyPush {
			logger.Debug().
				Str("source", acct.Source).
				Msg("scheduler tick: push-strategy provider; skipping due account")
			continue
		}
		accounts = append(accounts, acct)
	}
	return accounts, nil
}

// TriggerSync initiates a sync for a specific source/account. When the
// river enqueuer is wired (production path), TriggerSync enqueues a
// SyncProviderAccountJob and returns. If a job for the same
// (source, account_id) is already in-flight, TriggerSync is an idempotent
// no-op and returns nil. When the enqueuer is nil (tests / early boot),
// TriggerSync falls back to the synchronous runSyncForState path.
func (s *SyncService) TriggerSync(ctx context.Context, source string, accountID *string) error {
	// Get provider
	provider, ok := s.registry.Get(source)
	if !ok {
		return fmt.Errorf("unknown sync source: %s", source)
	}

	// Reject account-scoped providers triggered without an account, before we
	// would otherwise bootstrap a permanently-erroring sync_state row.
	if provider.Config().RequiresAccount && AccountIDMissing(accountID) {
		return fmt.Errorf("%w: source %q requires an account ID", ErrAccountRequired, source)
	}

	// Get or create sync state. The state itself is not needed on the
	// enqueue path (the worker will re-fetch fresh state), but we preserve
	// the "create on first trigger" behavior from the pre-PR-3 flow so
	// that callers who rely on TriggerSync to bootstrap a new sync_state
	// keep working unchanged.
	//
	// Distinguish "state not found" (benign — create a fresh row) from a
	// transient DB error (surface so the caller can retry) per .ai/rules/
	// core.md. Pre-PR-3 this check was a blanket `err != nil`, which could
	// mask a transient failure as a create attempt (and then fail with a
	// unique-constraint error from the unique index on (source, account_id)).
	state, err := s.syncRepo.GetSyncStateBySource(ctx, source, accountID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("get sync state: %w", err)
	}
	if errors.Is(err, db.ErrNotFound) {
		config := provider.Config()
		state, err = s.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    source,
			AccountID: accountID,
			Enabled:   true,
			Strategy:  config.Strategy,
		})
		if err != nil {
			return fmt.Errorf("create sync state: %w", err)
		}
		logger.Info().
			Str("source", source).
			Msg("created new sync state")
	}

	// Enqueue-first path. If the enqueuer is wired, dispatch via river;
	// otherwise fall back to synchronous sync.
	if s.enqueuer != nil {
		enqueued, err := s.EnqueueAccountSyncIfNotInFlight(ctx, source, accountID)
		if err != nil {
			return fmt.Errorf("enqueue sync job: %w", err)
		}
		if !enqueued {
			logger.Info().
				Str("source", source).
				Msg("sync already in-flight; enqueue skipped")
		}
		return nil
	}

	// Fallback: synchronous execution (tests / pre-wiring boot). This
	// path also used to return an error when state.Status == syncing; that
	// early-return is gone — river_job state is now the source of truth
	// for in-flight.
	return s.runSyncForState(ctx, state, provider)
}

// RunAccountSync fetches fresh sync state for (source, accountID) and
// runs the sync pipeline. Called by the SyncProviderAccountWorker.
//
// db.ErrNotFound is returned as a terminal nil-err (the worker treats it
// as a non-retryable outcome — a deleted sync_state should not keep
// retrying).
func (s *SyncService) RunAccountSync(ctx context.Context, source string, accountID *string) error {
	provider, ok := s.registry.Get(source)
	if !ok {
		// Terminal: a job was enqueued for a source that has since been
		// unregistered (e.g., operator revoked OAuth between the tick
		// enqueue and the worker fetch). Returning nil prevents river
		// from retrying — next tick will skip this account via
		// ListDueAccounts' provider filter.
		logger.Warn().
			Str("source", source).
			Msg("sync_provider_account: no provider registered; treating as terminal")
		return nil
	}

	state, err := s.syncRepo.GetSyncStateBySource(ctx, source, accountID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			logger.Warn().
				Str("source", source).
				Msg("sync state not found; skipping (terminal)")
			return nil
		}
		return fmt.Errorf("get sync state: %w", err)
	}

	return s.runSyncForState(ctx, state, provider)
}

// runSyncForState executes a sync operation for a given state and
// provider. River's job state (available/running/completed/retryable)
// is the source of truth for "in-flight"; no mutex write is needed.
// Prefixes a call to AbandonRunningLogsForState so that if a prior
// attempt crashed mid-sync, its orphan 'running' log row is marked
// 'abandoned' before the new attempt inserts a fresh log.
func (s *SyncService) runSyncForState(ctx context.Context, state *repository.SyncState, provider sync.SyncProvider) error {
	logger.Info().
		Str("source", state.Source).
		Str("status", string(state.Status)).
		Msg("starting sync")

	// Mark any pre-existing 'running' log rows for this sync_state as
	// 'abandoned'. A crashed prior attempt would have left one behind.
	// Non-fatal: log and proceed if the update fails — inserting a new
	// log row is still correct behavior.
	if err := s.syncRepo.AbandonRunningLogsForState(ctx, state.ID); err != nil {
		logger.Warn().
			Err(err).
			Str("sync_state_id", state.ID.String()).
			Msg("failed to abandon prior running log rows (non-fatal)")
	}

	// Create sync log
	logEntry, err := s.syncRepo.CreateSyncLog(ctx, state)
	if err != nil {
		// Revert status on log creation failure
		_, _ = s.syncRepo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusError, ptrString("failed to create sync log"))
		return fmt.Errorf("create sync log: %w", err)
	}

	// Get contacts for contact_driven strategy
	var contacts []repository.Contact
	config := provider.Config()
	if config.Strategy == repository.SyncStrategyContactDriven {
		contacts, err = s.contactRepo.ListContacts(ctx, repository.ListContactsParams{
			Limit:  10000,
			Offset: 0,
		})
		if err != nil {
			s.completeSyncWithError(ctx, state, logEntry.ID, "failed to list contacts", err)
			return fmt.Errorf("list contacts: %w", err)
		}
	}

	// Perform sync
	result, syncErr := provider.Sync(ctx, state, contacts)

	// Calculate next sync time
	now := accelerated.GetCurrentTime()
	var nextSync time.Time
	var newCursor *string
	var logStatus string

	if syncErr != nil {
		// Exponential backoff on error
		backoffIdx := int(state.ErrorCount)
		if backoffIdx >= len(backoffIntervals) {
			backoffIdx = len(backoffIntervals) - 1
		}
		nextSync = now.Add(backoffIntervals[backoffIdx])
		logStatus = "error"

		// Update state as error
		errMsg := syncErr.Error()
		if _, err := s.syncRepo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusError, &errMsg); err != nil {
			logger.Error().Err(err).Msg("failed to update sync state status to error")
		}

		logger.Error().
			Err(syncErr).
			Str("source", state.Source).
			Int("error_count", int(state.ErrorCount)+1).
			Time("next_sync", nextSync).
			Msg("sync failed")
	} else {
		// Success - use normal interval
		nextSync = now.Add(config.DefaultInterval)
		logStatus = "success"

		if result != nil && result.NewCursor != "" {
			newCursor = &result.NewCursor
		}

		// Update state as success
		if _, err := s.syncRepo.UpdateSyncStateSuccess(ctx, state.ID, nextSync, newCursor); err != nil {
			logger.Error().Err(err).Msg("failed to update sync state status to success")
		}

		logger.Info().
			Str("source", state.Source).
			Int("items_processed", result.ItemsProcessed).
			Int("items_matched", result.ItemsMatched).
			Int("items_created", result.ItemsCreated).
			Time("next_sync", nextSync).
			Msg("sync completed successfully")
	}

	// Complete log entry
	var itemsProcessed, itemsMatched, itemsCreated int32
	if result != nil {
		itemsProcessed = int32(result.ItemsProcessed)
		itemsMatched = int32(result.ItemsMatched)
		itemsCreated = int32(result.ItemsCreated)
	}

	var errMsgPtr *string
	if syncErr != nil {
		errMsg := syncErr.Error()
		errMsgPtr = &errMsg
	}

	if _, err := s.syncRepo.CompleteSyncLog(ctx, logEntry.ID, repository.CompleteSyncLogResult{
		Status:         logStatus,
		ItemsProcessed: itemsProcessed,
		ItemsMatched:   itemsMatched,
		ItemsCreated:   itemsCreated,
		ErrorMessage:   errMsgPtr,
	}); err != nil {
		logger.Error().Err(err).Msg("failed to complete sync log")
	}

	return syncErr
}

// completeSyncWithError is a helper to handle sync errors consistently
func (s *SyncService) completeSyncWithError(ctx context.Context, state *repository.SyncState, logID uuid.UUID, message string, err error) {
	errMsg := fmt.Sprintf("%s: %v", message, err)
	if _, updateErr := s.syncRepo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusError, &errMsg); updateErr != nil {
		logger.Error().Err(updateErr).Msg("failed to update sync state status")
	}
	if _, logErr := s.syncRepo.CompleteSyncLog(ctx, logID, repository.CompleteSyncLogResult{
		Status:       "error",
		ErrorMessage: &errMsg,
	}); logErr != nil {
		logger.Error().Err(logErr).Msg("failed to complete sync log")
	}
}

// GetSyncStatus returns sync status for all sources
func (s *SyncService) GetSyncStatus(ctx context.Context) ([]repository.SyncState, error) {
	return s.syncRepo.ListSyncStates(ctx)
}

// GetSyncStateBySource returns sync state for a specific source
func (s *SyncService) GetSyncStateBySource(ctx context.Context, source string, accountID *string) (*repository.SyncState, error) {
	return s.syncRepo.GetSyncStateBySource(ctx, source, accountID)
}

// GetSyncState returns sync state by ID
func (s *SyncService) GetSyncState(ctx context.Context, id uuid.UUID) (*repository.SyncState, error) {
	return s.syncRepo.GetSyncState(ctx, id)
}

// EnableSync enables/disables sync for a source
func (s *SyncService) EnableSync(ctx context.Context, id uuid.UUID, enabled bool) (*repository.SyncState, error) {
	return s.syncRepo.UpdateSyncStateEnabled(ctx, id, enabled)
}

// GetSyncLogs returns sync logs for a specific state
func (s *SyncService) GetSyncLogs(ctx context.Context, stateID uuid.UUID, limit, offset int32) ([]repository.SyncLog, error) {
	return s.syncRepo.ListSyncLogsByState(ctx, stateID, limit, offset)
}

// GetRecentSyncLogs returns the most recent sync logs across all sources
func (s *SyncService) GetRecentSyncLogs(ctx context.Context, limit int32) ([]repository.SyncLog, error) {
	return s.syncRepo.ListRecentSyncLogs(ctx, limit)
}

// GetAvailableProviders returns list of registered sync providers
func (s *SyncService) GetAvailableProviders() []sync.SourceConfig {
	return s.registry.List()
}

// CountSyncLogs returns the count of sync logs for a specific state
func (s *SyncService) CountSyncLogs(ctx context.Context, stateID uuid.UUID) (int64, error) {
	return s.syncRepo.CountSyncLogsByState(ctx, stateID)
}

// DeleteOldSyncLogs removes sync logs older than the specified duration
func (s *SyncService) DeleteOldSyncLogs(ctx context.Context, olderThan time.Duration) error {
	before := accelerated.GetCurrentTime().Add(-olderThan)
	return s.syncRepo.DeleteOldSyncLogs(ctx, before)
}

// Helper function to create a string pointer
func ptrString(s string) *string {
	return &s
}
