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
// review, or one of a rerun's scorecard metrics. Langfuse upserts a score by its ID,
// so every export sends each one.
type score struct {
	id, traceID    string
	name, dataType string
	at             time.Time
	value          any
	metadata       map[string]any
}

// judgmentKey is one judgment: a method version's answer for one example over
// one read of its context. Judgments of one method version and example that
// read different windows are different judgments.
type judgmentKey struct {
	example judgment.ExampleID
	method  judgment.MethodRef
	read    string
}

func keyOf(example judgment.ExampleID, method judgment.MethodRef, read string) judgmentKey {
	return judgmentKey{example: example, method: method, read: read}
}

// parts name the judgment in every ID derived from it. A judgment with no read
// digest, one from before message flags, names itself by example and method
// version alone, so the IDs it was exported under stay its IDs.
func (k judgmentKey) parts() []string {
	p := []string{string(k.example), k.method.Key, k.method.Version, k.method.WordingVersion}
	if k.read != "" {
		p = append(p, k.read)
	}
	return p
}

// ids are the judgment's trace and span; its reviews' scores name the trace.
func (k judgmentKey) ids() (trace, span string) {
	return ids(append([]string{traceJudgment}, k.parts()...)...)
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
	trace, spanID := keyOf(j.Example, j.Method, j.ReadDigest).ids()
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
	candidates := messageCandidates(j)
	s.metadata["window_start"] = candidates[j.WindowStart]
	s.metadata["flags"] = flagsOf(j, candidates)
	s.metadata["clearings"] = clearingsOf(j, candidates)
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

// flag is one flag decision as the export sends it.
type flag struct {
	Message     string  `json:"message"`
	Kind        string  `json:"kind"`
	Probability float64 `json:"probability"`
}

// clearing is one clearing decision as the export sends it. It names the
// commitment by its message's candidate, and not by the earlier judgment
// holding the commitment's flag decision, which is a store identifier.
type clearing struct {
	Message     string  `json:"message"`
	Done        bool    `json:"done"`
	Probability float64 `json:"probability"`
}

func flagsOf(j judgment.JudgmentRecord, candidates map[string]string) []flag {
	out := make([]flag, len(j.Flags))
	for i, f := range j.Flags {
		out[i] = flag{Message: candidates[f.MessageKey], Kind: string(f.Kind), Probability: f.Probability}
	}
	return out
}

func clearingsOf(j judgment.JudgmentRecord, candidates map[string]string) []clearing {
	out := make([]clearing, len(j.Clearings))
	for i, c := range j.Clearings {
		out[i] = clearing{Message: candidates[c.CommitmentMessageKey], Done: c.Done, Probability: c.Probability}
	}
	return out
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

// sourceMessageOptions are the options the retired source-message questions
// offered besides their messages, in every wording; none names a message.
// Only judgments of method versions from before message flags hold such a
// question, so the set never changes.
var sourceMessageOptions = map[string]bool{"none": true, "unlisted": true}

// isSourceMessage reports whether a question is a source-message question,
// by its own ID.
func isSourceMessage(id string) bool {
	return strings.HasSuffix(id, judgment.SourceMessageQuestionID(""))
}

// messageCandidates maps each message key the judgment names to an opaque
// candidate, numbered in key order: the keys its source-message questions
// offer or select, its window start, and its flag and clearing decisions.
// Every key of a source-message question that is not one of its own options is
// taken for a message key, so an unknown key is never sent as it is.
func messageCandidates(j judgment.JudgmentRecord) map[string]string {
	var keys []string
	for _, q := range j.Questions {
		if !isSourceMessage(q.ID) {
			continue
		}
		for _, k := range append(slices.Collect(maps.Keys(q.Probabilities)), q.Chosen()) {
			if !sourceMessageOptions[k] {
				keys = append(keys, k)
			}
		}
	}
	keys = append(keys, j.WindowStart)
	for _, f := range j.Flags {
		keys = append(keys, f.MessageKey)
	}
	for _, c := range j.Clearings {
		keys = append(keys, c.CommitmentMessageKey)
	}
	return candidatesOf(keys)
}

// candidatesOf numbers the distinct non-empty keys in key order.
func candidatesOf(keys []string) map[string]string {
	keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "" })
	slices.Sort(keys)
	keys = slices.Compact(keys)
	candidates := make(map[string]string, len(keys))
	for i, k := range keys {
		candidates[k] = "candidate_" + strconv.Itoa(i+1)
	}
	return candidates
}

// runIDs are a finished run's trace and span; its scorecard scores name the
// trace.
func runIDs(run judgment.RunID) (trace, span string) {
	return ids(traceRun, run.String())
}

// unjudged is one unjudged message as the export sends it. It names no
// thread: the cut-off and the message's candidate say which message it is.
type unjudged struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
	CutOff  string `json:"cut_off"`
}

// unjudgedOf lists a run's unjudged messages, each message as an opaque
// candidate numbered in key order across the run.
func unjudgedOf(messages []judgment.UnjudgedMessage) []unjudged {
	keys := make([]string, len(messages))
	for i, m := range messages {
		keys[i] = m.MessageKey
	}
	candidates := candidatesOf(keys)
	out := make([]unjudged, len(messages))
	for i, m := range messages {
		out[i] = unjudged{Message: candidates[m.MessageKey], Reason: string(m.Reason), CutOff: m.CutOff.UTC().Format(time.RFC3339)}
	}
	return out
}

// runSpan is a finished run's summary, spanning the run, with the new messages
// no judgment of it answered.
func runSpan(r judgment.RunRecord, messages []judgment.UnjudgedMessage) span {
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
	s.metadata["unjudged"] = unjudgedOf(messages)
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

// scorecardScores are a finished rerun run's scorecard metrics, each its own
// numeric score on the run's trace, so each metric charts per method and
// version. The metrics follow the latest labels, so each score's ID names the
// run and the metric, and each export replaces the value Langfuse holds. A
// rate with no labeled decision behind it is not sent, nor are a flag kind's or
// the clearing decisions' counts when none is labeled, nor the reviewed-outcome
// counts when the run covered no review, so a chart never reads an absent
// measurement as a measured zero. The message counts are always sent.
func scorecardScores(r judgment.RunRecord, m scorecard.RunMetrics) []score {
	trace, _ := runIDs(r.Run)
	var scores []score
	add := func(name string, value any) {
		id, _ := ids(scoreScorecard, r.Run.String(), name)
		metadata := methodMetadata(m.Method)
		metadata["run_id"] = m.Run.String()
		scores = append(scores, score{
			id: id, traceID: trace, name: name, dataType: "NUMERIC",
			at: r.FinishedAt, value: value, metadata: metadata,
		})
	}
	addRate := func(name string, rate scorecard.Rate) {
		if v, ok := rate.Value(); ok {
			add(name, v)
		}
	}
	if m.Reviewed > 0 {
		add("i_owe_missed", m.IOweMissed)
		add("corrections_caused", m.CorrectionsCaused)
	}
	confusions := map[string]map[string]map[string]int{}
	for _, key := range slices.Sorted(maps.Keys(m.PerOutcome)) {
		o := m.PerOutcome[key]
		addRate("outcome."+key+".recall", o.Recall)
		addRate("outcome."+key+".precision", o.Precision)
		// Every outcome of a question carries the question's whole confusion.
		question, _, _ := strings.Cut(key, ".")
		confusions[question] = o.Confusion
	}
	for _, question := range slices.Sorted(maps.Keys(confusions)) {
		for _, right := range slices.Sorted(maps.Keys(confusions[question])) {
			row := confusions[question][right]
			for _, judged := range slices.Sorted(maps.Keys(row)) {
				add("confusion."+question+"."+right+"."+judged, row[judged])
			}
		}
	}
	for _, kind := range judgment.FlagKinds {
		f, prefix := m.Flags[kind], "flag."+string(kind)
		if f.ShownHeld+f.ShownNotHeld+f.UnshownHeld+f.UnshownNotHeld == 0 {
			continue
		}
		addRate(prefix+".precision", f.Precision())
		addRate(prefix+".recall", f.Recall())
		add(prefix+".shown_held", f.ShownHeld)
		add(prefix+".shown_not_held", f.ShownNotHeld)
		add(prefix+".unshown_held", f.UnshownHeld)
		add(prefix+".unshown_not_held", f.UnshownNotHeld)
	}
	if c := m.Clearing; c.Accuracy().Of > 0 {
		addRate("clearing.accuracy", c.Accuracy())
		add("clearing.done_labeled_done", c.DoneLabeledDone)
		add("clearing.done_labeled_not_done", c.DoneLabeledNotDone)
		add("clearing.not_done_labeled_done", c.NotDoneLabeledDone)
		add("clearing.not_done_labeled_not_done", c.NotDoneLabeledNotDone)
	}
	add("messages.judged", m.Messages.Judged)
	for _, reason := range judgment.UnjudgedReasons {
		add("messages.unjudged."+string(reason), m.Messages.Unjudged[reason])
	}
	for _, kind := range judgment.FlagKinds {
		add("messages.flagged."+string(kind), m.Messages.Flagged[kind])
	}
	return scores
}

// reviewScore is the user's verdict on one outcome of a judgment, a score on
// that judgment's trace. Its ID names the judgment and the question, so a
// later review of the same outcome replaces it.
func reviewScore(r judgment.ReviewRecord) score {
	key := keyOf(r.Example, r.Method, r.ReadDigest)
	id, _ := ids(append(append([]string{scoreVerdict}, key.parts()...), r.QuestionID)...)
	metadata := methodMetadata(r.Method)
	metadata["example_id"] = string(r.Example)
	metadata["question_id"] = r.QuestionID
	metadata["right_outcome"] = r.RightOutcome
	trace, _ := key.ids()
	return score{
		id: id, traceID: trace, name: scoreVerdict, dataType: "CATEGORICAL",
		at: r.ReviewedAt, value: string(r.Verdict), metadata: metadata,
	}
}
