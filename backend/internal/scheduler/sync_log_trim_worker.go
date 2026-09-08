package scheduler

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// SyncLogTrimmer is the narrow interface SyncLogTrimWorker needs. In production
// this is *repository.SyncRepository.
type SyncLogTrimmer interface {
	DeleteOldSyncLogs(ctx context.Context, cutoff time.Time) (int64, error)
}

// SyncLogTrimWorker periodically deletes external_sync_log rows older than the
// retention window. Stateless worker over a bounded DELETE, modeled on
// jobsample.TrimWorker. Registered as a daily periodic job in
// cmd/crm-api/wire_synclog.go.
type SyncLogTrimWorker struct {
	river.WorkerDefaults[SyncLogTrimArgs]
	repo          SyncLogTrimmer
	retentionDays int
}

// NewSyncLogTrimWorker constructs a SyncLogTrimWorker over the given
// repository, retaining retentionDays of log rows.
func NewSyncLogTrimWorker(repo SyncLogTrimmer, retentionDays int) *SyncLogTrimWorker {
	return &SyncLogTrimWorker{repo: repo, retentionDays: retentionDays}
}

// Work deletes rows whose started_at is older than the retention cutoff. The
// cutoff is computed in Go from accelerated time (NOT SQL NOW()) so retention
// stays correct under time acceleration and on the same clock as the
// started_at written at insert.
func (w *SyncLogTrimWorker) Work(ctx context.Context, _ *river.Job[SyncLogTrimArgs]) error {
	cutoff := accelerated.GetCurrentTime().AddDate(0, 0, -w.retentionDays)
	n, err := w.repo.DeleteOldSyncLogs(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("trim sync logs: %w", err)
	}
	if n > 0 {
		logger.Info().Int64("deleted", n).Int("retention_days", w.retentionDays).
			Msg("sync_log_trim: removed old external_sync_log rows")
	}
	return nil
}

// Timeout caps each invocation. Steady state deletes a day's worth of rows (a
// few hundred), but the FIRST run on a database that has never been trimmed
// deletes every row older than the window at once — on the order of 100k rows
// on the Pi — and a timeout there aborts the whole DELETE, so the job would
// retry into the same wall forever. Two minutes covers that first sweep with
// margin while still bounding a pathological lock-wait.
func (*SyncLogTrimWorker) Timeout(*river.Job[SyncLogTrimArgs]) time.Duration {
	return 2 * time.Minute
}
