// Package decision holds the decision-model methods: how one method judges
// the window of one thread context in as few calls as its limits allow, for
// every compared method, and the registry their keys come from.
package decision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// Method produces judgments of thread contexts.
type Method interface {
	Ref() judgment.MethodRef
	// Plan decides what Judge would read and ask of the request's context:
	// its window, its new messages and the earlier commitments it re-checks. It
	// sends nothing. A plan the method cannot send is Incomplete.
	Plan(ctx context.Context, r Request) (*Plan, error)
	// Judge sends a plan and answers it. It returns an error only when ctx
	// ends; a plan it cannot answer is an Incomplete. A plan whose calls
	// failed may be judged again: calls already answered are not sent twice.
	Judge(ctx context.Context, p *Plan) (Result, error)
	// Measure sizes the request Judge would send for the whole context, every
	// message new, against the method's limits, sending nothing.
	Measure(ctx context.Context, c threadctx.Context) (Size, error)
	Terms() TermsStatus
}

// Request is a thread context at a cut-off, and what the method judging it
// knows of its own earlier judgments.
type Request struct {
	Context threadctx.Context
	// NewAfter is the thread's previous scheduled cut-off: the messages sent
	// after it are new. Nil at the thread's first judged cut-off in the
	// method version's history, where every message in the window is new.
	NewAfter *time.Time
	// Commitments are the method's own earlier commitment flag decisions above
	// its clearing floor that it has not cleared. The method re-checks those
	// whose message lies inside its window.
	Commitments []Commitment
}

// Commitment is an earlier commitment flag decision: the judgment holding it
// and the key of its message.
type Commitment struct {
	Judgment   uuid.UUID
	MessageKey string
}

// Plan is what a method reads and asks of a request. Its Read is the
// identity of what a judgment of the plan would read. A plan is judged once
// or more, never changed by the caller.
type Plan struct {
	// Incomplete is set when the method cannot send the request, and then
	// holds the reason; the plan reads nothing.
	Incomplete judgment.IncompleteReason
	Read       judgment.Read

	context threadctx.Context
	win     window
	tree    tree
	size    Size
	calls   [][]threadctx.Question
	// answered holds the answers of the calls already made, by call.
	answered map[int]callResult
}

// callResult is a call answered: its answers and the usage it cost.
type callResult struct {
	answers Answers
	request wireRequest
	latency time.Duration
}

// Result is exactly one of Judged or Incomplete.
type Result interface{ isResult() }

// Judged is a judgment a method returned.
type Judged struct{ judgment.Judgment }

// Incomplete is a context a method could not judge, and why.
type Incomplete struct{ Reason judgment.IncompleteReason }

func (Judged) isResult()     {}
func (Incomplete) isResult() {}

// Size is the request of a whole context, every message new, against a
// method's limits. Tokens is the size estimate scaled by the method's sizing.
// Calls is how many calls the questions need under QuestionLimit.
type Size struct {
	Tokens, Questions, Calls, TokenLimit, QuestionLimit int
	// NewestFits reports whether the context's newest message alone, with its
	// questions, fits the token limit.
	NewestFits bool
}

// Exceeds reports whether the request is over the token limit, so that
// judging reads a window of the context.
func (s Size) Exceeds() bool { return s.Tokens > s.TokenLimit }

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
	if def.Sizing.Ratio <= 0 || def.Sizing.Margin < 0 {
		panic(fmt.Sprintf("decision: method %q needs a positive sizing ratio and a margin of at least zero", def.Key))
	}
	def.Wording = def.Wording.clone()
	return &method{def: def, ref: def.ref(r), prepare: prepare}
}

func (m *method) Ref() judgment.MethodRef { return m.ref }

func (m *method) Terms() TermsStatus { return m.def.Terms }

// sized is a window's request measured: its tree and its size.
type sized struct {
	win   window
	tree  tree
	size  Size
	calls [][]threadctx.Question
}

// measureWindow builds the request of the context's newest k entries and sizes
// it, all its questions in one request, against the method's limits.
func (m *method) measureWindow(ctx context.Context, r Request, k int) (sized, error) {
	c := r.Context
	win := m.window(r, len(c.Entries)-k)
	t := buildTree(c, m.def.Wording, win)
	p, err := m.prepare(ctx, windowed(c, win.start), t.questions)
	if err != nil {
		return sized{}, err
	}
	limits := m.def.Limits
	calls := split(t.questions, limits.Questions)
	return sized{win: win, tree: t, calls: calls, size: Size{
		Tokens: m.def.Sizing.scale(countTokens(p.wire)), Questions: len(t.questions), Calls: len(calls),
		TokenLimit: limits.Tokens, QuestionLimit: limits.Questions,
	}}, nil
}

// window returns the window of a request starting at the entry start: the
// new messages inside it, and the commitments inside it to re-check.
func (m *method) window(r Request, start int) window {
	c := r.Context
	win := window{start: start}
	for i := start; i < len(c.Entries); i++ {
		if r.NewAfter == nil || c.Entries[i].SentAt.After(*r.NewAfter) {
			win.new = append(win.new, i)
		}
	}
	for _, commitment := range r.Commitments {
		for i := start; i < len(c.Entries); i++ {
			if c.Entries[i].Key == commitment.MessageKey {
				win.rechecked = append(win.rechecked, recheck{entry: i, commitment: commitment})
				break
			}
		}
	}
	sort.SliceStable(win.rechecked, func(a, b int) bool { return win.rechecked[a].entry < win.rechecked[b].entry })
	return win
}

// windowed is the context with only its entries from start on.
func windowed(c threadctx.Context, start int) threadctx.Context {
	c.Entries = c.Entries[start:]
	return c
}

// split divides questions into the fewest calls of at most limit questions,
// as equal as it can.
func split(questions []threadctx.Question, limit int) [][]threadctx.Question {
	if len(questions) == 0 {
		return nil
	}
	n := (len(questions) + limit - 1) / limit
	calls := make([][]threadctx.Question, 0, n)
	for i := range n {
		calls = append(calls, questions[i*len(questions)/n:(i+1)*len(questions)/n])
	}
	return calls
}

func (m *method) Plan(ctx context.Context, r Request) (*Plan, error) {
	n := len(r.Context.Entries)
	if n == 0 {
		return nil, errors.New("plan: thread context has no messages")
	}
	fits := func(k int) (sized, bool, error) {
		s, err := m.measureWindow(ctx, r, k)
		return s, s.size.Tokens <= m.def.Limits.Tokens, err
	}
	// The request grows with its window, so the largest window that fits is
	// found by bisection.
	best, ok, err := fits(1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Plan{Incomplete: judgment.IncompleteReasonMessageOverLimit}, nil
	}
	low, high := 1, n
	for low < high {
		mid := (low + high + 1) / 2
		s, ok, err := fits(mid)
		if err != nil {
			return nil, err
		}
		if ok {
			low, best = mid, s
		} else {
			high = mid - 1
		}
	}
	c := r.Context
	p := &Plan{
		context: c, win: best.win, tree: best.tree, size: best.size, calls: best.calls, answered: map[int]callResult{},
		Read: judgment.Read{WindowStart: c.Entries[best.win.start].Key},
	}
	for _, i := range best.win.new {
		p.Read.NewMessages = append(p.Read.NewMessages, c.Entries[i].Key)
	}
	for _, rc := range best.win.rechecked {
		p.Read.Rechecked = append(p.Read.Rechecked, judgment.Recheck{Judgment: rc.commitment.Judgment, MessageKey: rc.commitment.MessageKey})
	}
	return p, nil
}

func (m *method) Measure(ctx context.Context, c threadctx.Context) (Size, error) {
	if len(c.Entries) == 0 {
		return Size{}, errors.New("measure: thread context has no messages")
	}
	whole, err := m.measureWindow(ctx, Request{Context: c}, len(c.Entries))
	if err != nil {
		return Size{}, err
	}
	newest, err := m.measureWindow(ctx, Request{Context: c}, 1)
	if err != nil {
		return Size{}, err
	}
	whole.size.NewestFits = newest.size.Tokens <= m.def.Limits.Tokens
	return whole.size, nil
}

func (m *method) Judge(ctx context.Context, p *Plan) (Result, error) {
	if p.Incomplete != "" {
		return Incomplete{Reason: p.Incomplete}, nil
	}
	judgedAt := accelerated.GetCurrentTime().UTC()
	win := windowed(p.context, p.win.start)
	for i, questions := range p.calls {
		if _, done := p.answered[i]; done {
			continue
		}
		prep, err := m.prepare(ctx, win, questions)
		if err != nil {
			return nil, err
		}
		started := time.Now() //nolint:forbidigo // wall-clock time for call latency
		answers, err := prep.send(ctx)
		latency := time.Since(started) //nolint:forbidigo // wall-clock time for call latency
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// best-effort: a failed call is reported as call_failed; the error may
		// echo request content, so it goes nowhere.
		if err != nil {
			return Incomplete{Reason: judgment.IncompleteReasonCallFailed}, nil
		}
		p.answered[i] = callResult{answers: answers, request: prep.wire, latency: latency}
	}
	merged := map[string]Answer{}
	usage := judgment.Usage{Questions: p.size.Questions}
	billed, cost := 0, 0.0
	allBilled, allCosted := true, true
	for i := range p.calls {
		r := p.answered[i]
		for id, a := range r.answers.ByQuestion {
			merged[id] = a
		}
		usage.ContextTokens += countTokens(r.request)
		usage.Latency += r.latency
		if r.answers.BilledTokens == nil {
			allBilled = false
		} else {
			billed += *r.answers.BilledTokens
		}
		if r.answers.CostUSD == nil {
			allCosted = false
		} else {
			cost += *r.answers.CostUSD
		}
	}
	if len(p.calls) > 0 && allBilled {
		usage.BilledTokens = &billed
	}
	if len(p.calls) > 0 && allCosted {
		usage.CostUSD = &cost
	}
	got, err := p.tree.answer(p.context, merged)
	// best-effort: a response that leaves a question unanswered is a failed
	// call, reported as call_failed.
	if err != nil {
		p.answered = map[int]callResult{}
		return Incomplete{Reason: judgment.IncompleteReasonCallFailed}, nil
	}
	return Judged{judgment.Judgment{
		Method:      m.ref,
		Thread:      p.context.Thread,
		Kind:        p.context.Kind,
		CutOff:      p.context.CutOff,
		JudgedAt:    judgedAt,
		InputHash:   p.context.InputHash,
		WindowStart: p.Read.WindowStart,
		NewMessages: slices.Clone(p.Read.NewMessages),
		Flags:       got.flags,
		Clearings:   got.clearings,
		Questions:   got.questions,
		Outcomes:    got.outcomes,
		Usage:       usage,
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
