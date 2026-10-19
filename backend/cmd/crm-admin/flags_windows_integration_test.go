//go:build integration_testdb

package main

import (
	"fmt"
	"strings"
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const jevKey = "jev-masked"

// filler is the text of the i-th message of a synthetic thread: lowercase
// words only, every message of about one length.
func filler(i int) string {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa"}
	return fmt.Sprintf("note %s about the garden %s", words[i%len(words)], strings.Repeat("and the weather ", 6))
}

// chat stores n messages of one day on a thread of a new contact, alternating
// between the contact and the user.
func (e *flagsEnv) chat(t *testing.T, n int) *thr {
	t.Helper()
	th := e.newThread(t)
	for i := range n {
		th.say(t, jan(3, 9+i), i%2 == 1, filler(i))
	}
	return th
}

// A thread context over a method's limits at a cut-off is judged over its
// window: the newest whole messages ending at the cut-off whose text and every
// question fit the method's limits. The judgment records its window's first
// message, and a context that fits whole is judged over all of it.
// spec: KNW-055.fits-whole-sent-whole, KNW-055.over-limit-judged-over-window, KNW-055.no-message-split, KNW-055.newest-message-over-limit-incomplete, KNW-052.over-limit-context-windowed, KNW-052.message-over-limit-unsent
func TestJudgeMonthJudgesAThreadOverItsWindow(t *testing.T) {
	const n = 6
	cutOff := jan(4, 0)
	setup := func(t *testing.T) (*flagsEnv, *thr, []string, int) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, n)
		keys := th.keys(t)
		req := decision.Request{Context: th.context(t, cutOff)}
		return e, th, keys, e.tokensFor(t, fakeDefinition(jevKey, roomy), req, keys[0])
	}
	judgeWith := func(t *testing.T, e *flagsEnv, limit int) (*vendor, judgment.MethodRef, string) {
		v := newVendor(t, false)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})
		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)
		return v, m.Ref(), out
	}

	t.Run("a context that fits whole, sent with every message and its window starting at its first message", func(t *testing.T) {
		e, th, keys, whole := setup(t)

		v, ref, _ := judgeWith(t, e, whole)

		require.Len(t, v.all(), 1)
		assert.Len(t, v.all()[0].Texts, n, "every message is sent")
		j := th.judgmentAt(t, ref, cutOff)
		assert.Equal(t, keys[0], j.WindowStart)
		assert.Equal(t, keys, j.NewMessages)
	})

	t.Run("a context whose text fits but whose text and questions together do not, which is windowed", func(t *testing.T) {
		e, th, keys, whole := setup(t)
		roomyVendor := newVendor(t, false)
		e.judgeMonth(t, methods{jevKey: e.method(jevKey, roomyVendor, roomy)}, jevKey)
		text := roomyVendor.all()[0].StateBytes
		limit := text + 5000
		require.Less(t, limit, whole, "the text alone fits, the text with its questions does not")

		v, ref, _ := judgeWith(t, e, limit)

		j := th.judgmentAt(t, ref, cutOff)
		start := indexOf(keys, j.WindowStart)
		assert.Positive(t, start, "the window starts after the context's first message")
		assert.Equal(t, keys[start:], j.NewMessages)
		require.Len(t, v.all(), 1)
		assert.Len(t, v.all()[0].Texts, n-start)
		def := fakeDefinition(jevKey, roomy)
		req := decision.Request{Context: th.context(t, cutOff)}
		assert.LessOrEqual(t, e.tokensFor(t, def, req, keys[start]), limit, "the window fits")
		assert.Greater(t, e.tokensFor(t, def, req, keys[start-1]), limit, "the window is the newest messages that fit")
	})

	t.Run("a context one message over, which drops its oldest message whole and never splits one", func(t *testing.T) {
		e, th, keys, whole := setup(t)

		v, ref, _ := judgeWith(t, e, whole-1)

		j := th.judgmentAt(t, ref, cutOff)
		assert.Equal(t, keys[1], j.WindowStart)
		require.Len(t, v.all(), 1)
		var want []string
		for i := 1; i < n; i++ {
			want = append(want, strings.TrimSpace(filler(i)))
		}
		assert.Equal(t, want, v.all()[0].Texts, "each message is sent whole")
	})

	t.Run("a context whose newest message alone exceeds the limits, reported incomplete with reason message_over_limit, sending nothing and leaving no judgment", func(t *testing.T) {
		e, th, keys, _ := setup(t)
		newest := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, cutOff)}, keys[n-1])

		v, ref, out := judgeWith(t, e, newest-1)

		assert.Empty(t, v.all(), "nothing is sent")
		assert.Empty(t, th.judgments(t, ref), "no judgment is left")
		assert.Contains(t, out, "incomplete reason=message_over_limit n=1")
		assert.Equal(t, judgment.IncompleteReasonMessageOverLimit, e.lastRun(t, ref).Incomplete[0].Reason)
	})
}

func indexOf(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

// A cut-off's content leaves once per thread context per method with every
// question in that call, unless the questions exceed the method's question
// limit, when the fewest further calls over the same window carry the rest.
// spec: KNW-056.one-call-carries-every-question, KNW-056.over-question-limit-fewest-further-calls, KNW-053.group-once-per-cut-off
func TestJudgeMonthSendsEveryQuestionInTheFewestCalls(t *testing.T) {
	const n = 5
	questions := 1 + 3*n // ball-in-court for the contact, three flag questions per message
	cases := []struct {
		name  string
		limit int
		calls int
	}{
		{"questions equal to the limit, in one call", questions, 1},
		{"one question over the limit, in two calls over the same window", questions - 1, 2},
		{"questions needing three calls", (questions + 2) / 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, "")
			th := e.chat(t, n)
			v := newVendor(t, false)
			m := e.method(jevKey, v, decision.Limits{Tokens: 1 << 24, Questions: tc.limit})

			e.judgeMonth(t, methods{jevKey: m}, jevKey)

			calls := v.all()
			require.Len(t, calls, tc.calls)
			asked := 0
			for _, c := range calls {
				assert.LessOrEqual(t, len(c.Asks), tc.limit)
				assert.Equal(t, calls[0].Texts, c.Texts, "every call reads the same window")
				asked += len(c.Asks)
			}
			assert.Equal(t, questions, asked, "every question is asked once")
			j := th.judgmentAt(t, m.Ref(), jan(4, 0))
			assert.Len(t, j.Flags, 3*n, "the judgment holds the answers of every call")
			assert.Len(t, j.Outcomes, 1)
		})
	}
}
