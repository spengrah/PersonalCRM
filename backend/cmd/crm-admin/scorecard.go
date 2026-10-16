package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"personal-crm/backend/internal/scorecard"
)

// scorecardReporter is the narrow interface --scorecard needs. Production
// wires it to a *scorecard.Scorecard.
type scorecardReporter interface {
	Report(ctx context.Context, methodKey, compareKey string) (scorecard.Report, error)
}

// validateScorecard checks the flags --scorecard reads.
func validateScorecard(opts runOptions) error {
	switch {
	case opts.compareMethod == "":
		return nil
	case !opts.scorecard:
		return errors.New("--compare-method applies only to --scorecard")
	case opts.judgeMethod == "":
		return errors.New("--compare-method requires --method <key>")
	case opts.compareMethod == opts.judgeMethod:
		return errors.New("--compare-method must name a method other than --method")
	}
	return nil
}

// runScorecard reruns methods over the thread contexts the user reviewed and
// prints the content-free report.
func runScorecard(ctx context.Context, opts runOptions, deps adminDeps) error {
	report, err := deps.scorecard.Report(ctx, opts.judgeMethod, opts.compareMethod)
	if err != nil {
		return fmt.Errorf("scorecard: %w", err)
	}
	return writeScorecard(deps.stdout, report)
}

// writeScorecard prints the report: example IDs, method keys and versions,
// outcome values, counts and rates only.
func writeScorecard(w io.Writer, r scorecard.Report) error {
	p := &printer{w: w}
	p.printf("scorecard: reviewed_examples=%d\n", r.Examples)
	for _, m := range r.Methods {
		mm := m.Metrics
		p.printf("method key=%s version=%s wording=%s run=%s\n", mm.Method.Key, mm.Method.Version, mm.Method.WordingVersion, mm.Run)
		p.printf("  terms hosted=%t training=%s retention=%s passes_hc3=%t\n",
			m.Terms.Hosted, findingOrNone(string(m.Terms.Training)), findingOrNone(string(m.Terms.Retention)), m.Terms.Passes())
		p.printf("  i_owe_missed=%d corrections_caused=%d\n", mm.IOweMissed, mm.CorrectionsCaused)
		for _, reason := range slices.Sorted(maps.Keys(m.Incomplete)) {
			p.printf("  incomplete reason=%s n=%d\n", reason, m.Incomplete[reason])
		}
		for _, key := range slices.Sorted(maps.Keys(mm.PerOutcome)) {
			p.outcome("  ", key, mm.PerOutcome[key])
		}
	}
	for _, key := range r.Unavailable {
		p.printf("unavailable method=%s (route not configured; not scored)\n", key)
	}
	p.printf("reference timestamp_rule (they_owe_reply only; the rule never produces \"I owe\")\n")
	p.outcome("  ", "ball_in_court.they_owe_reply", r.Reference)
	if c := r.Comparison; c != nil {
		p.printf("comparison method=%s compare=%s (difference is compare minus method; comparing kev-masked with kev-unmasked, it is the masking cost)\n",
			c.Method.Key, c.Compare.Key)
		for _, key := range slices.Sorted(maps.Keys(c.Difference)) {
			d := c.Difference[key]
			p.printf("  difference outcome=%s recall=%+.3f precision=%+.3f\n", key, d.Recall, d.Precision)
		}
		for _, f := range c.Flips {
			p.printf("  flip example=%s outcome=%s right=%s method=%s compare=%s\n", f.Example, f.Outcome, f.Right, f.Method, f.Compare)
		}
	}
	pr := r.Proposal
	p.printf("proposal method=%s reason=%s", findingOrNone(pr.Method), pr.Reason)
	for _, key := range pr.Tied {
		p.printf(" tied=%s", key)
	}
	p.printf(" (a proposal only: designating the production method stays the user's)\n")
	return p.err
}

// printer writes formatted lines, keeping the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, err := fmt.Fprintf(p.w, format, args...)
		if err != nil {
			p.err = fmt.Errorf("write scorecard: %w", err)
		}
	}
}

func (p *printer) outcome(indent, key string, o scorecard.OutcomeMetrics) {
	p.printf("%soutcome=%s recall=%.3f precision=%.3f\n", indent, key, o.Recall, o.Precision)
	for _, right := range slices.Sorted(maps.Keys(o.Confusion)) {
		row := o.Confusion[right]
		for _, judged := range slices.Sorted(maps.Keys(row)) {
			p.printf("%s  reviewed=%s judged=%s n=%d\n", indent, right, judged, row[judged])
		}
	}
}

func findingOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
