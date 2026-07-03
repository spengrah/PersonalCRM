package jobsample

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// SampleTrimmer is the narrow interface TrimWorker needs. In production this is
// *repository.JobSampleRepository.
type SampleTrimmer interface {
	TrimJobExecSamples(ctx context.Context, cutoff time.Time) (int64, error)
}

// TrimWorker periodically deletes job_exec_sample rows older than the retention
// window. Modeled on scheduler.PairingTokenJanitorWorker (stateless worker over
// a bounded DELETE). Registered as a daily periodic job in
// cmd/crm-api/wire_jobsample.go.
type TrimWorker struct {
	river.WorkerDefaults[TrimArgs]
	repo          SampleTrimmer
	retentionDays int
}

// NewTrimWorker constructs a TrimWorker over the given repository, retaining
// retentionDays of samples.
func NewTrimWorker(repo SampleTrimmer, retentionDays int) *TrimWorker {
	return &TrimWorker{repo: repo, retentionDays: retentionDays}
}

// Work deletes rows whose created_at is older than the retention cutoff. The
// cutoff is computed in Go from accelerated time (NOT SQL NOW()) so trim
// retention stays correct under time acceleration and on the same clock as the
// created_at written at insert.
func (w *TrimWorker) Work(ctx context.Context, _ *river.Job[TrimArgs]) error {
	cutoff := accelerated.GetCurrentTime().AddDate(0, 0, -w.retentionDays)
	n, err := w.repo.TrimJobExecSamples(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("trim job exec samples: %w", err)
	}
	if n > 0 {
		logger.Info().Int64("deleted", n).Int("retention_days", w.retentionDays).
			Msg("job_sample_trim: removed old job_exec_sample rows")
	}
	return nil
}

// Timeout caps each invocation. The DELETE is bounded (a single-user Pi accrues
// at most a few thousand rows over the retention window), so 30s is generous;
// the limit prevents a pathological lock-wait from hanging a worker.
func (*TrimWorker) Timeout(*river.Job[TrimArgs]) time.Duration {
	return 30 * time.Second
}
