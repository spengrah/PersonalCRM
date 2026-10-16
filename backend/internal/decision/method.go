// Package decision holds the decision-model methods: how one method answers
// the question tree of one thread context in one call, for every compared
// method, and the registry their keys come from.
package decision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"
)

// Method produces judgments of thread contexts.
type Method interface {
	Ref() judgment.MethodRef
	// Judge answers the context's question tree in one call. It returns an
	// error only when ctx ends; a context it cannot judge is an Incomplete.
	Judge(ctx context.Context, c threadctx.Context) (Result, error)
	// Measure sizes the request Judge would send against the method's limits,
	// sending nothing.
	Measure(ctx context.Context, c threadctx.Context) (Size, error)
	Terms() TermsStatus
}

// Result is exactly one of Judged or Incomplete.
type Result interface{ isResult() }

// Judged is a judgment a method returned.
type Judged struct{ judgment.Judgment }

// Incomplete is a context a method could not judge, and why.
type Incomplete struct{ Reason judgment.IncompleteReason }

func (Judged) isResult()     {}
func (Incomplete) isResult() {}

// Size is a request's token and question counts against a method's limits.
type Size struct {
	Tokens, Questions, TokenLimit, QuestionLimit int
}

// Exceeds reports whether the request is over either limit.
func (s Size) Exceeds() bool { return s.Tokens > s.TokenLimit || s.Questions > s.QuestionLimit }

// Answer is a vendor's answer to one question: a probability for every option
// key the question offered, and the option the vendor names as its answer,
// empty when it names none.
type Answer struct {
	Probabilities map[string]float64
	Chosen        string
}

// Answers is a vendor's response to one request: an answer per question ID,
// and the billed tokens and cost the vendor reported, nil where it reported
// none.
type Answers struct {
	ByQuestion   map[string]Answer
	BilledTokens *int
	CostUSD      *float64
}

// errIncompleteAnswers is a response that leaves a question or an option
// unanswered; the call counts as failed.
var errIncompleteAnswers = errors.New("response leaves a question or option unanswered")

// method is the one implementation of Method; a masking variant decides only
// how the request is prepared and sent.
type method struct {
	def     Definition
	ref     judgment.MethodRef
	prepare func(ctx context.Context, c threadctx.Context, questions []threadctx.Question) (prepared, error)
}

// prepared is one request ready to send: what the vendor reads, and the call
// that sends it.
type prepared struct {
	wire wireRequest
	send func(ctx context.Context) (Answers, error)
}

// newMethod builds a method on its own copy of the wording, so the wording
// its version derives from is the wording it sends, whatever the caller later
// does with the definition.
func newMethod(def Definition, r Route, prepare func(context.Context, threadctx.Context, []threadctx.Question) (prepared, error)) *method {
	def.Wording = def.Wording.clone()
	return &method{def: def, ref: def.ref(r), prepare: prepare}
}

func (m *method) Ref() judgment.MethodRef { return m.ref }

func (m *method) Terms() TermsStatus { return m.def.Terms }

func (m *method) Measure(ctx context.Context, c threadctx.Context) (Size, error) {
	t := buildTree(c, m.def.Wording)
	p, err := m.prepare(ctx, c, t.questions)
	if err != nil {
		return Size{}, err
	}
	return m.size(p.wire), nil
}

func (m *method) size(w wireRequest) Size {
	return Size{Tokens: countTokens(w), Questions: len(w.Questions), TokenLimit: m.def.Limits.Tokens, QuestionLimit: m.def.Limits.Questions}
}

func (m *method) Judge(ctx context.Context, c threadctx.Context) (Result, error) {
	t := buildTree(c, m.def.Wording)
	p, err := m.prepare(ctx, c, t.questions)
	if err != nil {
		return nil, err
	}
	size := m.size(p.wire)
	if size.Exceeds() {
		return Incomplete{Reason: judgment.IncompleteReasonOverLimit}, nil
	}
	judgedAt := accelerated.GetCurrentTime().UTC()
	started := time.Now() //nolint:forbidigo // wall-clock time for call latency
	answers, err := p.send(ctx)
	latency := time.Since(started) //nolint:forbidigo // wall-clock time for call latency
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	// best-effort: a failed call is reported as call_failed; the error may
	// echo request content, so it goes nowhere.
	if err != nil {
		return Incomplete{Reason: judgment.IncompleteReasonCallFailed}, nil
	}
	questions, outcomes, err := t.answer(answers.ByQuestion)
	// best-effort: a response that leaves a question unanswered is a failed
	// call, reported as call_failed.
	if err != nil {
		return Incomplete{Reason: judgment.IncompleteReasonCallFailed}, nil
	}
	return Judged{judgment.Judgment{
		Method:    m.ref,
		Thread:    c.Thread,
		Kind:      c.Kind,
		CutOff:    c.CutOff,
		JudgedAt:  judgedAt,
		InputHash: c.InputHash,
		Questions: questions,
		Outcomes:  outcomes,
		Usage: judgment.Usage{
			ContextTokens: size.Tokens,
			Questions:     size.Questions,
			BilledTokens:  answers.BilledTokens,
			CostUSD:       answers.CostUSD,
			Latency:       latency,
		},
	}}, nil
}

// Registry is the one source of method keys.
type Registry struct {
	methods map[string]func() (Method, error)
}

// Get returns the method a key names. It fails for a key the registry lacks
// and for a method whose route is not configured.
func (r *Registry) Get(key string) (Method, error) {
	build, ok := r.methods[key]
	if !ok {
		return nil, fmt.Errorf("unknown decision method %q", key)
	}
	m, err := build()
	if err != nil {
		return nil, fmt.Errorf("decision method %q: %w", key, err)
	}
	return m, nil
}

// Keys returns every method key, sorted.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.methods))
	for k := range r.methods {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
