package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
)

// StalenessChecker is the narrow interface StalenessWatchdogWorker needs. In
// production this is *service.StalenessService.
type StalenessChecker interface {
	RunChecks(ctx context.Context) error
}

// StalenessWatchdogWorker periodically compares per-source freshness
// timestamps against config-backed thresholds and reconciles the resulting
// breach set into sync_staleness_breach. Scheduled every 5 minutes via the
// River periodic-job framework (see cmd/crm-api/main.go).
type StalenessWatchdogWorker struct {
	river.WorkerDefaults[StalenessWatchdogArgs]
	checker StalenessChecker
}

// NewStalenessWatchdogWorker constructs the worker over the given checker.
func NewStalenessWatchdogWorker(checker StalenessChecker) *StalenessWatchdogWorker {
	return &StalenessWatchdogWorker{checker: checker}
}

// Work implements river.Worker for StalenessWatchdogArgs. Errors are wrapped
// and returned so River retries the tick.
func (w *StalenessWatchdogWorker) Work(ctx context.Context, _ *river.Job[StalenessWatchdogArgs]) error {
	if err := w.checker.RunChecks(ctx); err != nil {
		return fmt.Errorf("run staleness checks: %w", err)
	}
	return nil
}

// Timeout caps each invocation. The watchdog reads a handful of small tables
// and writes a tiny breach set, so 30s is generous; the limit prevents a
// pathological lock-wait from hanging a worker indefinitely.
func (*StalenessWatchdogWorker) Timeout(*river.Job[StalenessWatchdogArgs]) time.Duration {
	return 30 * time.Second
}
