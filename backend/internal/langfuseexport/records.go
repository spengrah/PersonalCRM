package langfuseexport

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/scorecard"
)

// Trace and score names, one per kind of record.
const (
	traceJudgment  = "judgment"
	traceRun       = "judgment_run"
	scoreVerdict   = "verdict"
	scoreScorecard = "scorecard"
)

// span is one trace's single root span: the unit the export sends over OTLP.
// Langfuse never replaces an observation it holds, so a span carries only
// what is fixed once its record exists, and is sent once.
type span struct {
	traceID, spanID string
	name            string
	start, end      time.Time
	// generation marks a judgment's call, which carries its model, usage and
	// cost.
	generation bool
	model      string
	usage      map[string]int
	cost       map[string]float64
	metadata   map[string]any
	output     any
}

// score is a value on a trace that can change after it is first sent: a
// review, or a rerun's scorecard metrics. Langfuse upserts a score by its ID,
// so every export sends each one.
type score struct {
	id, traceID    string
	name, dataType string
	at             time.Time
	value          any
	metadata       map[string]any
}

// judgmentKey is one judgment: a method version's answer for one example.
type judgmentKey struct {
	example judgment.ExampleID
	method  judgment.MethodRef
}

func keyOf(example judgment.ExampleID, method judgment.MethodRef) judgmentKey {
	return judgmentKey{example: example, method: method}
}

// ids are the judgment's trace and span; its reviews' scores name the trace.
func (k judgmentKey) ids() (trace, span string) {
	return ids(traceJudgment, string(k.example), k.method.Key, k.method.Version, k.method.WordingVersion)
}

// ids derives a trace ID (32 hex digits) and a span ID (16 hex digits) from
// the parts naming one record, so every export names a record by the same
// IDs: it finds the spans Langfuse holds by their trace, and a score sent
// again replaces the one Langfuse holds.
func ids(parts ...string) (trace, span string) {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16]), hex.EncodeToString(sum[16:24])
}

func methodMetadata(m judgment.MethodRef) map[string]any {
	return map[string]any{"method_key": m.Key, "method_version": m.Version, "wording_version": m.WordingVersion}
}

// judgmentSpan is a judgment's call: timestamped at its execution time and
// lasting its latency, with its cut-off as metadata, since Langfuse hides
// observations far from their trace's date.
func judgmentSpan(j judgment.JudgmentRecord) span {
	trace, spanID := keyOf(j.Example, j.Method).ids()
	s := span{
		traceID: trace, spanID: spanID, name: traceJudgment,
		start: j.JudgedAt, end: j.JudgedAt.Add(j.Usage.Latency),
		generation: true, model: j.Method.Key,
		metadata: methodMetadata(j.Method),
		output:   questionsOf(j),
	}
	s.metadata["example_id"] = string(j.Example)
	s.metadata["cut_off"] = j.CutOff.UTC().Format(time.RFC3339)
	s.metadata["context_tokens"] = j.Usage.ContextTokens
	s.metadata["question_count"] = j.Usage.Questions
	s.metadata["latency_ms"] = j.Usage.Latency.Milliseconds()
	// Decision models generate no text, so every billed token is input.
	if j.Usage.BilledTokens != nil {
		s.usage = map[string]int{"input": *j.Usage.BilledTokens}
	}
	if j.Usage.CostUSD != nil {
		s.cost = map[string]float64{"total": *j.Usage.CostUSD}
	}
	return s
}

// question is one answered question as the export sends it: the option the
// method selected, every option's probability and, where the answer yields
// one, its outcome.
type question struct {
	ID            string             `json:"id"`
	Answer        string             `json:"answer"`
	Outcome       string             `json:"outcome,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// questionsOf lists every answered question, outcome or not, with its
// selected option and option probabilities. A source-message question's
// candidate options are keyed by message keys, which are source identifiers,
// so each message key is sent as an opaque candidate, the same candidate
// wherever the judgment names that message; its own options, which name no
// message, keep their keys.
func questionsOf(j judgment.JudgmentRecord) []question {
	outcomes := map[string]judgment.QuestionOutcome{}
	for _, o := range j.Outcomes {
		outcomes[o.QuestionID] = o
	}
	candidates := messageCandidates(j)
	out := make([]question, len(j.Questions))
	for i, q := range j.Questions {
		out[i] = question{ID: q.ID, Answer: q.Chosen(), Probabilities: q.Probabilities}
		if isSourceMessage(q.ID) {
			out[i].Answer = cmp.Or(candidates[out[i].Answer], out[i].Answer)
			out[i].Probabilities = make(map[string]float64, len(q.Probabilities))
			for k, p := range q.Probabilities {
				out[i].Probabilities[cmp.Or(candidates[k], k)] = p
			}
		}
		if o, ok := outcomes[q.ID]; ok {
			confidence := o.Confidence
			out[i].Outcome, out[i].Confidence = o.Outcome, &confidence
		}
	}
	return out
}

// sourceMessageOptions are the options a source-message question offers
// besides its messages, in every wording; none names a message.
var sourceMessageOptions = func() map[string]bool {
	keys := map[string]bool{}
	for _, w := range []decision.Wording{decision.DefaultWording, decision.ReasonedWording} {
		for _, o := range w.SourceMessage.Options {
			keys[o.Key] = true
		}
	}
	return keys
}()

// isSourceMessage reports whether a question is a source-message question,
// by its own ID.
func isSourceMessage(id string) bool {
	return strings.HasSuffix(id, judgment.SourceMessageQuestionID(""))
}

// messageCandidates maps each message key the judgment's source-message
// questions offer or select to an opaque candidate, numbered in key order.
// Every key of those questions that is not one of their own options is taken
// for a message key, so an unknown key is never sent as it is.
func messageCandidates(j judgment.JudgmentRecord) map[string]string {
	var keys []string
	for _, q := range j.Questions {
		if !isSourceMessage(q.ID) {
			continue
		}
		for _, k := range append(slices.Collect(maps.Keys(q.Probabilities)), q.Chosen()) {
			if k != "" && !sourceMessageOptions[k] && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	candidates := make(map[string]string, len(keys))
	for i, k := range keys {
		candidates[k] = "candidate_" + strconv.Itoa(i+1)
	}
	return candidates
}

// runIDs are a finished run's trace and span; its scorecard score names the
// trace.
func runIDs(run judgment.RunID) (trace, span string) {
	return ids(traceRun, run.String())
}

// runSpan is a finished run's summary, spanning the run.
func runSpan(r judgment.RunRecord) span {
	trace, spanID := runIDs(r.Run)
	incomplete := map[string]int{}
	for reason, n := range r.Incomplete {
		incomplete[string(reason)] = n
	}
	s := span{
		traceID: trace, spanID: spanID, name: traceRun,
		start: r.StartedAt, end: r.FinishedAt,
		metadata: methodMetadata(r.Method),
	}
	s.metadata["run_id"] = r.Run.String()
	s.metadata["kind"] = string(r.Kind)
	s.metadata["judged"] = r.Judged
	s.metadata["incomplete"] = incomplete
	s.metadata["obligations_opened"] = r.Effects.ObligationsOpened
	s.metadata["flags_raised"] = r.Effects.FlagsRaised
	s.metadata["claims_raised"] = r.Effects.ClaimsRaised
	if r.From != nil {
		s.metadata["window_from"] = r.From.UTC().Format(time.RFC3339)
	}
	if r.To != nil {
		s.metadata["window_to"] = r.To.UTC().Format(time.RFC3339)
	}
	if r.CostUSD != nil {
		s.metadata["cost_usd"] = *r.CostUSD
	}
	return s
}

// outcomeMetrics is one outcome's scores as the export sends them.
type outcomeMetrics struct {
	Recall    float64                   `json:"recall"`
	Precision float64                   `json:"precision"`
	Confusion map[string]map[string]int `json:"confusion"`
}

// scorecardScore is a finished rerun run's scorecard metrics, one score on
// the run's trace. The metrics follow the latest reviews, so they travel as a
// score whose ID names the run, and each export replaces the one Langfuse
// holds. Its value is the user's "I owe" outcomes the method missed, the
// number the scorecard ranks methods by first; its metadata holds every
// metric.
func scorecardScore(r judgment.RunRecord, m scorecard.RunMetrics) score {
	id, _ := ids(scoreScorecard, r.Run.String())
	trace, _ := runIDs(r.Run)
	perOutcome := make(map[string]outcomeMetrics, len(m.PerOutcome))
	for key, o := range m.PerOutcome {
		perOutcome[key] = outcomeMetrics{Recall: o.Recall, Precision: o.Precision, Confusion: o.Confusion}
	}
	metadata := methodMetadata(m.Method)
	metadata["run_id"] = m.Run.String()
	metadata["i_owe_missed"] = m.IOweMissed
	metadata["corrections_caused"] = m.CorrectionsCaused
	metadata["per_outcome"] = perOutcome
	return score{
		id: id, traceID: trace, name: scoreScorecard, dataType: "NUMERIC",
		at: r.FinishedAt, value: m.IOweMissed, metadata: metadata,
	}
}

// reviewScore is the user's verdict on one outcome of a judgment, a score on
// that judgment's trace. Its ID names the judgment and the question, so a
// later review of the same outcome replaces it.
func reviewScore(r judgment.ReviewRecord) score {
	id, _ := ids(scoreVerdict, string(r.Example), r.Method.Key, r.Method.Version, r.Method.WordingVersion, r.QuestionID)
	metadata := methodMetadata(r.Method)
	metadata["example_id"] = string(r.Example)
	metadata["question_id"] = r.QuestionID
	metadata["right_outcome"] = r.RightOutcome
	trace, _ := keyOf(r.Example, r.Method).ids()
	return score{
		id: id, traceID: trace, name: scoreVerdict, dataType: "CATEGORICAL",
		at: r.ReviewedAt, value: string(r.Verdict), metadata: metadata,
	}
}
