package decision_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Synthetic people only.
var (
	juniper = uuid.MustParse("00000000-0000-4000-8000-0000000000b1")
	tobias  = uuid.MustParse("00000000-0000-4000-8000-0000000000b2")
	names   = []repository.ContactName{
		{ContactID: juniper, Name: "Juniper Okafor"},
		{ContactID: tobias, Name: "Tobias Wren"},
	}
	thread = uuid.MustParse("00000000-0000-4000-8000-0000000000c1")
)

// jan14 is a Wednesday afternoon in January 2026.
func jan14(hour int) time.Time { return time.Date(2026, 1, 14, hour, 0, 0, 0, time.UTC) }

// dm is a one-to-one thread with Juniper. Each text is a message, from the
// role it starts with ("ME: ", "PERSON_2: "), else from the other party.
func dm(texts ...string) threadctx.Context {
	c := threadctx.Context{
		Thread: thread, Kind: threadctx.KindConversation, CutOff: jan14(23), InputHash: "hash-dm",
		Participants: map[threadctx.Role]threadctx.Participant{
			threadctx.RoleMe:   {IsUser: true},
			threadctx.RoleThem: {ContactID: &juniper},
		},
	}
	c.Entries = entries(threadctx.RoleThem, texts)
	return c
}

var rolePrefix = regexp.MustCompile(`^(ME|THEM|PERSON_\d+)$`)

func entries(other threadctx.Role, texts []string) []threadctx.Entry {
	out := make([]threadctx.Entry, len(texts))
	for i, text := range texts {
		role := other
		if prefix, after, ok := strings.Cut(text, ": "); ok && rolePrefix.MatchString(prefix) {
			role, text = threadctx.Role(prefix), after
		}
		out[i] = threadctx.Entry{Key: "synthetic:" + string(rune('a'+i)), Role: role, SentAt: jan14(9 + i), Text: text}
	}
	return out
}

// group is a group thread of the user, Juniper (PERSON_2), Tobias (PERSON_3)
// and PERSON_4, who is no contact.
func group(texts ...string) threadctx.Context {
	c := threadctx.Context{
		Thread: thread, Kind: threadctx.KindConversation, CutOff: jan14(23), InputHash: "hash-group",
		Participants: map[threadctx.Role]threadctx.Participant{
			threadctx.RoleMe:        {IsUser: true},
			threadctx.PersonRole(2): {ContactID: &juniper},
			threadctx.PersonRole(3): {ContactID: &tobias},
			threadctx.PersonRole(4): {Aliases: []string{"Rafa"}},
		},
	}
	c.Entries = entries(threadctx.PersonRole(4), texts)
	return c
}

// answerer answers one masked question.
type answerer func(q threadctx.Question) decision.Answer

// fake is a vendor adapter that records what it is sent and answers each
// question with its answerer.
type fake struct {
	answer   answerer
	err      error
	requests []threadctx.MaskedRequest
}

func (f *fake) Route() decision.Route {
	return decision.Route{Protocol: "fake", Endpoint: "https://vendor.example.test/decide", Model: "fake-1"}
}

func (f *fake) Send(_ context.Context, req threadctx.MaskedRequest) (decision.Answers, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return decision.Answers{}, f.err
	}
	out := decision.Answers{ByQuestion: map[string]decision.Answer{}}
	for _, q := range req.Questions() {
		out.ByQuestion[q.ID] = f.answer(q)
	}
	return out, nil
}

// pick answers a question with the first option the predicate accepts at
// probability p, the rest of the mass spread over the other options.
func pick(q threadctx.Question, p float64, accept func(threadctx.Option) bool) decision.Answer {
	a := decision.Answer{Probabilities: map[string]float64{}}
	for _, o := range q.Options {
		if a.Chosen == "" && accept(o) {
			a.Chosen, a.Probabilities[o.Key] = o.Key, p
		}
	}
	for _, o := range q.Options {
		if o.Key != a.Chosen {
			a.Probabilities[o.Key] = (1 - p) / float64(len(q.Options)-1)
		}
	}
	return a
}

// plain answers every question with its first option.
func plain(q threadctx.Question) decision.Answer {
	return pick(q, 0.9, func(threadctx.Option) bool { return true })
}

var errEndpoint = errors.New("endpoint unavailable")

func definition(limits decision.Limits) decision.Definition {
	return decision.Definition{Key: "test-method", Wording: decision.DefaultWording, Limits: limits, Sizing: decision.Sizing{Ratio: 1}}
}

// planAndJudge plans the context at its thread's first judged cut-off and judges the
// plan.
func planAndJudge(ctx context.Context, m decision.Method, c threadctx.Context) (decision.Result, error) {
	p, err := m.Plan(ctx, decision.Request{Context: c})
	if err != nil {
		return nil, err
	}
	return m.Judge(ctx, p)
}

var roomy = decision.Limits{Tokens: 1 << 20, Questions: 64}

func newMethod(def decision.Definition, f *fake) decision.Method {
	return decision.NewMaskedMethod(def, threadctx.NewMasker(names), f)
}

func judged(t *testing.T, res decision.Result) judgment.Judgment {
	t.Helper()
	j, ok := res.(decision.Judged)
	require.True(t, ok, "result is %#v, not a judgment", res)
	return j.Judgment
}

func answered(j judgment.Judgment, id string) judgment.AnsweredQuestion {
	for _, q := range j.Questions {
		if q.ID == id {
			return q
		}
	}
	return judgment.AnsweredQuestion{}
}
