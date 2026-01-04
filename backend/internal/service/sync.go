package service

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
)

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

// TriggerSync initiates a sync for a specific source/account
func (s *SyncService) TriggerSync(ctx context.Context, source string, accountID *string) error {
	// Get provider
	provider, ok := s.registry.Get(source)
	if !ok {
		return fmt.Errorf("unknown sync source: %s", source)
	}

	// Get or create sync state
	state, err := s.syncRepo.GetSyncStateBySource(ctx, source, accountID)
	if err != nil {
		// Create new state if not found
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

	// Check if already syncing
	if state.Status == repository.SyncStatusSyncing {
		return fmt.Errorf("sync already in progress for source: %s", source)
	}

	// Perform sync
	return s.performSync(ctx, state, provider)
}

// RunDueSyncs checks for and runs all due syncs
func (s *SyncService) RunDueSyncs(ctx context.Context) error {
	now := accelerated.GetCurrentTime()

	states, err := s.syncRepo.ListDueSyncStates(ctx, now)
	if err != nil {
		return fmt.Errorf("list due sync states: %w", err)
	}

	if len(states) == 0 {
		logger.Debug().Msg("no due syncs found")
		return nil
	}

	logger.Info().
		Int("count", len(states)).
		Msg("found due syncs")

	var lastErr error
	for _, state := range states {
		provider, ok := s.registry.Get(state.Source)
		if !ok {
			logger.Warn().
				Str("source", state.Source).
				Msg("no provider registered for sync source")
			continue
		}

		stateCopy := state // Avoid loop variable capture
		if err := s.performSync(ctx, &stateCopy, provider); err != nil {
			logger.Error().
				Err(err).
				Str("source", state.Source).
				Msg("sync failed")
			lastErr = err
			// Continue with other syncs
		}
	}

	return lastErr
}

// performSync executes a sync operation for a given state and provider
func (s *SyncService) performSync(ctx context.Context, state *repository.SyncState, provider sync.SyncProvider) error {
	logger.Info().
		Str("source", state.Source).
		Str("status", string(state.Status)).
		Msg("starting sync")

	// Mark as syncing
	if _, err := s.syncRepo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatusSyncing, nil); err != nil {
		return fmt.Errorf("update sync status to syncing: %w", err)
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
