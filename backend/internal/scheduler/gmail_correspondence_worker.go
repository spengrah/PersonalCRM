package scheduler

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// CorrespondenceScanner is the narrow producer surface the worker drives. In
// production this is *google.GmailCorrespondenceSuggester; keeping it an
// interface lets the worker unit test run without OAuth or a DB.
type CorrespondenceScanner interface {
	Run(ctx context.Context, since time.Time) (int, error)
}

// GmailCorrespondenceScanWorker periodically runs the correspondence-enrichment
// producer over the recent window (the steady-state scan). It computes
// `since = now - window` and delegates to the scanner. The scan is idempotent,
// so River retrying a failed tick is harmless.
//
// The window is injected (not imported from internal/google) because
// internal/google → internal/service → internal/scheduler, so this package
// cannot import internal/google without an import cycle. main.go supplies
// google.CorrespondenceWindow at construction time.
type GmailCorrespondenceScanWorker struct {
	river.WorkerDefaults[GmailCorrespondenceScanArgs]
	scanner CorrespondenceScanner
	window  time.Duration
}

// NewGmailCorrespondenceScanWorker constructs the worker over the given scanner
// and recent-window duration.
func NewGmailCorrespondenceScanWorker(scanner CorrespondenceScanner, window time.Duration) *GmailCorrespondenceScanWorker {
	return &GmailCorrespondenceScanWorker{scanner: scanner, window: window}
}

// Work implements river.Worker for GmailCorrespondenceScanArgs.
func (w *GmailCorrespondenceScanWorker) Work(ctx context.Context, _ *river.Job[GmailCorrespondenceScanArgs]) error {
	since := accelerated.GetCurrentTime().Add(-w.window)
	n, err := w.scanner.Run(ctx, since)
	if err != nil {
		return fmt.Errorf("gmail correspondence scan: %w", err)
	}
	if n > 0 {
		logger.Info().Int("candidates_upserted", n).Msg("gmail_correspondence_scan: tick complete")
	}
	return nil
}

// Timeout caps each invocation. The scan is bounded by the recent-window row
// count (small at current scale); 2 minutes is generous headroom.
func (*GmailCorrespondenceScanWorker) Timeout(*river.Job[GmailCorrespondenceScanArgs]) time.Duration {
	return 2 * time.Minute
}
