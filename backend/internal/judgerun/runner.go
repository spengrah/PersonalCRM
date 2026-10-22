// Package judgerun decides when and over which threads decision-model methods
// judge, at which cut-offs, and records every result through the judgment
// store. It reads thread content facts through threadctx and computes none of
// them; it writes no record itself.
package judgerun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
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
	// Unjudged counts the new messages no judgment of the run answered, by
	// reason.
	Unjudged map[judgment.UnjudgedReason]int
	// Effects are the live effects the run made.
	Effects judgment.Effects
}

// Historical judges, at each simulated nightly cut-off of the days, the
// threads that gained content since the previous cut-off, each over its
// window of content up to that cut-off, a thread's cut-offs in order, and
// writes as a production run does.
func (r *Runner) Historical(ctx context.Context, methodKey string, days Days) (Summary, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return Summary{}, err
	}
	from, to := days.Start(), days.End()
	run, err := r.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: m.Ref(), From: &from, To: &to})
	if err != nil {
		return Summary{}, err
	}
	s := r.newSummary(run, judgment.RunKindHistorical, m)
	s.CutOffs = len(days.CutOffs())
	if err := r.replay(ctx, &s, m, days, nil); err != nil {
		return s, fmt.Errorf("historical run %s: %w", run, err)
	}
	if err := r.store.FinishRun(ctx, run); err != nil {
		return s, err
	}
	return s, nil
}

// Target is a thread context to rerun: the thread, the cut-off and the input
// hash of an example, and the judgments the caller names as the ones it
// reviewed, whose runs the replay starts from. It holds no review: a run reads
// none.
type Target struct {
	Thread    threadctx.VenueID
	CutOff    time.Time
	InputHash string
	Judgments []uuid.UUID
}

// Rerun judges each target's thread as an evidence-only run: its cut-offs in
// order from the start of the run the judgment of the target appeared in, up
// to the target's cut-off, reusing any judgment the method version holds for
// what a cut-off reads, and reports a target whose rebuilt input differs as
// input_changed.
func (r *Runner) Rerun(ctx context.Context, methodKey string, targets []Target) (judgment.RunID, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return judgment.RunID{}, err
	}
	run, err := r.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKindRerun, Method: m.Ref()})
	if err != nil {
		return judgment.RunID{}, err
	}
	s := r.newSummary(run, judgment.RunKindRerun, m)
	starts, err := r.startDays(ctx, targets)
	if err != nil {
		return run, err
	}
	// The targets grouped by the days their replay covers.
	type group struct {
		days    Days
		reruns  map[threadctx.VenueID][]time.Time
		threads []threadctx.VenueID
	}
	var groups []*group
	for i, ex := range targets {
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
		gi := slices.IndexFunc(groups, func(g *group) bool { return g.days == starts[i] })
		if gi < 0 {
			groups = append(groups, &group{days: starts[i], reruns: map[threadctx.VenueID][]time.Time{}})
			gi = len(groups) - 1
		}
		g := groups[gi]
		if _, seen := g.reruns[ex.Thread]; !seen {
			g.threads = append(g.threads, ex.Thread)
		}
		g.reruns[ex.Thread] = append(g.reruns[ex.Thread], ex.CutOff)
	}
	for _, g := range groups {
		only := map[threadctx.VenueID][]time.Time{}
		for _, thread := range g.threads {
			only[thread] = g.reruns[thread]
		}
		if err := r.replay(ctx, &s, m, g.days, only); err != nil {
			return run, fmt.Errorf("rerun %s: %w", run, err)
		}
	}
	return run, r.store.FinishRun(ctx, run)
}

// startDays returns, for each target, the days of the run its named
// judgments appeared in: the earliest historical or scheduled run holding one
// of them, so its replay starts where that run started. A target no such run
// holds replays the month of its cut-off.
func (r *Runner) startDays(ctx context.Context, targets []Target) ([]Days, error) {
	runs, err := r.store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	windows := map[judgment.RunID]Days{}
	for _, run := range runs {
		if (run.Kind == judgment.RunKindHistorical || run.Kind == judgment.RunKindScheduled) && run.From != nil && run.To != nil {
			windows[run.ID] = Days{start: run.From.In(time.Local), end: run.To.In(time.Local)}
		}
	}
	out := make([]Days, len(targets))
	for i, ex := range targets {
		out[i] = MonthOf(ex.CutOff.Add(-time.Nanosecond))
		views, err := r.store.ListJudgments(ctx, judgment.JudgmentQuery{Thread: &ex.Thread})
		if err != nil {
			return nil, err
		}
		var earliest *Days
		for _, v := range views {
			if !slices.Contains(ex.Judgments, v.ID) {
				continue
			}
			for _, a := range v.Runs {
				if days, ok := windows[a.Run]; ok && (earliest == nil || days.start.Before(earliest.start)) {
					earliest = &days
				}
			}
		}
		if earliest != nil {
			out[i] = *earliest
		}
	}
	return out, nil
}

func (r *Runner) newSummary(run judgment.RunID, kind judgment.RunKind, m decision.Method) Summary {
	production, ok := r.store.ProductionMethod()
	return Summary{
		Run:        run,
		Kind:       kind,
		Method:     m.Ref(),
		Production: ok && production == m.Ref().Key,
		Incomplete: map[judgment.IncompleteReason]int{},
		Unjudged:   map[judgment.UnjudgedReason]int{},
	}
}

// earlier is a judgment of the method version at an earlier cut-off of a
// thread: what a later cut-off re-checks.
type earlier struct {
	id        uuid.UUID
	cutOff    time.Time
	flags     []judgment.FlagDecision
	clearings []judgment.ClearingDecision
}

// history is a thread's judgments by the method version at the cut-offs a run
// has passed, in order: those the version's history holds from before the run
// started and those the run judged or reused.
type history struct {
	entries []earlier
}

// commitments returns the earlier commitment flag decisions above the
// clearing floor that no clearing decision of the history answers done.
func (h *history) commitments(floor float64) []decision.Commitment {
	type key struct {
		judgment uuid.UUID
		message  string
	}
	cleared := map[key]bool{}
	for _, e := range h.entries {
		for _, c := range e.clearings {
			if c.Done {
				cleared[key{c.CommitmentJudgment, c.CommitmentMessageKey}] = true
			}
		}
	}
	var out []decision.Commitment
	for _, e := range h.entries {
		for _, f := range e.flags {
			if f.IsCommitmentAbove(floor) && !cleared[key{e.id, f.MessageKey}] {
				out = append(out, decision.Commitment{Judgment: e.id, MessageKey: f.MessageKey})
			}
		}
	}
	return out
}

// replay is the one path every run judges through. It judges, in order at
// each cut-off of the days, the threads eligible there, or with only set
// only those threads, at the cut-offs eligible up to the last of theirs it
// names and at each cut-off it names. Each thread's history starts from the
// judgments the method version holds before the days.
func (r *Runner) replay(ctx context.Context, s *Summary, m decision.Method, days Days, only map[threadctx.VenueID][]time.Time) error {
	floor, err := r.store.ClearingFloor(ctx, m.Ref().Key)
	if err != nil {
		return err
	}
	schedule, err := eligible(ctx, r.contexts, days)
	if err != nil {
		return err
	}
	// The cut-offs to judge per thread, in order.
	type step struct {
		thread threadctx.VenueID
		cutOff time.Time
	}
	var steps []step
	if only == nil {
		for _, e := range schedule {
			for _, thread := range e.threads {
				steps = append(steps, step{thread: thread, cutOff: e.cutOff})
			}
		}
	} else {
		for thread, named := range only {
			last := slices.MaxFunc(named, func(a, b time.Time) int { return a.Compare(b) })
			at := map[time.Time]bool{}
			for _, e := range schedule {
				if e.cutOff.After(last) || !slices.Contains(e.threads, thread) {
					continue
				}
				at[e.cutOff] = true
			}
			for _, cutOff := range named {
				at[cutOff] = true
			}
			for cutOff := range at {
				steps = append(steps, step{thread: thread, cutOff: cutOff})
			}
		}
		slices.SortFunc(steps, func(a, b step) int {
			if c := a.cutOff.Compare(b.cutOff); c != 0 {
				return c
			}
			return slices.Compare(a.thread[:], b.thread[:])
		})
	}
	histories := map[threadctx.VenueID]*history{}
	for _, st := range steps {
		h, ok := histories[st.thread]
		if !ok {
			if h, err = r.priorHistory(ctx, m, st.thread, days.Start()); err != nil {
				return err
			}
			histories[st.thread] = h
		}
		c, err := r.contexts.Build(ctx, st.thread, st.cutOff)
		if err != nil {
			return fmt.Errorf("build thread %s at %s: %w", st.thread, st.cutOff.Format(time.RFC3339), err)
		}
		if err := r.judge(ctx, s, m, floor, h, c); err != nil {
			return fmt.Errorf("thread %s at %s: %w", st.thread, st.cutOff.Format(time.RFC3339), err)
		}
	}
	return nil
}

// priorHistory is the method version's judgments of the thread at cut-offs up
// to and including start, one per cut-off. A range's start is the cut-off
// that closes the day before it, so a judgment there is part of the range's
// history.
func (r *Runner) priorHistory(ctx context.Context, m decision.Method, thread threadctx.VenueID, start time.Time) (*history, error) {
	prior, err := r.store.JudgmentsThrough(ctx, m.Ref(), thread, start)
	if err != nil {
		return nil, err
	}
	h := &history{}
	for _, j := range prior {
		e := earlier{id: j.ID, cutOff: j.CutOff, flags: j.Flags, clearings: j.Clearings}
		if n := len(h.entries); n > 0 && h.entries[n-1].cutOff.Equal(j.CutOff) {
			h.entries[n-1] = e
			continue
		}
		h.entries = append(h.entries, e)
	}
	return h, nil
}

// judge ends one eligible context with a recorded judgment, a reused one or an
// incomplete report, and records the new messages no judgment answered: those
// over the method's limits, and every one of a cut-off no judgment answers. A
// judgment the method version already holds for what the cut-off reads is
// reused and nothing is sent; otherwise the plan is sent, and a failed call is
// retried before it is reported call_failed.
func (r *Runner) judge(ctx context.Context, s *Summary, m decision.Method, floor float64, h *history, c threadctx.Context) error {
	s.Eligible++
	previous := previousCutOff(c.CutOff)
	first := len(h.entries) == 0
	req := decision.Request{Context: c, Commitments: h.commitments(floor), Floor: floor}
	if !first {
		req.NewAfter = &previous
	}
	// The messages new at the cut-off by the rule for a cut-off after the
	// thread's first judged one, which also decides those of a cut-off no
	// judgment answers.
	var newSince []string
	for _, e := range c.Entries {
		if e.SentAt.After(previous) {
			newSince = append(newSince, e.Key)
		}
	}
	plan, err := m.Plan(ctx, req)
	if err != nil {
		return err
	}
	if plan.Incomplete != "" {
		return r.unanswered(ctx, s, c, plan.Incomplete, newSince, nil)
	}
	effects, reused, err := r.store.ReuseRead(ctx, s.Run, c.Thread, c.CutOff, c.InputHash, plan.Read)
	if err != nil {
		return err
	}
	if reused {
		s.Reused++
	} else {
		result, err := judgeWithRetries(ctx, m, plan)
		if err != nil {
			return err
		}
		switch res := result.(type) {
		case decision.Judged:
			if effects, err = r.store.RecordJudgment(ctx, s.Run, res.Judgment); err != nil {
				return err
			}
			s.Judged++
		case decision.Incomplete:
			return r.unanswered(ctx, s, c, res.Reason, newSince, plan.OverLimit)
		default:
			return fmt.Errorf("method %s returned an unknown result %T", m.Ref().Key, result)
		}
	}
	s.Effects = addEffects(s.Effects, effects)
	over := make([]judgment.UnjudgedMessage, len(plan.OverLimit))
	for i, key := range plan.OverLimit {
		over[i] = judgment.UnjudgedMessage{Thread: c.Thread, CutOff: c.CutOff, MessageKey: key, Reason: judgment.UnjudgedReasonMessageOverLimit}
	}
	if err := r.unjudged(ctx, s, over); err != nil {
		return err
	}
	return r.remember(ctx, s, h, c)
}

// remember adds the judgment the run recorded or reused at the cut-off to the
// thread's history.
func (r *Runner) remember(ctx context.Context, s *Summary, h *history, c threadctx.Context) error {
	views, err := r.store.ListJudgments(ctx, judgment.JudgmentQuery{Thread: &c.Thread, Run: &s.Run})
	if err != nil {
		return err
	}
	for _, v := range views {
		for _, a := range v.Runs {
			if a.Run == s.Run && a.CutOff.Equal(c.CutOff) {
				h.entries = append(h.entries, earlier{id: v.ID, cutOff: c.CutOff, flags: v.Flags, clearings: v.Clearings})
				return nil
			}
		}
	}
	return errors.New("the judgment just recorded is not in the run")
}

// unanswered ends a cut-off no judgment answers: it reports the context
// incomplete and its new messages unjudged for the same reason, those the plan
// found over the method's limits as message_over_limit.
func (r *Runner) unanswered(ctx context.Context, s *Summary, c threadctx.Context, reason judgment.IncompleteReason, newSince, overLimit []string) error {
	if err := r.incomplete(ctx, s, c, reason); err != nil {
		return err
	}
	messages := make([]judgment.UnjudgedMessage, len(newSince))
	for i, key := range newSince {
		unjudged := judgment.UnjudgedReasonCallFailed
		if reason == judgment.IncompleteReasonMessageOverLimit || slices.Contains(overLimit, key) {
			unjudged = judgment.UnjudgedReasonMessageOverLimit
		}
		messages[i] = judgment.UnjudgedMessage{Thread: c.Thread, CutOff: c.CutOff, MessageKey: key, Reason: unjudged}
	}
	return r.unjudged(ctx, s, messages)
}

func (r *Runner) unjudged(ctx context.Context, s *Summary, messages []judgment.UnjudgedMessage) error {
	if len(messages) == 0 {
		return nil
	}
	if err := r.store.RecordUnjudged(ctx, s.Run, messages); err != nil {
		return err
	}
	for _, m := range messages {
		s.Unjudged[m.Reason]++
	}
	return nil
}

func (r *Runner) incomplete(ctx context.Context, s *Summary, c threadctx.Context, reason judgment.IncompleteReason) error {
	if err := r.store.RecordIncomplete(ctx, s.Run, c.Thread, c.CutOff, reason); err != nil {
		return err
	}
	s.Incomplete[reason]++
	return nil
}

// judgeWithRetries asks the method to judge the plan, asking again after a
// failed call up to callAttempts times in all. It returns an error only when
// ctx ends.
func judgeWithRetries(ctx context.Context, m decision.Method, p *decision.Plan) (decision.Result, error) {
	wait := retryBackoff
	for attempt := 1; ; attempt++ {
		result, err := m.Judge(ctx, p)
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
