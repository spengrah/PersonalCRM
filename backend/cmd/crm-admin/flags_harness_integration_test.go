//go:build integration_testdb

// Harness for the message-flag tests: synthetic threads in January 2026, each
// compared method's vendor faked at its API boundary (an HTTP server speaking
// the vendor's wire protocol), and crm-admin --judge-month and --scorecard
// driven through the real run() dispatch.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scorecard"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const januaryMonth = "2026-01"

// jan is a time in January 2026, in the server's timezone.
func jan(day, hour int) time.Time {
	return time.Date(2026, time.January, day, hour, 0, 0, 0, time.Local)
}

// flagsEnv is the judgment store over an isolated clone with the vendors a
// test fakes.
type flagsEnv struct {
	*labelsEnv
	masker *threadctx.Masker
}

func newFlagsEnv(t *testing.T, production string) *flagsEnv {
	t.Helper()
	return &flagsEnv{labelsEnv: newLabelsEnvFor(t, production), masker: threadctx.NewMasker(nil)}
}

// ask is one question a vendor receives, with the message it concerns.
type ask struct {
	ID    string
	Kind  string // flag, clearing or ball_in_court
	Flag  judgment.FlagKind
	Ref   string
	Text  string
	After []string // the texts of the window's messages after Ref
}

// call is one request a vendor received: the window's message texts in order
// and the questions asked of it.
type call struct {
	// StateBytes is the size of the rendered state the vendor read.
	StateBytes int
	Texts      []string
	Asks       []ask
}

func (c call) asks(kind string) []ask {
	var out []ask
	for _, a := range c.Asks {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// vendor is a fake vendor: an HTTP server that answers every question by the
// policy a test sets, and records every request.
type vendor struct {
	url    string
	sage   bool
	mu     sync.Mutex
	calls  []call
	flag   func(a ask) float64 // the yes probability of a flag question
	clear  func(a ask) float64 // the done probability of a clearing question
	bic    func(role string) string
	fail   func(c call) bool
	billed int
}

func newVendor(t *testing.T, sage bool) *vendor {
	t.Helper()
	v := &vendor{
		sage:  sage,
		flag:  func(ask) float64 { return 0.05 },
		clear: func(ask) float64 { return 0.1 },
		bic:   func(string) string { return "nobody_owes" },
	}
	srv := httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(srv.Close)
	v.url = srv.URL
	return v
}

func (v *vendor) all() []call {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.calls)
}

func (v *vendor) reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = nil
}

var (
	entryHead   = regexp.MustCompile(`(?m)^\[m(\d+)\] \S+, [^\n]*\n`)
	flagID      = regexp.MustCompile(`^flag/([a-z_]+)/(m\d+)$`)
	clearingID  = regexp.MustCompile(`^clearing/(m\d+)$`)
	ballInCourt = regexp.MustCompile(`^ball_in_court/(.+)$`)
)

// parseState returns the text of each message the rendered state holds, by
// reference, and the references in order.
func parseState(state string) (map[string]string, []string) {
	heads := entryHead.FindAllStringSubmatchIndex(state, -1)
	texts, refs := map[string]string{}, []string{}
	for i, h := range heads {
		ref := "m" + state[h[2]:h[3]]
		end := len(state)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		texts[ref] = strings.TrimSpace(state[h[1]:end])
		refs = append(refs, ref)
	}
	return texts, refs
}

func (v *vendor) record(state string, ids []string) call {
	texts, refs := parseState(state)
	c := call{StateBytes: len(state)}
	for _, ref := range refs {
		c.Texts = append(c.Texts, texts[ref])
	}
	for _, id := range ids {
		a := ask{ID: id}
		switch {
		case flagID.MatchString(id):
			m := flagID.FindStringSubmatch(id)
			a.Kind, a.Flag, a.Ref = "flag", judgment.FlagKind(m[1]), m[2]
		case clearingID.MatchString(id):
			a.Kind, a.Ref = "clearing", clearingID.FindStringSubmatch(id)[1]
		case ballInCourt.MatchString(id):
			a.Kind, a.Ref = "ball_in_court", ballInCourt.FindStringSubmatch(id)[1]
		}
		if a.Kind != "ball_in_court" {
			a.Text = texts[a.Ref]
			if i := slices.Index(refs, a.Ref); i >= 0 {
				for _, later := range refs[i+1:] {
					a.After = append(a.After, texts[later])
				}
			}
		}
		c.Asks = append(c.Asks, a)
	}
	slices.SortFunc(c.Asks, func(a, b ask) int { return cmp.Compare(a.ID, b.ID) })
	return c
}

func (v *vendor) serve(w http.ResponseWriter, r *http.Request) {
	if v.sage {
		v.serveSage(w, r)
		return
	}
	var req struct {
		State     string `json:"state"`
		Questions map[string]struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		ids = append(ids, id)
	}
	c := v.record(req.State, ids)
	if !v.admit(c) {
		http.Error(w, "refused", http.StatusInternalServerError)
		return
	}
	answers := map[string]any{}
	for _, a := range c.Asks {
		q := req.Questions[a.ID]
		if q.Type == "noul" {
			answers[a.ID] = map[string]any{"type": "noul", "noul": v.yes(a)}
			continue
		}
		var criteria map[string]*string
		if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(criteria))
		for name := range criteria {
			names = append(names, name)
		}
		slices.Sort(names)
		probs, chosen := v.choice(a, names)
		answers[a.ID] = map[string]any{"type": "choice", "choice": chosen, "probabilities": probs}
	}
	writeTestJSON(w, map[string]any{"answers": answers, "usage": map[string]any{"input_tokens": v.billedFor(req.State)}})
}

func (v *vendor) serveSage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Requests []struct {
			Content   struct{ Value string } `json:"content"`
			Questions []struct {
				Kind    string `json:"kind"`
				ID      string `json:"id"`
				Options []struct {
					Option string `json:"option"`
				} `json:"options"`
			} `json:"questions"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	group := req.Requests[0]
	ids := make([]string, len(group.Questions))
	for i, q := range group.Questions {
		ids[i] = q.ID
	}
	c := v.record(group.Content.Value, ids)
	if !v.admit(c) {
		http.Error(w, "refused", http.StatusInternalServerError)
		return
	}
	byID := map[string]ask{}
	for _, a := range c.Asks {
		byID[a.ID] = a
	}
	var answers []any
	for _, q := range group.Questions {
		a := byID[q.ID]
		if q.Kind == "yesno" {
			answers = append(answers, map[string]any{"ok": true, "result": map[string]any{"id": q.ID, "kind": "yesno", "result": map[string]any{"answer": nil, "probability": v.yes(a)}}})
			continue
		}
		names := make([]string, len(q.Options))
		for i, o := range q.Options {
			names[i] = o.Option
		}
		probs, chosen := v.choice(a, names)
		var list []any
		for _, name := range names {
			list = append(list, map[string]any{"option": name, "probability": probs[name]})
		}
		answers = append(answers, map[string]any{"ok": true, "result": map[string]any{"id": q.ID, "kind": "choice", "result": map[string]any{"chosen": chosen, "probabilities": list}}})
	}
	writeTestJSON(w, map[string]any{
		"results": []any{map[string]any{"answers": answers}},
		"meta":    map[string]any{"usage": map[string]any{"billed_input_tokens": v.billedFor(group.Content.Value)}},
	})
}

// admit records the call and reports whether the vendor answers it.
func (v *vendor) admit(c call) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, c)
	return v.fail == nil || !v.fail(c)
}

func (v *vendor) yes(a ask) float64 {
	if a.Kind == "clearing" {
		return v.clear(a)
	}
	return v.flag(a)
}

// choice answers a ball-in-court question: the option v.bic names for the
// party, at 0.9.
func (v *vendor) choice(a ask, names []string) (map[string]float64, string) {
	want := v.bic(a.Ref)
	prefix := map[string]string{"i_owe_reply": "ME owes", "they_owe_reply": "THEM owes ME", "nobody_owes": "Nobody owes", "not_applicable": "Not a conversation"}[want]
	probs, chosen := map[string]float64{}, ""
	for _, name := range names {
		if chosen == "" && (strings.HasPrefix(name, prefix) || (want == "they_owe_reply" && strings.HasSuffix(name, "owes ME a reply"))) {
			chosen = name
		}
	}
	for _, name := range names {
		probs[name] = 0.1 / float64(len(names)-1)
	}
	probs[chosen] = 0.9
	return probs, chosen
}

func (v *vendor) billedFor(state string) int { return cmp.Or(v.billed, len(state)/4+1) }

func writeTestJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// methods is the registry a test hands the runner.
type methods map[string]decision.Method

func (m methods) Get(key string) (decision.Method, error) {
	if method, ok := m[key]; ok {
		return method, nil
	}
	return nil, errors.New("unknown method " + key)
}

func (m methods) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// fakeDefinition is a method definition with the given limits and an
// unscaled sizing.
func fakeDefinition(key string, limits decision.Limits) decision.Definition {
	return decision.Definition{Key: key, Wording: decision.DefaultWording, Limits: limits, Sizing: decision.Sizing{Ratio: 1}}
}

// roomy limits never window or split a test thread.
var roomy = decision.Limits{Tokens: 1 << 24, Questions: 1 << 20}

func (e *flagsEnv) method(key string, v *vendor, limits decision.Limits) decision.Method {
	return e.methodOf(fakeDefinition(key, limits), v)
}

func (e *flagsEnv) methodOf(def decision.Definition, v *vendor) decision.Method {
	if v.sage {
		return decision.NewMaskedMethod(def, e.masker, decision.NewSageAdapter(v.url, "key"))
	}
	return decision.NewMaskedMethod(def, e.masker, decision.NewSystemOneAdapter(v.url, "key", "fake-1"))
}

// judgeMonth runs crm-admin --judge-month over January with the method, and
// returns its output.
func (e *flagsEnv) judgeMonth(t *testing.T, set judgerun.Methods, key string) string {
	t.Helper()
	return e.judgeMonthOf(t, januaryMonth, set, key)
}

// judgeMonthOf is judgeMonth over another month, written YYYY-MM.
func (e *flagsEnv) judgeMonthOf(t *testing.T, month string, set judgerun.Methods, key string) string {
	t.Helper()
	out, err := e.tryJudgeMonth(month, set, key, false)
	require.NoError(t, err)
	return out
}

func (e *flagsEnv) tryJudgeMonth(month string, set judgerun.Methods, key string, measure bool) (string, error) {
	runner, err := judgerun.New(e.contexts, e.store, set)
	if err != nil {
		return "", err
	}
	var stdout bytes.Buffer
	err = run(e.ctx, runOptions{judgeMonth: month, judgeMethod: key, judgeMeasure: measure}, adminDeps{
		judgeMonth: runner, stdout: &stdout, stderr: &bytes.Buffer{},
	})
	return stdout.String(), err
}

// scorecard runs crm-admin --scorecard for the method.
func (e *flagsEnv) scorecard(t *testing.T, set judgerun.Methods, key string) string {
	t.Helper()
	runner, err := judgerun.New(e.contexts, e.store, set)
	require.NoError(t, err)
	var stdout bytes.Buffer
	err = run(e.ctx, runOptions{scorecard: true, judgeMethod: key}, adminDeps{
		scorecard: scorecard.New(e.store, runner, set, e.contacts, e.cfg.Watchdog),
		stdout:    &stdout, stderr: &bytes.Buffer{},
	})
	require.NoError(t, err)
	return stdout.String()
}

// threadRef is a thread of the env: its venue.
type threadRef struct {
	e     *flagsEnv
	venue threadctx.VenueID
}

// thr is an email thread with one synthetic contact.
type thr struct {
	threadRef
	cont    string
	contact uuid.UUID
	name    string
}

func (e *flagsEnv) newThread(t *testing.T) *thr {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	container := "thread-" + uuid.NewString()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	venue, err := repository.NewVenueRepository(e.database.Queries).ResolveVenueForInteraction(e.ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return &thr{threadRef: threadRef{e: e, venue: venue}, cont: container, contact: c.ID, name: c.FullName}
}

// say stores a message of the thread: the user's own, or the contact's.
func (th *thr) say(t *testing.T, at time.Time, fromUser bool, text string) {
	t.Helper()
	direction := repository.InteractionDirectionInbound
	if fromUser {
		direction = repository.InteractionDirectionOutbound
	}
	container, peer := th.cont, "peer-"+th.contact.String()+"@example.test"
	_, err := repository.NewCommsMessageRepository(th.e.database.Queries).UpsertMessage(th.e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container,
		Body: &text, PeerHandle: &peer, PeerNormalized: &peer, Direction: direction, SentAt: at,
		SourceMetadata: []byte("{}"), MatchedContactID: th.contact,
	})
	require.NoError(t, err)
}

// context builds the thread's content up to the cut-off.
func (th *threadRef) context(t *testing.T, cutOff time.Time) threadctx.Context {
	t.Helper()
	c, err := th.e.contexts.Build(th.e.ctx, th.venue, cutOff)
	require.NoError(t, err)
	return c
}

// keys returns the message keys of the thread's whole content, in order.
func (th *threadRef) keys(t *testing.T) []string {
	t.Helper()
	c := th.context(t, time.Date(2026, time.March, 1, 0, 0, 0, 0, time.Local))
	out := make([]string, len(c.Entries))
	for i, e := range c.Entries {
		out[i] = e.Key
	}
	return out
}

// judgments returns the thread's judgments of the method version, oldest cut-off
// first.
func (th *threadRef) judgments(t *testing.T, m judgment.MethodRef) []judgment.JudgmentView {
	t.Helper()
	views, err := th.e.store.ListJudgments(th.e.ctx, judgment.JudgmentQuery{Thread: &th.venue})
	require.NoError(t, err)
	var out []judgment.JudgmentView
	for _, v := range views {
		if v.Method == m {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b judgment.JudgmentView) int {
		return cmp.Or(a.CutOff.Compare(b.CutOff), a.JudgedAt.Compare(b.JudgedAt))
	})
	return out
}

// judgmentAt returns the one judgment of the method version at the cut-off.
func (th *threadRef) judgmentAt(t *testing.T, m judgment.MethodRef, cutOff time.Time) judgment.JudgmentView {
	t.Helper()
	var found []judgment.JudgmentView
	for _, v := range th.judgments(t, m) {
		if v.CutOff.Equal(cutOff) {
			found = append(found, v)
		}
	}
	require.Len(t, found, 1, "judgments of %s at %s", m.Key, cutOff)
	return found[0]
}

// flagsOf maps a judgment's flag decisions to message key and kind.
func flagsOf(v judgment.JudgmentView) probabilities {
	out := probabilities{}
	for _, f := range v.Flags {
		out[f.MessageKey+"/"+string(f.Kind)] = f.Probability
	}
	return out
}

// lastRun returns the newest run of the method.
func (e *flagsEnv) lastRun(t *testing.T, m judgment.MethodRef) judgment.RunSummary {
	t.Helper()
	runs, err := e.store.ListRuns(e.ctx)
	require.NoError(t, err)
	var last *judgment.RunSummary
	for _, r := range runs {
		if r.Method == m && (last == nil || r.StartedAt.After(last.StartedAt)) {
			last = &r
		}
	}
	require.NotNil(t, last)
	return *last
}

// noSend is an adapter that sends nothing: a method on it plans requests and
// cannot judge them.
type noSend struct{}

func (noSend) Route() decision.Route { return decision.Route{Protocol: "none", Endpoint: "none"} }
func (noSend) Send(context.Context, threadctx.MaskedRequest) (decision.Answers, error) {
	return decision.Answers{}, errors.New("nothing is sent")
}

// tokensFor returns the smallest token limit under which a method of the
// definition plans the request as one window starting at the message key
// start: the limit a window of exactly that size needs.
func (e *flagsEnv) tokensFor(t *testing.T, def decision.Definition, req decision.Request, start string) int {
	t.Helper()
	startsAt := func(limit int) (string, bool) {
		def.Limits.Tokens = limit
		p, err := decision.NewMaskedMethod(def, e.masker, noSend{}).Plan(e.ctx, req)
		require.NoError(t, err)
		return p.Read.WindowStart, p.Incomplete == "" && len(p.Read.Chunks) == 0
	}
	index := func(key string) int {
		return slices.IndexFunc(req.Context.Entries, func(e threadctx.Entry) bool { return e.Key == key })
	}
	low, high := 1, 1<<24
	for low < high {
		mid := (low + high) / 2
		if got, ok := startsAt(mid); ok && index(got) <= index(start) {
			high = mid
		} else {
			low = mid + 1
		}
	}
	got, ok := startsAt(low)
	require.True(t, ok && got == start, "no token limit gives a window starting at the message")
	return low
}

// meeting is a meeting with notes, attended by no one: its thread is a
// calendar event's venue.
type meeting struct {
	threadRef
	event uuid.UUID
}

func (e *flagsEnv) newMeeting(t *testing.T) *meeting {
	t.Helper()
	calendar := repository.NewCalendarEventRepository(e.database.Queries)
	title := "planning review"
	event, err := calendar.Upsert(e.ctx, repository.UpsertCalendarEventRequest{
		GcalEventID: "event|" + uuid.NewString(), GcalCalendarID: "calendar:" + uuid.NewString(), GoogleAccountID: "account-" + uuid.NewString(),
		Title: &title, StartTime: jan(2, 9), EndTime: jan(2, 10), Status: "confirmed", SyncedAt: jan(2, 9),
	})
	require.NoError(t, err)
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	venue, err := repository.NewVenueResolverRegistry(repository.NewVenueRepository(e.database.Queries), nil, calendar).ResolveGCalVenueTx(e.ctx, tx, event.ID)
	require.NoError(t, err)
	require.NotNil(t, venue)
	require.NoError(t, tx.Commit(e.ctx))
	return &meeting{threadRef: threadRef{e: e, venue: *venue}, event: event.ID}
}

// note stores a note of the meeting.
func (m *meeting) note(t *testing.T, at time.Time, text string) {
	t.Helper()
	tx, err := m.e.database.Pool.Begin(m.e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(m.e.ctx) }()
	linked := "event"
	_, err = repository.NewMeetingNoteRepository(m.e.database.Queries).InsertMeetingNoteTx(m.e.ctx, tx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: uuid.New(), Summary: &text, LinkedKind: &linked, LinkedID: &m.event,
		LinkageState: repository.LinkageStateLinked, MeetingAt: at,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(m.e.ctx))
}

// vendorTransport sends every request to the fake vendor serving its host.
type vendorTransport struct {
	vendors map[string]*vendor
	base    *http.Transport
}

func (vt *vendorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	v, ok := vt.vendors[r.URL.Host]
	if !ok {
		return nil, errors.New("no fake vendor serves " + r.URL.Host)
	}
	target, err := url.Parse(v.url)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
	return vt.base.RoundTrip(r)
}

// routeVendors makes the registry's real endpoints reach the fake vendors by
// host for the test.
func routeVendors(t *testing.T, vendors map[string]*vendor) {
	t.Helper()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = &vendorTransport{vendors: vendors, base: original.(*http.Transport)}
}
