// Package langfuseexport sends the decision-model records the extraction
// spec allows (G15) to Langfuse's extraction project, and is the one code path
// that sends anything there. Every record it sends is built from the judgment
// store's export reads and the scorecard's per-run metrics, which hold opaque
// example IDs, run IDs, method keys and versions, question IDs, outcome
// values, counts, rates, usage and times, and never thread content, a thread
// ID or a contact ID.
//
// Each judgment is one trace, timestamped at its execution time and carrying
// its cut-off as metadata; each review is a score on its judgment's trace;
// each finished run is one trace with its summary; each finished rerun run's
// scorecard metrics are one trace. Every trace, span and score ID derives from
// the record it carries, so a record sent twice is one record in Langfuse, and
// a record sent again with changed values replaces the earlier ones.
package langfuseexport

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/scorecard"
)

// Store is the judgment store's content-free export reads.
// *judgment.Store satisfies it.
type Store interface {
	RunExports(ctx context.Context) ([]judgment.RunExport, error)
	ReviewRecords(ctx context.Context) ([]judgment.ReviewRecord, error)
}

// Metrics is the scorecard's per-run metrics. *scorecard.Reader satisfies it.
type Metrics interface {
	Metrics(ctx context.Context, run judgment.RunID) (scorecard.RunMetrics, error)
}

// Exporter sends the store's records to the extraction project.
type Exporter struct {
	store   Store
	metrics Metrics
	// client is nil when a setting of the extraction project is unset, so an
	// unconfigured exporter has nothing to send with.
	client  *client
	missing []string
}

// New builds an Exporter over the store's export reads and the scorecard's
// metrics. With any of the extraction project's settings unset, the exporter
// sends nothing.
func New(cfg config.LangfuseExtractionConfig, store Store, metrics Metrics) *Exporter {
	e := &Exporter{store: store, metrics: metrics}
	for _, s := range []struct{ name, value string }{
		{"LANGFUSE_EXTRACTION_HOST", cfg.Host},
		{"LANGFUSE_EXTRACTION_PUBLIC_KEY", cfg.PublicKey},
		{"LANGFUSE_EXTRACTION_SECRET_KEY", cfg.SecretKey},
	} {
		if s.value == "" {
			e.missing = append(e.missing, s.name)
		}
	}
	if len(e.missing) == 0 {
		e.client = &client{
			host:      cfg.Host,
			publicKey: cfg.PublicKey,
			secretKey: cfg.SecretKey,
			http:      &http.Client{Timeout: 30 * time.Second},
		}
	}
	return e
}

// Report is what one export did, content-free.
type Report struct {
	// Missing names the extraction project's unset settings; when any is
	// unset, the export sent nothing.
	Missing []string
	// Runs counts the finished runs sent; Skipped the runs whose judgments and
	// summary an earlier export already sent.
	Runs, Skipped int
	// Judgments, Scorecards and Reviews count the records sent.
	Judgments, Scorecards, Reviews int
}

// Sent reports whether the export reached Langfuse.
func (r Report) Sent() bool { return len(r.Missing) == 0 }

// Export sends every record made since the previous export, and every record
// derived from reviews, which can change after it was sent. Each run's
// judgments and summary go as one batch: the judgments its calls produced, in
// the order it recorded them, then, once it finished, its own trace. A batch
// is sent in order, so Langfuse holding its last record means an earlier
// export sent all of it, and the batch is skipped; a judgment a run records
// later, or the run finishing, changes that last record, so the next export
// sends the batch again. Judgments therefore reach Langfuse whether or not
// their run finishes. A finished rerun's scorecard metrics are computed from
// the latest reviews, and every live review is sent, on each export, so a
// changed verdict replaces the earlier verdict and the metrics it changed.
func (e *Exporter) Export(ctx context.Context) (Report, error) {
	if e.client == nil {
		return Report{Missing: e.missing}, nil
	}
	var report Report
	var scorecards []span
	runs, err := e.store.RunExports(ctx)
	if err != nil {
		return report, fmt.Errorf("read runs: %w", err)
	}
	for _, run := range runs {
		var spans []span
		for _, j := range run.Judgments {
			spans = append(spans, judgmentSpan(j))
		}
		finished := run.Finished
		if finished != nil && finished.Kind == judgment.RunKindRerun {
			m, err := e.metrics.Metrics(ctx, run.Run)
			if err != nil {
				return report, fmt.Errorf("read scorecard metrics of run %s: %w", run.Run, err)
			}
			scorecards = append(scorecards, scorecardSpan(*finished, m))
		}
		if finished != nil {
			spans = append(spans, runSpan(*finished))
		}
		if len(spans) == 0 {
			continue
		}
		sent, err := e.client.traceExists(ctx, spans[len(spans)-1].traceID)
		if err != nil {
			return report, fmt.Errorf("look up run %s: %w", run.Run, err)
		}
		if sent {
			report.Skipped++
			continue
		}
		if err := e.client.sendSpans(ctx, spans); err != nil {
			return report, fmt.Errorf("send records of run %s: %w", run.Run, err)
		}
		report.Judgments += len(run.Judgments)
		if finished != nil {
			report.Runs++
		}
	}
	if err := e.client.sendSpans(ctx, scorecards); err != nil {
		return report, fmt.Errorf("send scorecard metrics: %w", err)
	}
	report.Scorecards = len(scorecards)
	reviews, err := e.store.ReviewRecords(ctx)
	if err != nil {
		return report, fmt.Errorf("read reviews: %w", err)
	}
	scores := make([]score, len(reviews))
	for i, r := range reviews {
		scores[i] = reviewScore(r)
	}
	if err := e.client.sendScores(ctx, scores); err != nil {
		return report, fmt.Errorf("send reviews: %w", err)
	}
	report.Reviews = len(reviews)
	return report, nil
}
