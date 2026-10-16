package langfuseexport

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// NightlyArgs are the args for the nightly periodic Worker. Stateless: each
// export finds what Langfuse lacks from the store and from Langfuse itself.
type NightlyArgs struct{}

// Kind implements river.JobArgs.
func (NightlyArgs) Kind() string { return "langfuse_export_nightly" }

// Worker runs one export a night. Registered as a daily periodic job in
// cmd/crm-api/wire_langfuse.go.
type Worker struct {
	river.WorkerDefaults[NightlyArgs]
	exporter *Exporter
}

// NewWorker constructs a Worker over the exporter.
func NewWorker(exporter *Exporter) *Worker {
	return &Worker{exporter: exporter}
}

// Work runs one export and logs its counts. With the extraction project's
// settings unset, it sends nothing and logs which settings are unset.
func (w *Worker) Work(ctx context.Context, _ *river.Job[NightlyArgs]) error {
	r, err := w.exporter.Export(ctx)
	if err != nil {
		return fmt.Errorf("langfuse export: %w", err)
	}
	if !r.Sent() {
		logger.Info().Strs("unset", r.Missing).
			Msg("langfuse_export: extraction project not configured; nothing sent")
		return nil
	}
	logger.Info().Int("runs", r.Runs).Int("judgments", r.Judgments).Int("already_sent", r.Held).
		Int("scorecards", r.Scorecards).Int("reviews", r.Reviews).
		Msg("langfuse_export: sent decision-model records")
	return nil
}

// Timeout caps each invocation. The first export after historical months are
// judged sends every judgment those runs made.
func (*Worker) Timeout(*river.Job[NightlyArgs]) time.Duration {
	return 10 * time.Minute
}
