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
	// Floor is the method's clearing floor, which a commitment an earlier
	// chunk of the same night flags must exceed to be re-checked.
	Floor float64
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
//
// A night whose new messages fit one window is read over that window. A night
// whose new messages do not is read in chunks: the fewest that fit, earliest
// first, the new messages spread evenly over them, each chunk's window its new
// messages preceded by as many older messages as fit, and only the last chunk
// asking ball-in-court.
type Plan struct {
	// Incomplete is set when the method cannot send the request, and then
	// holds the reason; the plan reads nothing.
	Incomplete judgment.IncompleteReason
	Read       judgment.Read
	// OverLimit are the keys of the new messages that alone exceed the
	// method's limits: no window reads them and the judgment answers none.
	OverLimit []string

	id      uuid.UUID
	context threadctx.Context
	floor   float64
	chunks  []*chunk
}

// chunk is one window of a plan and what judging it has answered so far. Its
// questions are built when it is judged, since it re-checks the commitments
// the chunks before it flagged.
type chunk struct {
	win window
	// asked is the window as judged, with every commitment it re-checks.
	asked    window
	tree     *tree
	calls    [][]threadctx.Question
	answered map[int]callResult
	got      *answers
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

// measure builds a window's question tree and sizes its request, all its
// questions in one request, by the method's sizing.
func (m *method) measure(ctx context.Context, c threadctx.Context, win window) (tree, int, error) {
	t := buildTree(c, m.def.Wording, win)
	p, err := m.prepare(ctx, windowed(c, win), t.questions)
	if err != nil {
		return tree{}, 0, err
	}
	return t, m.def.Sizing.scale(p.wire), nil
}

// fits reports whether a window's request, all its questions in one request,
// fits the method's token limit.
func (m *method) fits(ctx context.Context, c threadctx.Context, win window) (bool, error) {
	_, tokens, err := m.measure(ctx, c, win)
	return tokens <= m.def.Limits.Tokens, err
}

// windowOf returns the window of the request's entries start through end that
// flags the entries flagged, re-checks the request's commitments inside it and,
// when it ends at the context's newest entry, asks ball-in-court.
func windowOf(r Request, start, end int, flagged []int) window {
	c := r.Context
	win := window{start: start, end: end, new: flagged, ballInCourt: end == len(c.Entries)-1}
	for _, commitment := range r.Commitments {
		for i := start; i <= end; i++ {
			if c.Entries[i].Key == commitment.MessageKey {
				win.rechecked = append(win.rechecked, recheck{entry: i, commitment: commitment})
				break
			}
		}
	}
	sort.SliceStable(win.rechecked, func(a, b int) bool { return win.rechecked[a].entry < win.rechecked[b].entry })
	return win
}

// newIn returns the request's entries start through end that are new at its
// cut-off.
func newIn(r Request, start, end int) []int {
	var out []int
	for i := start; i <= end; i++ {
		if r.NewAfter == nil || r.Context.Entries[i].SentAt.After(*r.NewAfter) {
			out = append(out, i)
		}
	}
	return out
}

// widest returns the window through end that starts earliest, at or before
// latest, and fits, flagging the entries flagged gives for its start. It
// reports false when even the window starting at latest does not fit. The
// request grows with its window, so the start is found by bisection.
func (m *method) widest(ctx context.Context, r Request, latest, end int, flagged func(start int) []int) (window, bool, error) {
	at := func(start int) (window, bool, error) {
		win := windowOf(r, start, end, flagged(start))
		ok, err := m.fits(ctx, r.Context, win)
		return win, ok, err
	}
	best, ok, err := at(latest)
	if err != nil || !ok {
		return best, ok, err
	}
	low, high := 0, latest
	for low < high {
		mid := (low + high) / 2
		win, ok, err := at(mid)
		if err != nil {
			return window{}, false, err
		}
		if ok {
			high, best = mid, win
		} else {
			low = mid + 1
		}
	}
	return best, true, nil
}

// windowed is the context as a window reads it: its entries start through
// end, read up to the last of them.
func windowed(c threadctx.Context, win window) threadctx.Context {
	if win.end < len(c.Entries)-1 {
		c.CutOff = c.Entries[win.end].SentAt
	}
	c.Entries = c.Entries[win.start : win.end+1]
	return c
}

// parts divides items into n parts in order, their sizes differing by at most
// one.
func parts[T any](items []T, n int) [][]T {
	out := make([][]T, 0, n)
	for i := range n {
		out = append(out, items[i*len(items)/n:(i+1)*len(items)/n])
	}
	return out
}

// split divides questions into the fewest calls of at most limit questions,
// as equal as it can.
func split(questions []threadctx.Question, limit int) [][]threadctx.Question {
	return parts(questions, (len(questions)+limit-1)/limit)
}

func (m *method) Plan(ctx context.Context, r Request) (*Plan, error) {
	c := r.Context
	n := len(c.Entries)
	if n == 0 {
		return nil, errors.New("plan: thread context has no messages")
	}
	last := n - 1
	whole, ok, err := m.widest(ctx, r, last, last, func(start int) []int { return newIn(r, start, last) })
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Plan{Incomplete: judgment.IncompleteReasonMessageOverLimit}, nil
	}
	p := &Plan{id: uuid.New(), context: c, floor: r.Floor, chunks: []*chunk{{win: whole}}}
	var over []int
	if news := newIn(r, 0, last); r.NewAfter != nil && len(news) > 0 && whole.start > news[0] {
		if p.chunks, over, err = m.chunk(ctx, r, news); err != nil {
			return nil, err
		}
	}
	key := func(i int) string { return c.Entries[i].Key }
	p.Read.WindowStart = key(p.chunks[0].win.start)
	for _, ch := range p.chunks {
		read := judgment.Read{WindowStart: key(ch.win.start)}
		for _, i := range ch.win.new {
			read.NewMessages = append(read.NewMessages, key(i))
		}
		for _, rc := range ch.win.rechecked {
			read.Rechecked = append(read.Rechecked, judgment.Recheck{Judgment: rc.commitment.Judgment, MessageKey: rc.commitment.MessageKey})
		}
		p.Read.NewMessages = append(p.Read.NewMessages, read.NewMessages...)
		for _, rc := range read.Rechecked {
			if !slices.Contains(p.Read.Rechecked, rc) {
				p.Read.Rechecked = append(p.Read.Rechecked, rc)
			}
		}
		if len(p.chunks) > 1 {
			p.Read.Chunks = append(p.Read.Chunks, read)
		}
	}
	for _, i := range over {
		p.OverLimit = append(p.OverLimit, key(i))
	}
	return p, nil
}

// chunk plans the chunks of a night whose new messages no one window holds:
// the fewest windows that fit, earliest first, each its new messages preceded
// by as many older messages as fit. A new message whose window alone does not
// fit is over the limit, and the chunks are planned around it.
func (m *method) chunk(ctx context.Context, r Request, news []int) ([]*chunk, []int, error) {
	var runs [][]int
	var over, run []int
	for _, i := range news {
		ok, err := m.fits(ctx, r.Context, windowOf(r, i, i, []int{i}))
		if err != nil {
			return nil, nil, err
		}
		if ok {
			run = append(run, i)
			continue
		}
		over = append(over, i)
		if len(run) > 0 {
			runs, run = append(runs, run), nil
		}
	}
	if len(run) > 0 {
		runs = append(runs, run)
	}
	var chunks []*chunk
	for _, run := range runs {
		groups, err := m.spread(ctx, r, run)
		if err != nil {
			return nil, nil, err
		}
		for _, g := range groups {
			win, _, err := m.widest(ctx, r, g[0], g[len(g)-1], func(int) []int { return g })
			if err != nil {
				return nil, nil, err
			}
			chunks = append(chunks, &chunk{win: win})
		}
	}
	return chunks, over, nil
}

// spread divides a run of consecutive new messages, each of which fits a
// window alone, into the fewest groups whose sizes differ by at most one and
// whose windows each fit with no older message.
func (m *method) spread(ctx context.Context, r Request, run []int) ([][]int, error) {
	for k := 1; ; k++ {
		groups := parts(run, k)
		fit := true
		for _, g := range groups {
			ok, err := m.fits(ctx, r.Context, windowOf(r, g[0], g[len(g)-1], g))
			if err != nil {
				return nil, err
			}
			if !ok {
				fit = false
				break
			}
		}
		if fit {
			return groups, nil
		}
	}
}

func (m *method) Measure(ctx context.Context, c threadctx.Context) (Size, error) {
	if len(c.Entries) == 0 {
		return Size{}, errors.New("measure: thread context has no messages")
	}
	r := Request{Context: c}
	last := len(c.Entries) - 1
	whole, tokens, err := m.measure(ctx, c, windowOf(r, 0, last, newIn(r, 0, last)))
	if err != nil {
		return Size{}, err
	}
	_, newest, err := m.measure(ctx, c, windowOf(r, last, last, []int{last}))
	if err != nil {
		return Size{}, err
	}
	limits := m.def.Limits
	return Size{
		Tokens: tokens, Questions: len(whole.questions), Calls: len(split(whole.questions, limits.Questions)),
		TokenLimit: limits.Tokens, QuestionLimit: limits.Questions, NewestFits: newest <= limits.Tokens,
	}, nil
}

// rechecks returns the commitments the plan's chunk i re-checks, in thread
// order: the earlier commitments inside its window and the commitments the
// chunks before it flagged above the clearing floor inside its window, each
// unless a chunk before it answered it done.
func (p *Plan) rechecks(i int) []recheck {
	win := p.chunks[i].win
	done := map[string]bool{}
	var flagged []string
	for _, ch := range p.chunks[:i] {
		for _, f := range ch.got.flags {
			if f.IsCommitmentAbove(p.floor) {
				flagged = append(flagged, f.MessageKey)
			}
		}
		for _, c := range ch.got.clearings {
			done[c.CommitmentMessageKey] = c.Done
		}
	}
	var out []recheck
	for _, rc := range win.rechecked {
		if !done[rc.commitment.MessageKey] {
			out = append(out, rc)
		}
	}
	for _, key := range flagged {
		entry := slices.IndexFunc(p.context.Entries, func(e threadctx.Entry) bool { return e.Key == key })
		if !done[key] && entry >= win.start && entry <= win.end {
			out = append(out, recheck{entry: entry, commitment: Commitment{Judgment: p.id, MessageKey: key}})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].entry < out[b].entry })
	return out
}

// calls divides a window's questions into the fewest calls that each fit the
// method's limits. A window planned with its questions needs only the calls
// its question limit asks for; a chunk that re-checks commitments an earlier
// chunk of its night flagged may need further calls over the same window.
func (m *method) calls(ctx context.Context, c threadctx.Context, questions []threadctx.Question) ([][]threadctx.Question, error) {
	for n := (len(questions) + m.def.Limits.Questions - 1) / m.def.Limits.Questions; n < len(questions); n++ {
		calls := parts(questions, n)
		fit := true
		for _, q := range calls {
			prep, err := m.prepare(ctx, c, q)
			if err != nil {
				return nil, err
			}
			if m.def.Sizing.scale(prep.wire) > m.def.Limits.Tokens {
				fit = false
				break
			}
		}
		if fit {
			return calls, nil
		}
	}
	return parts(questions, len(questions)), nil
}

func (m *method) Judge(ctx context.Context, p *Plan) (Result, error) {
	if p.Incomplete != "" {
		return Incomplete{Reason: p.Incomplete}, nil
	}
	judgedAt := accelerated.GetCurrentTime().UTC()
	for i, ch := range p.chunks {
		if ch.got != nil {
			continue
		}
		if ch.tree == nil {
			ch.asked = ch.win
			ch.asked.rechecked = p.rechecks(i)
			t := buildTree(p.context, m.def.Wording, ch.asked)
			calls, err := m.calls(ctx, windowed(p.context, ch.asked), t.questions)
			if err != nil {
				return nil, err
			}
			ch.tree, ch.calls, ch.answered = &t, calls, map[int]callResult{}
		}
		win := windowed(p.context, ch.asked)
		for j, questions := range ch.calls {
			if _, done := ch.answered[j]; done {
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
			ch.answered[j] = callResult{answers: answers, request: prep.wire, latency: latency}
		}
		merged := map[string]Answer{}
		for j := range ch.calls {
			for id, a := range ch.answered[j].answers.ByQuestion {
				merged[id] = a
			}
		}
		got, err := ch.tree.answer(p.context, merged)
		// best-effort: a response that leaves a question unanswered is a failed
		// call, reported as call_failed; the plan is asked again from its first
		// chunk.
		if err != nil {
			for _, ch := range p.chunks {
				*ch = chunk{win: ch.win}
			}
			return Incomplete{Reason: judgment.IncompleteReasonCallFailed}, nil
		}
		ch.got = &got
	}
	var out answers
	at := map[string]int{}
	usage := judgment.Usage{}
	calls, billed, cost := 0, 0, 0.0
	allBilled, allCosted := true, true
	for _, ch := range p.chunks {
		out.questions = append(out.questions, ch.got.questions...)
		out.outcomes.BallInCourt = append(out.outcomes.BallInCourt, ch.got.outcomes.BallInCourt...)
		out.flags = append(out.flags, ch.got.flags...)
		// A commitment a later chunk re-checks again carries that chunk's answer.
		for _, c := range ch.got.clearings {
			if i, ok := at[c.CommitmentMessageKey]; ok {
				out.clearings[i] = c
				continue
			}
			at[c.CommitmentMessageKey] = len(out.clearings)
			out.clearings = append(out.clearings, c)
		}
		usage.Questions += len(ch.tree.questions)
		calls += len(ch.calls)
		for j := range ch.calls {
			r := ch.answered[j]
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
	}
	if calls > 0 && allBilled {
		usage.BilledTokens = &billed
	}
	if calls > 0 && allCosted {
		usage.CostUSD = &cost
	}
	return Judged{judgment.Judgment{
		ID:          p.id,
		Method:      m.ref,
		Thread:      p.context.Thread,
		Kind:        p.context.Kind,
		CutOff:      p.context.CutOff,
		JudgedAt:    judgedAt,
		InputHash:   p.context.InputHash,
		WindowStart: p.Read.WindowStart,
		NewMessages: slices.Clone(p.Read.NewMessages),
		Chunks:      p.Read.Chunks,
		Flags:       out.flags,
		Clearings:   out.clearings,
		Questions:   out.questions,
		Outcomes:    out.outcomes,
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
