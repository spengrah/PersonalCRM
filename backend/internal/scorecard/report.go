package scorecard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"
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
	// method's. For kev-masked compared with kev-unmasked it is the masking
	// cost.
	Difference map[string]Rates
	// Flips are the reviewed outcomes one method judged as the user did and
	// the other did not.
	Flips []Flip
}

// Rates are an outcome's recall and precision.
type Rates struct {
	Recall, Precision float64
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
	// ProposalFewestIOweMissed: the method that passes HC3 and misses the
	// fewest "I owe" outcomes, ties broken by fewer corrections caused.
	ProposalFewestIOweMissed ProposalReason = "fewest_i_owe_missed"
	// ProposalTied: several methods that pass HC3 tie on both counts.
	ProposalTied ProposalReason = "tied"
	// ProposalNonePasses: no scored method's terms status passes HC3.
	ProposalNonePasses ProposalReason = "none_passes_hc3"
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
// scores.
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
	contexts *threadctx.Builder,
	contacts *repository.ContactRepository,
	watchdog config.WatchdogConfig,
) *Scorecard {
	return &Scorecard{
		reader:  NewReader(store),
		runner:  runner,
		methods: methods,
		rule:    &timestampRule{contexts: contexts, contacts: contacts, watchdog: watchdog},
	}
}

// Report reruns methods over every example the user reviewed and scores
// them: the method methodKey names, and compareKey's compared with it when
// set; with no method named, every method in the registry. A named method
// must be available; with none named, a method whose route is not configured
// is listed as unavailable. The report proposes a first production method
// among the scored methods and changes no designation.
func (s *Scorecard) Report(ctx context.Context, methodKey, compareKey string) (Report, error) {
	examples, err := s.reader.store.ReviewedExamples(ctx)
	if err != nil {
		return Report{}, err
	}
	report := Report{Examples: len(examples)}
	if len(examples) == 0 {
		report.Proposal = Proposal{Reason: ProposalNoReviews}
		return report, nil
	}
	methods, unavailable, err := s.selectMethods(methodKey, compareKey)
	if err != nil {
		return Report{}, err
	}
	report.Unavailable = unavailable
	ids := make([]judgment.ExampleID, len(examples))
	for i, ex := range examples {
		ids[i] = ex.ID
	}
	var runs []scoredRun
	for _, m := range methods {
		run, err := s.runner.Rerun(ctx, m.Ref().Key, ids)
		if err != nil {
			return Report{}, fmt.Errorf("rerun %s: %w", m.Ref().Key, err)
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
	report.Proposal = propose(report.Methods)
	return report, nil
}

// selectMethods returns the methods a report scores, and with none named the
// registry's methods it cannot build.
func (s *Scorecard) selectMethods(methodKey, compareKey string) ([]decision.Method, []string, error) {
	if methodKey == "" && compareKey != "" {
		return nil, nil, errors.New("a compare method needs a method to compare with")
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
		m, err := s.methods.Get(key)
		if err != nil {
			return nil, nil, err
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
		c.Difference[key] = Rates{Recall: b.Recall - a.Recall, Precision: b.Precision - a.Precision}
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

// propose proposes, among the scored methods whose terms status passes HC3,
// the one that misses the fewest "I owe" outcomes, ties broken by fewer
// corrections caused.
func propose(methods []MethodScore) Proposal {
	var passing []MethodScore
	for _, m := range methods {
		if m.Terms.Passes() {
			passing = append(passing, m)
		}
	}
	if len(passing) == 0 {
		return Proposal{Reason: ProposalNonePasses}
	}
	rank := func(a, b MethodScore) int {
		return cmp.Or(
			cmp.Compare(a.Metrics.IOweMissed, b.Metrics.IOweMissed),
			cmp.Compare(a.Metrics.CorrectionsCaused, b.Metrics.CorrectionsCaused),
		)
	}
	slices.SortStableFunc(passing, rank)
	var tied []string
	for _, m := range passing {
		if rank(m, passing[0]) == 0 {
			tied = append(tied, m.Metrics.Method.Key)
		}
	}
	if len(tied) > 1 {
		return Proposal{Reason: ProposalTied, Tied: tied}
	}
	return Proposal{Method: tied[0], Reason: ProposalFewestIOweMissed}
}
