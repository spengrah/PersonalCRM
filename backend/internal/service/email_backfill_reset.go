package service

import (
	"context"
	"fmt"
	"strconv"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	syncpkg "personal-crm/backend/internal/sync"
)

// EmailBackfillCursorResetResult is the counts-only outcome for the operator
// reset. It intentionally carries no account identifiers or cursor values.
type EmailBackfillCursorResetResult struct {
	Scanned int
	Reset   int
}

// EmailBackfillCursorResetService rewinds enabled Gmail sync cursors so the
// corrected provider can re-scan from each account's backfill floor.
type EmailBackfillCursorResetService struct {
	syncRepo *repository.SyncRepository
}

func NewEmailBackfillCursorResetService(syncRepo *repository.SyncRepository) *EmailBackfillCursorResetService {
	return &EmailBackfillCursorResetService{syncRepo: syncRepo}
}

func (s *EmailBackfillCursorResetService) ResetGmailBackfillCursors(ctx context.Context) (EmailBackfillCursorResetResult, error) {
	if s.syncRepo == nil {
		return EmailBackfillCursorResetResult{}, fmt.Errorf("sync repository is required")
	}

	states, err := s.syncRepo.ListEnabledSyncStatesBySource(ctx, emailSyncSource)
	if err != nil {
		return EmailBackfillCursorResetResult{}, fmt.Errorf("list enabled email sync states: %w", err)
	}

	now := accelerated.GetCurrentTime()
	result := EmailBackfillCursorResetResult{Scanned: len(states)}
	for _, st := range states {
		cursor := strconv.FormatInt(emailBackfillFloorEpoch(st.Metadata), 10)
		if _, err := s.syncRepo.ResetSyncStateBackfillCursor(ctx, st.ID, cursor, now); err != nil {
			return result, fmt.Errorf("reset email sync cursor: %w", err)
		}
		result.Reset++
	}

	return result, nil
}

func emailBackfillFloorEpoch(metadata map[string]any) int64 {
	return syncpkg.EmailBackfillFloorEpoch(metadata)
}
