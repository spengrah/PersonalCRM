// Package judgerun decides when and over which threads decision-model methods
// judge, at which cut-offs, and records every result through the judgment
// store. It reads thread content facts through threadctx and computes none of
// them; it writes no record itself.
package judgerun

import (
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"
)

// Methods is the method registry's surface a runner reads.
// *decision.Registry satisfies it.
type Methods interface {
	Get(key string) (decision.Method, error)
	Keys() []string
}

// callAttempts is how many times a run asks a method to judge one context
// whose call failed before reporting it call_failed.
const callAttempts = 3

// retryBackoff is the wait before the second attempt; each later attempt
// waits twice as long as the one before.
const retryBackoff = time.Second

// Runner runs methods over threads.
type Runner struct {
	contexts *threadctx.Builder
	store    *judgment.Store
	methods  Methods
}

// New builds a Runner. It fails when the production designation names a key
// the method registry lacks, so no run of a misnamed designation starts.
func New(contexts *threadctx.Builder, store *judgment.Store, methods Methods) (*Runner, error) {
	if key, ok := store.ProductionMethod(); ok && !slices.Contains(methods.Keys(), key) {
		return nil, fmt.Errorf("DECISION_PRODUCTION_METHOD names unknown decision method %q", key)
	}
	return &Runner{contexts: contexts, store: store, methods: methods}, nil
}

// Summary is a finished run, content-free: IDs, counts and reasons only.
type Summary struct {
	Run    judgment.RunID
	Kind   judgment.RunKind
	Method judgment.MethodRef
	// Production reports whether the run's method is the designated
	// production method; only then can the run change live state.
	Production bool
	CutOffs    int
	// Eligible counts the thread contexts the run had to judge, one per
	// thread and cut-off; each ends Judged, Reused or Incomplete.
	Eligible   int
	Judged     int
	Reused     int
	Incomplete map[judgment.IncompleteReason]int
	// Effects are the live effects the run made.
	Effects judgment.Effects
}

// Historical judges, at each simulated nightly cut-off of the month, the
// threads that gained content since the previous cut-off, each over its whole
// content up to that cut-off, and writes as a production run does.
func (r *Runner) Historical(ctx context.Context, methodKey string, month Month) (Summary, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return Summary{}, err
	}
	from, to := month.Start(), month.End()
	run, err := r.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: m.Ref(), From: &from, To: &to})
	if err != nil {
		return Summary{}, err
	}
	s := r.newSummary(run, judgment.RunKindHistorical, m)
	s.CutOffs = len(month.CutOffs())
	err = eachEligible(ctx, r.contexts, month, func(c threadctx.Context) error {
		return r.judge(ctx, &s, m, c)
	})
	if err != nil {
		return s, fmt.Errorf("historical run %s: %w", run, err)
	}
	if err := r.store.FinishRun(ctx, run); err != nil {
		return s, err
	}
	return s, nil
}

// Rerun judges each reviewed example's thread at the example's cut-off as an
// evidence-only run, reusing any judgment the method version already holds
// for that input, and reports an example whose rebuilt input differs from the
// reviewed one as input_changed.
func (r *Runner) Rerun(ctx context.Context, methodKey string, examples []judgment.ExampleID) (judgment.RunID, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return judgment.RunID{}, err
	}
	reviewed, err := r.store.ReviewedExamples(ctx)
	if err != nil {
		return judgment.RunID{}, err
	}
	byID := make(map[judgment.ExampleID]judgment.Example, len(reviewed))
	for _, ex := range reviewed {
		byID[ex.ID] = ex
	}
	targets := make([]judgment.Example, 0, len(examples))
	for _, id := range examples {
		ex, ok := byID[id]
		if !ok {
			return judgment.RunID{}, fmt.Errorf("rerun: %q is not a reviewed example", id)
		}
		targets = append(targets, ex)
	}
	run, err := r.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKindRerun, Method: m.Ref()})
	if err != nil {
		return judgment.RunID{}, err
	}
	s := r.newSummary(run, judgment.RunKindRerun, m)
	for _, ex := range targets {
		c, err := r.contexts.Build(ctx, ex.Thread, ex.CutOff)
		if err != nil {
			return run, fmt.Errorf("rerun %s: build thread %s at %s: %w", run, ex.Thread, ex.CutOff.Format(time.RFC3339), err)
		}
		if c.InputHash != ex.InputHash {
			s.Eligible++
			if err := r.incomplete(ctx, &s, c, judgment.IncompleteReasonInputChanged); err != nil {
				return run, err
			}
			continue
		}
		if err := r.judge(ctx, &s, m, c); err != nil {
			return run, fmt.Errorf("rerun %s: %w", run, err)
		}
	}
	return run, r.store.FinishRun(ctx, run)
}

func (r *Runner) newSummary(run judgment.RunID, kind judgment.RunKind, m decision.Method) Summary {
	production, ok := r.store.ProductionMethod()
	return Summary{
		Run:        run,
		Kind:       kind,
		Method:     m.Ref(),
		Production: ok && production == m.Ref().Key,
		Incomplete: map[judgment.IncompleteReason]int{},
	}
}

// judge ends one eligible context with a recorded judgment or an incomplete
// report. A judgment the method version already holds for the thread and input
// is reused and nothing is sent; otherwise the context is sent, and a failed
// call is retried before it is reported call_failed.
func (r *Runner) judge(ctx context.Context, s *Summary, m decision.Method, c threadctx.Context) error {
	s.Eligible++
	effects, reused, err := r.store.ReuseJudgment(ctx, s.Run, c.Thread, c.CutOff, c.InputHash)
	if err != nil {
		return err
	}
	if reused {
		s.Reused++
		s.Effects = addEffects(s.Effects, effects)
		return nil
	}
	result, err := judgeWithRetries(ctx, m, c)
	if err != nil {
		return err
	}
	switch res := result.(type) {
	case decision.Judged:
		effects, err := r.store.RecordJudgment(ctx, s.Run, res.Judgment)
		if err != nil {
			return err
		}
		s.Judged++
		s.Effects = addEffects(s.Effects, effects)
		return nil
	case decision.Incomplete:
		return r.incomplete(ctx, s, c, res.Reason)
	default:
		return fmt.Errorf("method %s returned an unknown result %T", m.Ref().Key, result)
	}
}

func (r *Runner) incomplete(ctx context.Context, s *Summary, c threadctx.Context, reason judgment.IncompleteReason) error {
	if err := r.store.RecordIncomplete(ctx, s.Run, c.Thread, c.CutOff, reason); err != nil {
		return err
	}
	s.Incomplete[reason]++
	return nil
}

// judgeWithRetries asks the method to judge the context, asking again after a
// failed call up to callAttempts times in all. It returns an error only when
// ctx ends.
func judgeWithRetries(ctx context.Context, m decision.Method, c threadctx.Context) (decision.Result, error) {
	wait := retryBackoff
	for attempt := 1; ; attempt++ {
		result, err := m.Judge(ctx, c)
		if err != nil {
			return nil, err
		}
		failed, ok := result.(decision.Incomplete)
		if !ok || failed.Reason != judgment.IncompleteReasonCallFailed || attempt == callAttempts {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func addEffects(a, b judgment.Effects) judgment.Effects {
	return judgment.Effects{
		ObligationsOpened: a.ObligationsOpened + b.ObligationsOpened,
		FlagsRaised:       a.FlagsRaised + b.FlagsRaised,
		ClaimsRaised:      a.ClaimsRaised + b.ClaimsRaised,
	}
}
