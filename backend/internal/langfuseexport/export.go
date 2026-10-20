// Package langfuseexport sends the decision-model records the extraction
// spec allows (G15) to Langfuse's extraction project, and is the one code path
// that sends anything there. Every record it sends is built from the judgment
// store's export reads and the scorecard's per-run metrics, which hold opaque
// example IDs, run IDs, method keys and versions, question IDs, outcome
// values, counts, rates, usage and times, and never thread content, a thread
// ID or a contact ID.
//
// Each judgment is one trace, timestamped at its execution time and carrying
// its cut-off as metadata; each finished run is one trace with its summary;
// each review is a score on its judgment's trace; each metric of a finished
// rerun run's scorecard is a score on its run's trace. Every trace, span and
// score ID derives from the record it carries. Langfuse never replaces an
// observation it holds, so each trace's span carries what is fixed once its
// record exists and is sent once; what changes after that, a review and the
// metrics computed from reviews, is a score, which Langfuse replaces by ID.
package langfuseexport

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
	// Runs and Judgments count the finished runs and judgments sent; Held the
	// runs and judgments Langfuse already held, which were not sent again.
	Runs, Judgments, Held int
	// Scorecards counts the reruns whose scorecard metrics were sent, and
	// Reviews the review scores sent.
	Scorecards, Reviews int
}

// Sent reports whether the export reached Langfuse.
func (r Report) Sent() bool { return len(r.Missing) == 0 }

// Export sends each judgment and finished run Langfuse does not yet hold, and
// every score. A run's judgments are sent whether or not it finished, and its
// summary once it finished. Langfuse is asked once which traces it holds, and
// each span is sent only when its trace is not among them, so no span is sent
// twice. A finished rerun's scorecard metrics, computed from the latest
// reviews, and every live review are sent as scores on each export, so a
// changed verdict replaces the earlier verdict and the metrics it changed.
func (e *Exporter) Export(ctx context.Context) (Report, error) {
	if e.client == nil {
		return Report{Missing: e.missing}, nil
	}
	var report Report
	runs, err := e.store.RunExports(ctx)
	if err != nil {
		return report, fmt.Errorf("read runs: %w", err)
	}
	var spans []span
	var scores []score
	for _, run := range runs {
		for _, j := range run.Judgments {
			spans = append(spans, judgmentSpan(j))
		}
		finished := run.Finished
		if finished == nil {
			continue
		}
		spans = append(spans, runSpan(*finished, run.Unjudged))
		if finished.Kind == judgment.RunKindRerun {
			m, err := e.metrics.Metrics(ctx, run.Run)
			if err != nil {
				return report, fmt.Errorf("read scorecard metrics of run %s: %w", run.Run, err)
			}
			scores = append(scores, scorecardScores(*finished, m)...)
			report.Scorecards++
		}
	}
	unsent, err := e.unsent(ctx, spans)
	if err != nil {
		return report, err
	}
	report.Held = len(spans) - len(unsent)
	if err := e.client.sendSpans(ctx, unsent); err != nil {
		return report, fmt.Errorf("send judgments and runs: %w", err)
	}
	for _, s := range unsent {
		if s.name == traceRun {
			report.Runs++
		} else {
			report.Judgments++
		}
	}
	reviews, err := e.store.ReviewRecords(ctx)
	if err != nil {
		return report, fmt.Errorf("read reviews: %w", err)
	}
	for _, r := range reviews {
		scores = append(scores, reviewScore(r))
	}
	report.Reviews = len(reviews)
	if err := e.client.sendScores(ctx, scores); err != nil {
		return report, fmt.Errorf("send scores: %w", err)
	}
	return report, nil
}

// unsent returns the spans whose trace Langfuse does not hold. A span Langfuse
// holds starts where the span does, at the millisecond precision Langfuse
// keeps, so the lookup reads from the earliest span's start floored to the
// second.
func (e *Exporter) unsent(ctx context.Context, spans []span) ([]span, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	from := slices.MinFunc(spans, func(a, b span) int { return a.start.Compare(b.start) }).start.Truncate(time.Second)
	held, err := e.client.heldTraces(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("look up records Langfuse holds: %w", err)
	}
	var unsent []span
	for _, s := range spans {
		if !held[s.traceID] {
			unsent = append(unsent, s)
		}
	}
	return unsent, nil
}
