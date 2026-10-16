// Package scorecard scores decision-model methods against the user's reviews:
// it reruns methods over the thread contexts the user reviewed and reports how
// each method's outcomes compare with the user's right outcomes. Its report
// and per-run metrics hold only opaque example IDs, run IDs, method keys and
// versions, outcome values, counts, rates and the methods' terms status; it
// sends nothing itself.
package scorecard

import (
	"context"
	"fmt"
	"slices"

	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"
)

// Unanswered is the judged value of a reviewed outcome the method gave no
// answer for: the run reported the example incomplete, or the judgment holds
// no outcome for the reviewed question and contact.
const Unanswered = "unanswered"

// RunMetrics are one finished rerun run's scores against the user's reviews.
type RunMetrics struct {
	Run    judgment.RunID
	Method judgment.MethodRef
	// PerOutcome is keyed by outcome, written <question>.<value> (for example
	// commitment_mine.open), since one value answers several questions.
	PerOutcome map[string]OutcomeMetrics
	// IOweMissed counts the user's "I owe" outcomes (i_owe_reply, and an open
	// commitment_mine) the method did not judge as the user did.
	IOweMissed int
	// CorrectionsCaused counts the reviewed outcomes the method answered
	// differently from the user's right outcome.
	CorrectionsCaused int
}

// OutcomeMetrics score one outcome. A rate whose denominator is zero is 0;
// the confusion shows the counts behind it.
type OutcomeMetrics struct {
	Recall, Precision float64
	// Confusion counts the reviewed outcomes of the outcome's question by the
	// user's right outcome, then the method's judged outcome, over the cells
	// where either is this outcome.
	Confusion map[string]map[string]int
}

// OutcomeKey names a question's outcome in PerOutcome.
func OutcomeKey(question judgment.Question, value string) string {
	return string(question) + "." + value
}

// Reader reads per-run metrics from the judgment store alone.
type Reader struct {
	store *judgment.Store
}

// NewReader builds a Reader over the judgment store.
func NewReader(store *judgment.Store) *Reader {
	return &Reader{store: store}
}

// Metrics scores a finished rerun run against the user's reviews of the
// examples it covered: those it judged and those it reported incomplete. It
// reads the store only and sends nothing.
func (r *Reader) Metrics(ctx context.Context, run judgment.RunID) (RunMetrics, error) {
	scored, err := r.scored(ctx, run)
	return scored.metrics, err
}

// scoredRun is a finished rerun run's metrics, the labels they count and the
// run's summary.
type scoredRun struct {
	metrics RunMetrics
	labels  []label
	summary judgment.RunSummary
}

func (r *Reader) scored(ctx context.Context, run judgment.RunID) (scoredRun, error) {
	summary, err := r.finishedRerun(ctx, run)
	if err != nil {
		return scoredRun{}, err
	}
	records, err := r.store.JudgmentRecords(ctx, run)
	if err != nil {
		return scoredRun{}, err
	}
	var judged []judgment.ExampleID
	for _, rec := range records {
		if !slices.Contains(judged, rec.Example) {
			judged = append(judged, rec.Example)
		}
	}
	reviewed, err := r.store.ReviewedExamples(ctx)
	if err != nil {
		return scoredRun{}, err
	}
	var covered []judgment.Example
	for _, ex := range reviewed {
		if slices.Contains(judged, ex.ID) || reportedIncomplete(summary, ex) {
			covered = append(covered, ex)
		}
	}
	outcomes, err := r.store.OutcomesFor(ctx, summary.Method, judged)
	if err != nil {
		return scoredRun{}, err
	}
	labels := labelsOf(covered, outcomes)
	return scoredRun{metrics: score(run, summary.Method, labels), labels: labels, summary: summary}, nil
}

func (r *Reader) finishedRerun(ctx context.Context, run judgment.RunID) (judgment.RunSummary, error) {
	runs, err := r.store.ListRuns(ctx)
	if err != nil {
		return judgment.RunSummary{}, err
	}
	i := slices.IndexFunc(runs, func(s judgment.RunSummary) bool { return s.ID == run })
	switch {
	case i < 0:
		return judgment.RunSummary{}, fmt.Errorf("run %s not found", run)
	case runs[i].Kind != judgment.RunKindRerun:
		return judgment.RunSummary{}, fmt.Errorf("run %s is a %s run, not a rerun", run, runs[i].Kind)
	case runs[i].FinishedAt == nil:
		return judgment.RunSummary{}, fmt.Errorf("run %s has not finished", run)
	}
	return runs[i], nil
}

// reportedIncomplete reports whether the run reported the example's thread
// incomplete at the example's cut-off, where a rerun judges it.
func reportedIncomplete(run judgment.RunSummary, ex judgment.Example) bool {
	return slices.ContainsFunc(run.Incomplete, func(t judgment.IncompleteThread) bool {
		return t.Thread == ex.Thread && t.CutOff.Equal(ex.CutOff)
	})
}

// answerKey names one answer of a judgment: a question, and the contact it
// concerns (uuid.Nil for big_moment).
type answerKey struct {
	question judgment.Question
	contact  uuid.UUID
}

// label is one reviewed outcome: the user's right outcome and the method's
// judged outcome for the same question and contact.
type label struct {
	example judgment.ExampleID
	key     answerKey
	right   string
	judged  string
}

// labelsOf pairs every review of the examples with the method's outcome for
// the same question and contact, Unanswered where it has none.
func labelsOf(examples []judgment.Example, outcomes map[judgment.ExampleID]judgment.Outcomes) []label {
	var labels []label
	for _, ex := range examples {
		answers := answersOf(outcomes[ex.ID])
		for _, rv := range ex.Reviews {
			key := answerKey{question: judgment.Question(rv.Question)}
			if rv.ContactID != nil {
				key.contact = *rv.ContactID
			}
			judged, ok := answers[key]
			if !ok {
				judged = Unanswered
			}
			labels = append(labels, label{example: ex.ID, key: key, right: rv.RightOutcome, judged: judged})
		}
	}
	return labels
}

func answersOf(o judgment.Outcomes) map[answerKey]string {
	answers := map[answerKey]string{}
	for _, b := range o.BallInCourt {
		answers[answerKey{judgment.QuestionBallInCourt, b.ContactID}] = string(b.Outcome)
	}
	for _, c := range o.Commitments {
		question := judgment.QuestionCommitmentTheirs
		if c.Direction == judgment.CommitmentDirectionMine {
			question = judgment.QuestionCommitmentMine
		}
		answers[answerKey{question, c.ContactID}] = string(c.Outcome)
	}
	if o.BigMoment.QuestionID != "" {
		answers[answerKey{question: judgment.QuestionBigMoment}] = string(o.BigMoment.Outcome)
	}
	return answers
}

// iOwe reports whether a right outcome is one of the user's "I owe" outcomes.
func iOwe(question judgment.Question, value string) bool {
	return question == judgment.QuestionBallInCourt && value == string(judgment.BallInCourtIOweReply) ||
		question == judgment.QuestionCommitmentMine && value == string(judgment.CommitmentStateOpen)
}

// score computes a run's metrics from its labels. Every outcome value that
// is a right or judged outcome of some label gets its metrics.
func score(run judgment.RunID, method judgment.MethodRef, labels []label) RunMetrics {
	m := RunMetrics{Run: run, Method: method, PerOutcome: map[string]OutcomeMetrics{}}
	confusion := map[judgment.Question]map[string]map[string]int{}
	for _, l := range labels {
		q := l.key.question
		if confusion[q] == nil {
			confusion[q] = map[string]map[string]int{}
		}
		if confusion[q][l.right] == nil {
			confusion[q][l.right] = map[string]int{}
		}
		confusion[q][l.right][l.judged]++
		if iOwe(q, l.right) && l.judged != l.right {
			m.IOweMissed++
		}
		if l.judged != Unanswered && l.judged != l.right {
			m.CorrectionsCaused++
		}
	}
	for question, cells := range confusion {
		for _, value := range outcomeValues(cells) {
			m.PerOutcome[OutcomeKey(question, value)] = outcomeMetrics(cells, value)
		}
	}
	return m
}

// outcomeValues are the outcome values among a question's right and judged
// outcomes.
func outcomeValues(cells map[string]map[string]int) []string {
	var values []string
	add := func(v string) {
		if v != Unanswered && !slices.Contains(values, v) {
			values = append(values, v)
		}
	}
	for right, row := range cells {
		add(right)
		for judged := range row {
			add(judged)
		}
	}
	return values
}

// outcomeMetrics scores one outcome value from its question's confusion.
func outcomeMetrics(cells map[string]map[string]int, value string) OutcomeMetrics {
	out := OutcomeMetrics{Confusion: map[string]map[string]int{}}
	var hit, reviewed, judged int
	for right, row := range cells {
		for j, n := range row {
			if right != value && j != value {
				continue
			}
			if out.Confusion[right] == nil {
				out.Confusion[right] = map[string]int{}
			}
			out.Confusion[right][j] = n
			if right == value {
				reviewed += n
			}
			if j == value {
				judged += n
			}
			if right == value && j == value {
				hit = n
			}
		}
	}
	out.Recall, out.Precision = rate(hit, reviewed), rate(hit, judged)
	return out
}

func rate(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}
