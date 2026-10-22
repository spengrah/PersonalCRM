// Package decision holds the decision-model methods: how one method judges
// the window of one thread context in as few calls as its limits allow, for
// every compared method, and the registry their keys come from.
package decision

import (
	"cmp"
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
// messages preceded by as many older messages as fit with the commitments it
// re-checks, and only the last chunk asking ball-in-court. A later chunk
// re-checks what the chunks before it left open, so its window is sized when
// it is judged, and the plan's Read names it by its new messages and the
// night's Chunking.
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
	// earlier are the request's commitments, which any chunk may re-check.
	earlier []Commitment
	floor   float64
	chunks  []*chunk
}

// chunk is one window of a plan and what judging it has answered so far.
type chunk struct {
	// group is the new messages the chunk flags.
	group []int
	// win is the chunk's window, with every commitment it re-checks. A plan's
	// first window is planned; a later chunk's is sized when it is judged,
	// once the chunks before it have answered.
	win      *window
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
	def Definition
	ref judgment.MethodRef
	// cover readies a context for prepare: a masking variant covers it with
	// name spans, failing with threadctx.ErrNoNameSpans when it cannot.
	cover   func(ctx context.Context, c threadctx.Context) (threadctx.Context, error)
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
// does with the definition. masking is the version of the masking its prepare
// applies.
func newMethod(def Definition, r Route, masking string, cover func(context.Context, threadctx.Context) (threadctx.Context, error), prepare func(context.Context, threadctx.Context, []threadctx.Question) (prepared, error)) *method {
	if def.Sizing.Ratio <= 0 || def.Sizing.Margin < 0 {
		panic(fmt.Sprintf("decision: method %q needs a positive sizing ratio and a margin of at least zero", def.Key))
	}
	def.Wording = def.Wording.clone()
	return &method{def: def, ref: def.ref(r, masking), cover: cover, prepare: prepare}
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
	n := len(r.Context.Entries)
	if n == 0 {
		return nil, errors.New("plan: thread context has no messages")
	}
	c, err := m.cover(ctx, r.Context)
	// best-effort: a context the name model cannot cover is a failed call,
	// reported call_failed; the cause may name the model's failure, so it goes
	// nowhere.
	if errors.Is(err, threadctx.ErrNoNameSpans) {
		return &Plan{Incomplete: judgment.IncompleteReasonCallFailed}, nil
	}
	if err != nil {
		return nil, err
	}
	r.Context = c
	last := n - 1
	whole, ok, err := m.widest(ctx, r, last, last, func(start int) []int { return newIn(r, start, last) })
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Plan{Incomplete: judgment.IncompleteReasonMessageOverLimit}, nil
	}
	p := &Plan{id: uuid.New(), context: c, earlier: r.Commitments, floor: r.Floor, chunks: []*chunk{{group: whole.new, win: &whole}}}
	if news := newIn(r, 0, last); r.NewAfter != nil && len(news) > 0 && whole.start > news[0] {
		groups, over, err := m.chunk(ctx, r, news)
		if err != nil {
			return nil, err
		}
		p.chunks = nil
		for _, g := range groups {
			p.chunks = append(p.chunks, &chunk{group: g})
		}
		if p.chunks[0].win, err = m.window(ctx, p, 0); err != nil {
			return nil, err
		}
		for _, i := range over {
			p.OverLimit = append(p.OverLimit, c.Entries[i].Key)
		}
	}
	p.Read = p.read(p.chunks[0])
	p.Read.NewMessages = nil
	for _, ch := range p.chunks {
		p.Read.NewMessages = append(p.Read.NewMessages, p.read(ch).NewMessages...)
	}
	if len(p.chunks) > 1 {
		p.Read.Chunks = p.reads()
		p.Read.Chunking = m.chunking(r)
	}
	return p, nil
}

// read is what a chunk reads: its new messages and, once its window is sized,
// the window's start and the earlier judgments' commitments it re-checks.
func (p *Plan) read(ch *chunk) judgment.Read {
	key := func(i int) string { return p.context.Entries[i].Key }
	var read judgment.Read
	for _, i := range ch.group {
		read.NewMessages = append(read.NewMessages, key(i))
	}
	if ch.win == nil {
		return read
	}
	read.WindowStart = key(ch.win.start)
	for _, rc := range ch.win.rechecked {
		if rc.commitment.Judgment != p.id {
			read.Rechecked = append(read.Rechecked, judgment.Recheck{Judgment: rc.commitment.Judgment, MessageKey: rc.commitment.MessageKey})
		}
	}
	return read
}

// reads returns what each chunk of the plan reads, earliest first.
func (p *Plan) reads() []judgment.Read {
	out := make([]judgment.Read, len(p.chunks))
	for i, ch := range p.chunks {
		out[i] = p.read(ch)
	}
	return out
}

// chunking identifies what decides a chunked night's later windows besides
// the answers of the chunks before them: the method's token limit and sizing,
// its clearing floor and the request's commitments.
func (m *method) chunking(r Request) string {
	earlier := slices.Clone(r.Commitments)
	slices.SortFunc(earlier, func(a, b Commitment) int {
		return cmp.Or(slices.Compare(a.Judgment[:], b.Judgment[:]), cmp.Compare(a.MessageKey, b.MessageKey))
	})
	return digest(struct {
		Tokens      int
		Sizing      Sizing
		Floor       float64
		Commitments []Commitment
	}{m.def.Limits.Tokens, m.def.Sizing, r.Floor, earlier})
}

// chunk divides the new messages of a night no one window holds into the
// groups of the fewest chunks that fit, earliest first. A new message whose
// window alone does not fit is over the limit, and the chunks are planned
// around it.
func (m *method) chunk(ctx context.Context, r Request, news []int) ([][]int, []int, error) {
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
	var groups [][]int
	for _, run := range runs {
		spread, err := m.spread(ctx, r, run)
		if err != nil {
			return nil, nil, err
		}
		groups = append(groups, spread...)
	}
	return groups, over, nil
}

// spread divides a run of consecutive new messages, each of which fits a
// window alone, into the fewest groups whose sizes differ by at most one and
// whose windows each fit with no older message. Of the divisions into that
// many groups, it takes the first that fits, smaller groups first.
func (m *method) spread(ctx context.Context, r Request, run []int) ([][]int, error) {
	fits := map[[2]int]bool{}
	fit := func(from, to int) (bool, error) {
		if ok, seen := fits[[2]int{from, to}]; seen {
			return ok, nil
		}
		g := run[from:to]
		ok, err := m.fits(ctx, r.Context, windowOf(r, g[0], g[len(g)-1], g))
		fits[[2]int{from, to}] = ok
		return ok, err
	}
	for k := 1; ; k++ {
		bounds, ok, err := balanced(len(run), k, fit)
		if err != nil {
			return nil, err
		}
		if ok {
			groups := make([][]int, len(bounds))
			for i, b := range bounds {
				groups[i] = run[b[0]:b[1]]
			}
			return groups, nil
		}
	}
}

// balanced returns the first division of n items, in order, into k parts
// whose sizes differ by at most one, smaller parts first, every part of which
// fit accepts, as each part's bounds. It reports false when no such division
// does.
func balanced(n, k int, fit func(from, to int) (bool, error)) ([][2]int, bool, error) {
	small := n / k
	// dead holds the (start, parts left) from which no division fits.
	dead := map[[2]int]bool{}
	var from func(start, left int) ([][2]int, bool, error)
	from = func(start, left int) ([][2]int, bool, error) {
		if left == 0 {
			return nil, true, nil
		}
		if dead[[2]int{start, left}] {
			return nil, false, nil
		}
		for _, size := range []int{small, small + 1} {
			if rest := n - start - size; rest < small*(left-1) || rest > (small+1)*(left-1) {
				continue
			}
			ok, err := fit(start, start+size)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
			tail, ok, err := from(start+size, left-1)
			if err != nil {
				return nil, false, err
			}
			if ok {
				return append([][2]int{{start, start + size}}, tail...), true, nil
			}
		}
		dead[[2]int{start, left}] = true
		return nil, false, nil
	}
	return from(0, k)
}

func (m *method) Measure(ctx context.Context, c threadctx.Context) (Size, error) {
	if len(c.Entries) == 0 {
		return Size{}, errors.New("measure: thread context has no messages")
	}
	c, err := m.cover(ctx, c)
	if err != nil {
		return Size{}, fmt.Errorf("measure: %w", err)
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

// window sizes the plan's chunk i: the widest window through its last new
// message that fits with every commitment inside it the chunk re-checks, the
// request's commitments and those the chunks before it flagged above the
// clearing floor, each unless a chunk before it answered it done. The window
// starting at the chunk's first new message always fits: the chunk's group
// was divided to fit alone, and no commitment lies among new messages.
func (m *method) window(ctx context.Context, p *Plan, i int) (*window, error) {
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
	var open []Commitment
	for _, c := range p.earlier {
		if !done[c.MessageKey] {
			open = append(open, c)
		}
	}
	for _, key := range flagged {
		if !done[key] {
			open = append(open, Commitment{Judgment: p.id, MessageKey: key})
		}
	}
	g := p.chunks[i].group
	win, _, err := m.widest(ctx, Request{Context: p.context, Commitments: open}, g[0], g[len(g)-1], func(int) []int { return g })
	return &win, err
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
			if ch.win == nil {
				win, err := m.window(ctx, p, i)
				if err != nil {
					return nil, err
				}
				ch.win = win
			}
			t := buildTree(p.context, m.def.Wording, *ch.win)
			ch.tree, ch.calls, ch.answered = &t, split(t.questions, m.def.Limits.Questions), map[int]callResult{}
		}
		win := windowed(p.context, *ch.win)
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
		// chunk, every later window sized again from the answers it gets.
		if err != nil {
			for j, ch := range p.chunks {
				win := ch.win
				*ch = chunk{group: ch.group}
				if j == 0 {
					ch.win = win
				}
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
	var chunks []judgment.Read
	if len(p.chunks) > 1 {
		chunks = p.reads()
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
		Chunks:      chunks,
		Chunking:    p.Read.Chunking,
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
