package scorecard

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// Report scores methods over every thread context the user reviewed.
type Report struct {
	// Examples counts the reviewed examples each method reran over.
	Examples int
	Methods  []MethodScore
	// Unavailable lists the registry's methods a report of every method left
	// unscored because their route is not configured.
	Unavailable []string
	// Reference is the timestamp rule's score on they_owe_reply, the one
	// outcome it is a reference for.
	Reference OutcomeMetrics
	// Comparison is set when the report compares two methods.
	Comparison *Comparison
	Proposal   Proposal
}

// MethodScore is one method's rerun scored, beside its terms status.
type MethodScore struct {
	Metrics RunMetrics
	Terms   decision.TermsStatus
	// Incomplete counts the examples the rerun reported incomplete, by reason.
	Incomplete map[judgment.IncompleteReason]int
}

// Comparison compares a method with a second one over the same examples.
type Comparison struct {
	Method, Compare judgment.MethodRef
	// Difference is, per outcome, the compare method's rates minus the
	// method's.
	Difference map[string]Rates
	// Flips are the reviewed outcomes one method judged as the user did and
	// the other did not.
	Flips []Flip
}

// Rates are an outcome's recall and precision, 0 where a rate has no value.
type Rates struct {
	Recall, Precision float64
}

// orZero is the rate's value, 0 when it has none.
func orZero(r Rate) float64 {
	v, _ := r.Value()
	return v
}

// Flip is one reviewed outcome of an example that the two methods judged
// differently against the user's right outcome.
type Flip struct {
	Example judgment.ExampleID
	Outcome judgment.Question
	Right   string
	// Method and Compare are each method's judged outcome.
	Method, Compare string
}

// ProposalReason says how the report's proposal was decided.
type ProposalReason string

const (
	// ProposalFewestIOweMissed: the method that misses the fewest "I owe"
	// outcomes, ties broken by fewer corrections caused.
	ProposalFewestIOweMissed ProposalReason = "fewest_i_owe_missed"
	// ProposalTied: several methods tie on both counts.
	ProposalTied ProposalReason = "tied"
	// ProposalNoMethods: no method was scored.
	ProposalNoMethods ProposalReason = "no_methods_scored"
	// ProposalNoReviews: the user has reviewed no example.
	ProposalNoReviews ProposalReason = "no_reviewed_examples"
)

// Proposal is the report's proposal for the first production method. It
// designates nothing: designating the production method stays the user's.
type Proposal struct {
	// Method is the proposed method's key; empty when none is proposed.
	Method string
	Reason ProposalReason
	// Tied lists the tied methods' keys when Reason is ProposalTied.
	Tied []string
}

// Scorecard reruns methods over the reviewed examples and reports their
// scores. A rerun judges each reviewed thread's cut-offs in order through the
// runner, the one path every run judges through.
type Scorecard struct {
	reader  *Reader
	runner  *judgerun.Runner
	methods judgerun.Methods
	rule    *timestampRule
}

// New builds a Scorecard. Its reruns are evidence-only runs of the runner's.
func New(
	store *judgment.Store,
	runner *judgerun.Runner,
	methods judgerun.Methods,
	contacts *repository.ContactRepository,
	watchdog config.WatchdogConfig,
) *Scorecard {
	return &Scorecard{
		reader:  NewReader(store),
		runner:  runner,
		methods: methods,
		rule:    &timestampRule{contacts: contacts, watchdog: watchdog},
	}
}

// Report reruns methods over every example the user reviewed, and every
// example whose judgment answered a flag or clearing decision the user
// labeled, and scores them: the method methodKey names, and compareKey's
// compared with it when set; with no method named, every method in the
// registry. A named method must be known and available, whether or not the
// user has reviewed or labeled anything; with none named, a method whose route
// is not configured is listed as unavailable. The report proposes a first
// production method among the scored methods, by their reviewed outcomes, and
// changes no designation. Its error is always a *Failure.
func (s *Scorecard) Report(ctx context.Context, methodKey, compareKey string) (Report, error) {
	report, err := s.report(ctx, methodKey, compareKey)
	return report, failureOf(err)
}

// targets names, for each reviewed example, the judgments its reviews sit on:
// those of the methods the user reviewed, at the example's thread and input
// hash. A rerun replays from the start of the run they appeared in.
func (s *Scorecard) targets(ctx context.Context, examples []judgment.Example) ([]judgerun.Target, error) {
	records, err := s.reader.store.ReviewRecords(ctx)
	if err != nil {
		return nil, err
	}
	reviewedBy := map[judgment.ExampleID][]judgment.MethodRef{}
	for _, r := range records {
		reviewedBy[r.Example] = append(reviewedBy[r.Example], r.Method)
	}
	targets := make([]judgerun.Target, len(examples))
	for i, ex := range examples {
		targets[i] = judgerun.Target{Thread: ex.Thread, CutOff: ex.CutOff, InputHash: ex.InputHash}
		views, err := s.reader.store.ListJudgments(ctx, judgment.JudgmentQuery{Thread: &ex.Thread})
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			if v.InputHash == ex.InputHash && slices.Contains(reviewedBy[ex.ID], v.Method) {
				targets[i].Judgments = append(targets[i].Judgments, v.ID)
			}
		}
	}
	return targets, nil
}

// labeledTargets names, for each labeled flag or clearing decision, the
// examples whose judgments answered it, and those judgments: a flag decision is
// answered by a judgment holding its message among its new messages, a
// clearing decision by a judgment at its cut-off that re-checked its
// commitment. A label belongs to its decision and not to one method's answer,
// so every method's judgment of the decision is named.
func (s *Scorecard) labeledTargets(ctx context.Context) ([]judgerun.Target, error) {
	labels, err := s.reader.store.Labels(ctx)
	if err != nil {
		return nil, err
	}
	var targets []judgerun.Target
	for _, thread := range slices.SortedFunc(maps.Keys(labels), func(a, b threadctx.VenueID) int { return slices.Compare(a[:], b[:]) }) {
		lb := labels[thread]
		views, err := s.reader.store.ListJudgments(ctx, judgment.JudgmentQuery{Thread: &thread})
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			answers := slices.ContainsFunc(lb.Flags, func(l judgment.FlagLabel) bool { return slices.Contains(v.NewMessages, l.MessageKey) }) ||
				slices.ContainsFunc(lb.Clearings, func(l judgment.ClearingLabel) bool {
					return l.CutOff.Equal(v.CutOff) && slices.ContainsFunc(v.Clearings, func(c judgment.ClearingDecision) bool {
						return c.CommitmentMessageKey == l.CommitmentMessageKey
					})
				})
			if answers {
				targets = addTarget(targets, judgerun.Target{Thread: thread, CutOff: v.CutOff, InputHash: v.InputHash, Judgments: []uuid.UUID{v.ID}})
			}
		}
	}
	return targets, nil
}

// addTarget adds a target to the targets, merging it into the one of the same
// example.
func addTarget(targets []judgerun.Target, t judgerun.Target) []judgerun.Target {
	i := slices.IndexFunc(targets, func(o judgerun.Target) bool { return o.Thread == t.Thread && o.InputHash == t.InputHash })
	if i < 0 {
		return append(targets, t)
	}
	for _, id := range t.Judgments {
		if !slices.Contains(targets[i].Judgments, id) {
			targets[i].Judgments = append(targets[i].Judgments, id)
		}
	}
	return targets
}

func (s *Scorecard) report(ctx context.Context, methodKey, compareKey string) (Report, error) {
	methods, unavailable, err := s.selectMethods(methodKey, compareKey)
	if err != nil {
		return Report{}, err
	}
	examples, err := s.reader.store.ReviewedExamples(ctx)
	if err != nil {
		return Report{}, failed(Failure{Reason: FailureReadReviews}, err)
	}
	report := Report{Examples: len(examples), Unavailable: unavailable, Proposal: Proposal{Reason: ProposalNoReviews}}
	targets, err := s.targets(ctx, examples)
	if err != nil {
		return Report{}, failed(Failure{Reason: FailureReadReviews}, err)
	}
	labeled, err := s.labeledTargets(ctx)
	if err != nil {
		return Report{}, failed(Failure{Reason: FailureReadLabels}, err)
	}
	for _, t := range labeled {
		targets = addTarget(targets, t)
	}
	if len(targets) == 0 {
		return report, nil
	}
	var runs []scoredRun
	for _, m := range methods {
		run, err := s.runner.Rerun(ctx, m.Ref().Key, targets)
		if err != nil {
			return Report{}, failed(Failure{Reason: FailureRerun, Method: m.Ref().Key, Run: run}, err)
		}
		scored, err := s.reader.scored(ctx, run)
		if err != nil {
			return Report{}, err
		}
		incomplete := map[judgment.IncompleteReason]int{}
		for _, t := range scored.summary.Incomplete {
			incomplete[t.Reason]++
		}
		report.Methods = append(report.Methods, MethodScore{Metrics: scored.metrics, Terms: m.Terms(), Incomplete: incomplete})
		runs = append(runs, scored)
	}
	if compareKey != "" {
		report.Comparison = compare(runs[0], runs[1])
	}
	report.Reference, err = s.rule.reference(ctx, examples)
	if err != nil {
		return Report{}, err
	}
	if len(examples) > 0 {
		report.Proposal = propose(report.Methods)
	}
	return report, nil
}

// selectMethods returns the methods a report scores, and with none named the
// registry's methods it cannot build.
func (s *Scorecard) selectMethods(methodKey, compareKey string) ([]decision.Method, []string, error) {
	if methodKey == "" && compareKey != "" {
		return nil, nil, &Failure{Reason: FailureCompareNeedsMethod}
	}
	if methodKey == "" {
		var methods []decision.Method
		var unavailable []string
		for _, key := range s.methods.Keys() {
			m, err := s.methods.Get(key)
			// best-effort: a registered method fails to build only when its
			// route is not configured; the report lists it as unavailable.
			if err != nil {
				unavailable = append(unavailable, key)
				continue
			}
			methods = append(methods, m)
		}
		return methods, unavailable, nil
	}
	var methods []decision.Method
	for _, key := range []string{methodKey, compareKey} {
		if key == "" {
			continue
		}
		if !slices.Contains(s.methods.Keys(), key) {
			return nil, nil, &Failure{Reason: FailureUnknownMethod, Method: key}
		}
		m, err := s.methods.Get(key)
		if err != nil {
			return nil, nil, failed(Failure{Reason: FailureMethodUnavailable, Method: key}, err)
		}
		methods = append(methods, m)
	}
	return methods, nil, nil
}

// compare compares two reruns over the same examples.
func compare(method, other scoredRun) *Comparison {
	c := &Comparison{Method: method.metrics.Method, Compare: other.metrics.Method, Difference: map[string]Rates{}}
	outcomes := slices.Concat(slices.Collect(maps.Keys(method.metrics.PerOutcome)), slices.Collect(maps.Keys(other.metrics.PerOutcome)))
	for _, key := range outcomes {
		a, b := method.metrics.PerOutcome[key], other.metrics.PerOutcome[key]
		c.Difference[key] = Rates{Recall: orZero(b.Recall) - orZero(a.Recall), Precision: orZero(b.Precision) - orZero(a.Precision)}
	}
	type at struct {
		example judgment.ExampleID
		key     answerKey
	}
	judged := map[at]string{}
	for _, l := range method.labels {
		judged[at{l.example, l.key}] = l.judged
	}
	for _, l := range other.labels {
		mine, ok := judged[at{l.example, l.key}]
		if !ok || (mine == l.right) == (l.judged == l.right) {
			continue
		}
		c.Flips = append(c.Flips, Flip{Example: l.example, Outcome: l.key.question, Right: l.right, Method: mine, Compare: l.judged})
	}
	slices.SortFunc(c.Flips, func(a, b Flip) int {
		return cmp.Or(cmp.Compare(a.Example, b.Example), cmp.Compare(a.Outcome, b.Outcome))
	})
	return c
}

// propose proposes, among the scored methods, the one that misses the
// fewest "I owe" outcomes, ties broken by fewer corrections caused. Terms
// findings are reported beside each method and do not filter the proposal;
// judging them is the user's.
func propose(methods []MethodScore) Proposal {
	if len(methods) == 0 {
		return Proposal{Reason: ProposalNoMethods}
	}
	ranked := slices.Clone(methods)
	rank := func(a, b MethodScore) int {
		return cmp.Or(
			cmp.Compare(a.Metrics.IOweMissed, b.Metrics.IOweMissed),
			cmp.Compare(a.Metrics.CorrectionsCaused, b.Metrics.CorrectionsCaused),
		)
	}
	slices.SortStableFunc(ranked, rank)
	var tied []string
	for _, m := range ranked {
		if rank(m, ranked[0]) == 0 {
			tied = append(tied, m.Metrics.Method.Key)
		}
	}
	if len(tied) > 1 {
		return Proposal{Reason: ProposalTied, Tied: tied}
	}
	return Proposal{Method: tied[0], Reason: ProposalFewestIOweMissed}
}
