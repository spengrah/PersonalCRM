package scheduler

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// AssertionRollover is the narrow interface AssertionRolloverWorker needs. In
// production this is *service.AssertService.
type AssertionRollover interface {
	RunRollover(ctx context.Context) (int, error)
}

// AssertionRolloverWorker periodically terminalizes the bounded-with-pending-
// successor assertions whose valid_to has passed: the future-successor branch
// bounds a prior (valid_to=future, superseded_by=successor) but leaves it accepted
// until the date arrives, and no event fires at that future date — so this daily
// sweep flips just those rows to superseded and emits assertion.superseded.
//
// Scoped TIGHT (in the SQL): it touches ONLY rows with superseded_by NOT NULL, so
// a successor-less bounded-past historical fact is never terminalized. Stateless
// catch-up — a row already rolled over no longer matches, so re-running after
// downtime is safe and idempotent. Scheduled daily via the River periodic-job
// framework (see cmd/crm-api/main.go).
type AssertionRolloverWorker struct {
	river.WorkerDefaults[AssertionRolloverArgs]
	svc AssertionRollover
}

// NewAssertionRolloverWorker constructs the worker over the assert service.
func NewAssertionRolloverWorker(svc AssertionRollover) *AssertionRolloverWorker {
	return &AssertionRolloverWorker{svc: svc}
}

// Work implements river.Worker for AssertionRolloverArgs. Errors are wrapped and
// returned so River retries the tick.
func (w *AssertionRolloverWorker) Work(ctx context.Context, _ *river.Job[AssertionRolloverArgs]) error {
	n, err := w.svc.RunRollover(ctx)
	if err != nil {
		return fmt.Errorf("run assertion rollover: %w", err)
	}
	if n > 0 {
		logger.Info().Int("rolled_over", n).Msg("assertion rollover: terminalized bounded successors")
	}
	return nil
}

// Timeout caps each invocation. The sweep reads + updates a tight row set (the
// due-bounded-successor rows) and emits one event each; 60s is generous at
// single-user scale and prevents a pathological lock-wait from hanging a worker.
func (*AssertionRolloverWorker) Timeout(*river.Job[AssertionRolloverArgs]) time.Duration {
	return 60 * time.Second
}
