//go:build integration_testdb

package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowLimit returns the smallest token limit under which a method of the
// definition reads the thread at the cut-off as one window starting at the
// message keys[from], the messages from keys[newFrom] on new, re-checking the
// commitments: the limit a night's last chunk of exactly that window needs.
func (e *flagsEnv) windowLimit(t *testing.T, def decision.Definition, th *thr, cutOff time.Time, from, newFrom int, commitments ...decision.Commitment) int {
	t.Helper()
	c := th.context(t, cutOff)
	after := c.Entries[newFrom-1].SentAt
	return e.tokensFor(t, def, decision.Request{Context: c, NewAfter: &after, Commitments: commitments}, c.Entries[from].Key)
}

// flagged returns the texts of the messages a call asks flag questions of, in
// the order the window holds them.
func (c call) flagged() []string {
	var out []string
	for _, a := range c.asks("flag") {
		if !slices.Contains(out, a.Text) {
			out = append(out, a.Text)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(c.Texts, a) - slices.Index(c.Texts, b) })
	return out
}

// night stores a thread whose first night (Jan 3) holds one message and whose
// second night (Jan 4, judged at the Jan 5 cut-off) holds n messages, every
// message the filler of its position.
func (e *flagsEnv) night(t *testing.T, n int) (*thr, []string, []string) {
	t.Helper()
	texts := make([]string, n+1)
	for i := range texts {
		texts[i] = filler(i)
	}
	return e.nightOf(t, texts...)
}

// nightOf stores a thread whose first night (Jan 3) holds the first text and
// whose second night (Jan 4, judged at the Jan 5 cut-off) holds the rest, and
// returns its message keys and texts in order.
func (e *flagsEnv) nightOf(t *testing.T, texts ...string) (*thr, []string, []string) {
	t.Helper()
	th := e.newThread(t)
	th.say(t, jan(3, 9), false, texts[0])
	for i, text := range texts[1:] {
		th.say(t, jan(4, 9+i), i%2 == 1, text)
	}
	trimmed := make([]string, len(texts))
	for i, text := range texts {
		trimmed[i] = strings.TrimSpace(text)
	}
	return th, th.keys(t), trimmed
}

// chunkCalls returns the calls of the thread's second night, one list per
// chunk: consecutive calls over the same window, since a later chunk's window
// ends at a later message.
func chunkCalls(v *vendor) [][]call {
	var out [][]call
	for _, c := range v.all()[1:] {
		if n := len(out); n > 0 && slices.Equal(out[n-1][0].Texts, c.Texts) {
			out[n-1] = append(out[n-1], c)
			continue
		}
		out = append(out, []call{c})
	}
	return out
}

// asked returns the questions of the kind a chunk's calls ask.
func asked(calls []call, kind string) []ask {
	var out []ask
	for _, c := range calls {
		out = append(out, c.asks(kind)...)
	}
	return out
}

// A night whose new messages do not fit one window of a method is judged in
// the fewest chunks that fit, in order from earliest to latest; each chunk's
// window is its new messages preceded by as many older messages as fit; the
// new messages are spread evenly over the chunks; each new message is flagged
// once, in its chunk.
// spec: KNW-068.fewest-chunks-earliest-first, KNW-068.window-is-chunk-and-older-context, KNW-068.spread-evenly, KNW-068.flagged-once-in-its-chunk
func TestJudgeMonthJudgesAnOverflowingNightInChunks(t *testing.T) {
	second := jan(5, 0)

	t.Run("a night that fits one window, judged over one window as before", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, texts := e.night(t, 5)
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()
		require.Len(t, calls, 2, "one call per night")
		assert.Equal(t, texts, calls[1].Texts)
		assert.Equal(t, texts[1:], calls[1].flagged())
		j := th.judgmentAt(t, m.Ref(), second)
		assert.Equal(t, keys[0], j.WindowStart)
		assert.Equal(t, keys[1:], j.NewMessages)
	})

	t.Run("a night one message over a window, judged in two chunks", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, texts := e.night(t, 5)
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 2)
		v := newVendor(t)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()[1:]
		require.Len(t, calls, 2, "two chunks, earliest first")
		assert.Equal(t, texts[1:3], calls[0].flagged())
		assert.Empty(t, calls[0].asks("ball_in_court"))
		assert.Equal(t, texts[3:], calls[1].flagged())
		assert.Len(t, calls[1].asks("ball_in_court"), 1)
		j := th.judgmentAt(t, m.Ref(), second)
		assert.Equal(t, keys[0], j.WindowStart, "the first chunk reads the older message as context")
		assert.Equal(t, keys[1:], j.NewMessages)
		assert.Len(t, j.Flags, 3*5, "each new message flagged once per kind")
		assert.Contains(t, out, "unjudged=0")
	})

	t.Run("a night needing three chunks, whose chunks' counts of new messages differ by at most one", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, texts := e.night(t, 7)
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 5, 5)
		v := newVendor(t)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()[1:]
		require.Len(t, calls, 3)
		var counts []int
		var all []string
		for _, c := range calls {
			counts = append(counts, len(c.flagged()))
			all = append(all, c.flagged()...)
		}
		assert.LessOrEqual(t, slices.Max(counts)-slices.Min(counts), 1, "counts %v", counts)
		assert.Equal(t, texts[1:], all, "each new message flagged once, the chunks in order")
		assert.Equal(t, keys[1:], th.judgmentAt(t, m.Ref(), second).NewMessages)
	})

	t.Run("a later chunk, whose window holds the earlier chunk's messages as context as far as they fit", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, _, texts := e.night(t, 5)
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3)
		v := newVendor(t)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()[1:]
		require.Len(t, calls, 2)
		assert.Equal(t, texts[1:3], calls[0].flagged())
		assert.Equal(t, texts[2:], calls[1].Texts, "the second chunk reads the first chunk's last message, the most that fits")
		assert.Equal(t, texts[3:], calls[1].flagged())
	})
}

// Each chunk of a night re-checks the method's earlier commitments whose
// messages lie inside its window and that it has not cleared, those an
// earlier chunk of the same night flagged included, and the night's judgment
// holds one clearing decision per commitment re-checked, carrying the latest
// chunk's answer.
// spec: KNW-069.chunk-rechecks-inside-its-window, KNW-069.same-night-commitment-rechecked, KNW-069.cleared-not-asked-again, KNW-069.one-clearing-latest-answer
func TestJudgeMonthRechecksCommitmentsInEachChunk(t *testing.T) {
	second := jan(5, 0)
	// A commitment, flagged by vendorFlagging, and a commitment's keeping.
	const said, done = promise, kept

	t.Run("a commitment flagged in a night's first chunk and re-checked in its second", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, texts := e.nightOf(t, filler(0), filler(1), said, filler(3), done, filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3)
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		chunks := chunkCalls(v)
		require.Len(t, chunks, 2)
		assert.Equal(t, texts[1:3], chunks[0][0].flagged(), "the first chunk flags the commitment")
		assert.Equal(t, texts[2:], chunks[1][0].Texts, "the second chunk's window holds it")
		assert.Empty(t, asked(chunks[0], "clearing"))
		clearings := asked(chunks[1], "clearing")
		require.Len(t, clearings, 1, "the second chunk re-checks it")
		assert.Equal(t, said, clearings[0].Text)
		j := th.judgmentAt(t, m.Ref(), second)
		assert.InDelta(t, 0.9, flagsOf(j)[keys[2]+"/commitment"], 1e-9)
		require.Len(t, j.Clearings, 1)
		assert.Equal(t, keys[2], j.Clearings[0].CommitmentMessageKey)
		assert.Equal(t, j.ID, j.Clearings[0].CommitmentJudgment, "the commitment is the night's own")
		assert.True(t, j.Clearings[0].Done)
	})

	t.Run("an earlier night's commitment judged done in the first chunk, which the second does not ask again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, said, done, filler(2), filler(3), filler(4), filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 0, 3, decision.Commitment{Judgment: uuid.New(), MessageKey: keys[0]})
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		chunks := chunkCalls(v)
		require.Len(t, chunks, 2)
		require.Contains(t, chunks[1][0].Texts, said, "the second chunk's window holds the commitment")
		assert.Len(t, asked(chunks[0], "clearing"), 1)
		assert.Empty(t, asked(chunks[1], "clearing"), "not asked again once cleared")
		first := th.judgmentAt(t, m.Ref(), jan(4, 0))
		j := th.judgmentAt(t, m.Ref(), second)
		require.Len(t, j.Clearings, 1)
		assert.Equal(t, first.ID, j.Clearings[0].CommitmentJudgment)
		assert.True(t, j.Clearings[0].Done)
	})

	t.Run("an earlier night's commitment judged not done in the first chunk and done in the second, recorded once as done", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, said, filler(1), filler(2), filler(3), done, filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 0, 3, decision.Commitment{Judgment: uuid.New(), MessageKey: keys[0]})
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		chunks := chunkCalls(v)
		require.Len(t, chunks, 2)
		assert.Len(t, asked(chunks[0], "clearing"), 1)
		assert.Len(t, asked(chunks[1], "clearing"), 1)
		j := th.judgmentAt(t, m.Ref(), second)
		require.Len(t, j.Clearings, 1, "one clearing decision per commitment")
		assert.True(t, j.Clearings[0].Done, "the latest chunk's answer")
		assert.InDelta(t, 0.9, j.Clearings[0].Probability, 1e-9)
	})

	t.Run("a commitment whose message lies before a chunk's window, which that chunk neither asks nor clears", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, _, _ := e.nightOf(t, said, filler(1), filler(2), filler(3), done, filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3)
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		chunks := chunkCalls(v)
		require.Len(t, chunks, 2)
		require.NotContains(t, chunks[1][0].Texts, said, "the commitment lies before the second chunk's window")
		assert.Len(t, asked(chunks[0], "clearing"), 1)
		assert.Empty(t, asked(chunks[1], "clearing"))
		j := th.judgmentAt(t, m.Ref(), second)
		require.Len(t, j.Clearings, 1)
		assert.False(t, j.Clearings[0].Done, "the first chunk's answer stands: the second chunk, which reads the keeping, clears nothing")
	})
}

// Ball-in-court is asked once per chunked night, in its last chunk, about the
// thread's state at the cut-off, and the reply-obligation rule compares an
// obligation's evidence with that chunk's window.
// spec: KNW-070.asked-in-last-chunk, KNW-070.evidence-against-last-chunk-window
func TestJudgeMonthAsksBallInCourtInTheLastChunk(t *testing.T) {
	t.Run("a two-chunk night whose first chunk carries no ball-in-court question", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, _, _ := e.night(t, 5)
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, jan(5, 0), 2, 3)
		v := newVendor(t)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		chunks := chunkCalls(v)
		require.Len(t, chunks, 2)
		assert.Empty(t, asked(chunks[0], "ball_in_court"))
		assert.Len(t, asked(chunks[1], "ball_in_court"), 1)
		assert.Len(t, th.judgmentAt(t, m.Ref(), jan(5, 0)).Outcomes, 1, "one ball-in-court outcome for the night")
	})

	// The thread holds m0 to m6: m1 is the newest message at the cut-off that
	// opens a reply obligation; the next run judges a later night of m2 to m6
	// in two chunks, the first reading from m0 and the last from m1 or m3, and
	// says nobody owes a reply.
	cases := []struct {
		name   string
		from   int
		claims string
	}{
		{"evidence inside the last chunk's window, where the apply rule acts", 1, "1"},
		{"evidence inside the first chunk's window but before the last chunk's, where the obligation stays as it was", 3, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, jevKey)
			th := e.newThread(t)
			th.sayFiller(t, jan(2, 9), false, 0)
			th.sayFiller(t, jan(3, 9), false, 1)
			v := newVendor(t)
			v.bic = func(string) string { return "i_owe_reply" }
			out := e.judgeMonth(t, methods{jevKey: e.method(jevKey, v, roomy)}, jevKey)
			require.Contains(t, out, "live_effects obligations_opened=1")
			for i := 2; i <= 6; i++ {
				th.sayFiller(t, jan(10, 7+i), i%2 == 1, i)
			}
			keys := th.keys(t)
			limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, jan(11, 0), tc.from, 4)
			v.bic = func(string) string { return "nobody_owes" }
			v.reset()
			chunked := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

			out = e.judgeMonth(t, methods{jevKey: chunked}, jevKey)

			night := th.judgmentAt(t, chunked.Ref(), jan(11, 0))
			require.Equal(t, keys[0], night.WindowStart, "the first chunk's window holds the evidence")
			calls := v.all()
			require.Len(t, calls, 2, "the night is judged in two chunks; the earlier nights are reused")
			require.Equal(t, strings.TrimSpace(filler(tc.from)), calls[1].Texts[0])
			assert.Contains(t, out, "live_effects obligations_opened=0 flags_raised=0 claims_raised="+tc.claims)
			assert.Len(t, openObligations(t, th), 1, "a claim removes nothing")
		})
	}
}

// huge is a message that alone exceeds the limits a test sets.
var huge = "zanzibar " + strings.Repeat("and the weather ", 400)

// A run of a new method version leaves no new message unjudged for lying
// before its window: a new message that alone exceeds the method's limits is
// recorded only as unjudged message_over_limit, with no flag decision, and the
// night's other new messages are judged.
// spec: KNW-071.none-before-window, KNW-071.oversized-message-unjudged, KNW-071.oversized-newest-incomplete
func TestJudgeMonthLeavesNoNewMessageUnjudgedBeforeItsWindow(t *testing.T) {
	t.Run("a month whose bursty nights overflow a window by hundreds of new messages, leaving none unjudged", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		bursts := map[time.Time]int{jan(5, 0): 150, jan(7, 0): 120}
		for cutOff, n := range bursts {
			day := cutOff.AddDate(0, 0, -1)
			for i := range n {
				th.sayFiller(t, day.Add(time.Duration(i+1)*time.Minute), i%2 == 1, i)
			}
		}
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, jan(5, 0), 145, 145)
		m := e.method(jevKey, newVendor(t), decision.Limits{Tokens: limit, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Contains(t, out, "unjudged=0")
		assert.Empty(t, e.lastRun(t, m.Ref()).Unjudged)
		keys := th.keys(t)
		first := th.judgmentAt(t, m.Ref(), jan(5, 0))
		assert.Equal(t, keys[1:151], first.NewMessages)
		assert.Len(t, first.Flags, 3*150)
		assert.Equal(t, keys[151:], th.judgmentAt(t, m.Ref(), jan(7, 0)).NewMessages)
	})

	t.Run("an oversized message in the middle of a night, unjudged with no flag decision while its neighbors are judged", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, filler(0), filler(1), filler(2), huge, filler(4), filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, jan(5, 0), 4, 4)
		m := e.method(jevKey, newVendor(t), decision.Limits{Tokens: limit, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Contains(t, out, "unjudged=1")
		assert.Contains(t, out, "unjudged reason=message_over_limit n=1")
		j := th.judgmentAt(t, m.Ref(), jan(5, 0))
		assert.Equal(t, []string{keys[1], keys[2], keys[4], keys[5]}, j.NewMessages)
		assert.Len(t, j.Flags, 3*4)
		assert.NotContains(t, flagsOf(j), keys[3]+"/commitment", "no flag decision for the oversized message")
		unjudged := e.lastRun(t, m.Ref()).Unjudged
		require.Len(t, unjudged, 1)
		assert.Equal(t, keys[3], unjudged[0].MessageKey)
		assert.Equal(t, judgment.UnjudgedReasonMessageOverLimit, unjudged[0].Reason)
	})

	t.Run("an oversized newest message, whose cut-off is reported incomplete with message_over_limit as stage 1 reports it", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, filler(0), filler(1), filler(2), huge)
		limit := 3 * e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, keys[0])
		v := newVendor(t)
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Len(t, v.all(), 1, "nothing of the night is sent")
		assert.Contains(t, out, "incomplete reason=message_over_limit n=1")
		assert.Contains(t, out, "unjudged reason=message_over_limit n=3")
		views := th.judgments(t, m.Ref())
		require.Len(t, views, 1, "the night records no judgment")
		assert.True(t, jan(4, 0).Equal(views[0].CutOff))
	})
}

// decisionsOf returns a judgment's flag decisions as message key and kind, in
// the order the store reads them.
func decisionsOf(v judgment.JudgmentView) []string {
	out := make([]string, len(v.Flags))
	for i, f := range v.Flags {
		out[i] = f.MessageKey + "/" + string(f.Kind)
	}
	return out
}

// everyKind returns each message key with each flag kind, in thread order.
func everyKind(keys ...string) []string {
	var out []string
	for _, key := range keys {
		for _, kind := range judgment.FlagKinds {
			out = append(out, key+"/"+string(kind))
		}
	}
	return out
}

// A chunked night reads back from the store as one judgment per thread,
// method and cut-off, on the example every method records for that context:
// its window start the first chunk's, its new messages the night's new
// messages its chunks answered in thread order, one flag decision per new
// message and kind and none for an unjudged message, and at most one clearing
// decision per commitment.
// spec: KNW-072.one-judgment-per-night, KNW-072.window-start-first-chunk, KNW-072.new-messages-answered-in-order, KNW-072.unjudged-not-new
func TestStoreReadsAChunkedNightAsOneJudgment(t *testing.T) {
	second := jan(5, 0)

	t.Run("a night one method chunks and another reads whole, both on one example", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.night(t, 5)
		v := newVendor(t)
		chunking := e.method(jevKey, v, decision.Limits{Tokens: e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3), Questions: 1 << 20})
		whole := e.method(clefKey, newVendor(t), roomy)

		e.judgeMonth(t, methods{jevKey: chunking}, jevKey)
		e.judgeMonth(t, methods{clefKey: whole}, clefKey)

		a, b := th.judgmentAt(t, chunking.Ref(), second), th.judgmentAt(t, whole.Ref(), second)
		require.Len(t, chunkCalls(v), 2)
		require.Equal(t, strings.TrimSpace(filler(2)), chunkCalls(v)[1][0].Texts[0], "the last chunk's window starts later than the first's")
		assert.Equal(t, keys[0], a.WindowStart, "the first chunk's window start")
		assert.Equal(t, keys[1:], a.NewMessages)
		assert.Equal(t, everyKind(keys[1:]...), decisionsOf(a))
		assert.Equal(t, a.NewMessages, b.NewMessages)
		assert.Equal(t, b.InputHash, a.InputHash)
		assert.Equal(t, e.exampleAt(t, whole.Ref(), second), e.exampleAt(t, chunking.Ref(), second), "one example for both")
	})

	t.Run("the same night read by thread and by run", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, filler(0), filler(1), promise, filler(3), kept, filler(5))
		m := e.method(jevKey, vendorFlagging(t, 0.9), decision.Limits{Tokens: e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3), Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		byThread := th.judgmentAt(t, m.Ref(), second)
		run := e.lastRun(t, m.Ref()).ID
		inRun, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Run: &run})
		require.NoError(t, err)
		var byRun []judgment.JudgmentView
		for _, v := range inRun {
			if v.CutOff.Equal(second) {
				byRun = append(byRun, v)
			}
		}
		require.Len(t, byRun, 1, "one judgment of the night in the run")
		assert.Equal(t, byThread, byRun[0])
		assert.Equal(t, keys[1:], byRun[0].NewMessages)
		require.Len(t, byRun[0].Clearings, 1, "one clearing decision for the commitment")
		assert.Equal(t, keys[2], byRun[0].Clearings[0].CommitmentMessageKey)
	})

	t.Run("a chunked night holding an oversized message, which appears among its unjudged messages and not among its new messages", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, filler(0), filler(1), filler(2), huge, filler(4), filler(5))
		m := e.method(jevKey, newVendor(t), decision.Limits{Tokens: e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 4, 4), Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		j := th.judgmentAt(t, m.Ref(), second)
		judged := []string{keys[1], keys[2], keys[4], keys[5]}
		assert.Equal(t, judged, j.NewMessages)
		assert.Equal(t, everyKind(judged...), decisionsOf(j))
		run := e.lastRun(t, m.Ref())
		require.Len(t, run.Unjudged, 1)
		assert.Equal(t, keys[3], run.Unjudged[0].MessageKey)
		assert.Equal(t, map[judgment.UnjudgedReason]int{judgment.UnjudgedReasonMessageOverLimit: 1}, run.UnjudgedCounts)
	})
}

// judgmentsAt returns the thread's judgments of the method version at the
// cut-off, oldest first.
func (th *threadRef) judgmentsAt(t *testing.T, m judgment.MethodRef, cutOff time.Time) []judgment.JudgmentView {
	t.Helper()
	var out []judgment.JudgmentView
	for _, j := range th.judgments(t, m) {
		if j.CutOff.Equal(cutOff) {
			out = append(out, j)
		}
	}
	return out
}

// windowsOf returns the windows a night's chunks read, as their texts.
func windowsOf(chunks [][]call) [][]string {
	out := make([][]string, len(chunks))
	for i, c := range chunks {
		out[i] = c[0].Texts
	}
	return out
}

// A rerun reuses a chunked night's judgment only when every chunk read the
// same window and new messages and re-checked the same earlier judgments.
// spec: KNW-073.reuse-when-every-chunk-read-same, KNW-073.different-recheck-judged-again, KNW-073.different-chunks-judged-again
func TestJudgeMonthReusesAChunkedNightOnlyWhenEveryChunkReadTheSame(t *testing.T) {
	second := jan(5, 0)

	t.Run("an unchanged rerun, which reuses", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, _, _ := e.night(t, 5)
		v := newVendor(t)
		set := methods{jevKey: e.method(jevKey, v, decision.Limits{Tokens: e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 2, 3), Questions: 1 << 20})}
		e.judgeMonth(t, set, jevKey)
		require.Len(t, chunkCalls(v), 2)
		sent := len(v.all())

		out := e.judgeMonth(t, set, jevKey)

		assert.Contains(t, out, "judged=0 reused=2")
		assert.Len(t, v.all(), sent, "nothing is sent")
	})

	t.Run("a rerun whose earlier night now judges a commitment differently, so a chunk that re-checked it is judged again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		th.sayFiller(t, jan(3, 10), true, 1)
		th.say(t, jan(3, 11), true, promise)
		for i := 3; i <= 6; i++ {
			th.sayFiller(t, jan(4, 6+i), i%2 == 0, i)
		}
		keys := th.keys(t)
		def := fakeDefinition(jevKey, roomy)
		// The first run reads the earlier night whole; the rerun's limit is one
		// below it, so the earlier night is judged anew over a narrower window,
		// while the later night splits into the same chunks over the same
		// windows under both.
		wide := e.windowLimit(t, def, th, second, 3, 3) - 1
		narrow := e.tokensFor(t, def, decision.Request{Context: th.context(t, jan(4, 0))}, keys[0]) - 1
		v := vendorFlagging(t, 0.9)
		e.judgeMonth(t, methods{jevKey: e.method(jevKey, v, decision.Limits{Tokens: wide, Questions: 1 << 20})}, jevKey)
		before := windowsOf(chunkCalls(v))
		v.reset()
		m := e.method(jevKey, v, decision.Limits{Tokens: narrow, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		require.Len(t, before, 2)
		after := windowsOf(chunkCalls(v))
		require.Equal(t, before, after, "the later night's chunks read the same windows")
		assert.Contains(t, out, "judged=2 reused=0")
		earlier := th.judgmentsAt(t, m.Ref(), jan(4, 0))
		require.Len(t, earlier, 2)
		nights := th.judgmentsAt(t, m.Ref(), second)
		require.Len(t, nights, 2, "the later night is judged again")
		require.Len(t, nights[1].Clearings, 1)
		assert.Equal(t, earlier[1].ID, nights[1].Clearings[0].CommitmentJudgment, "it re-checked the earlier night's new judgment")
	})

	t.Run("a rerun whose night now splits into a different number of chunks, which is judged again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th, _, _ := e.night(t, 6)
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, second, 4, 4)
		v := newVendor(t)
		e.judgeMonth(t, methods{jevKey: e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})}, jevKey)
		require.Len(t, chunkCalls(v), 2)
		v.reset()
		m := e.method(jevKey, v, decision.Limits{Tokens: limit - 1, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Len(t, v.all(), 3, "the night is judged again in three chunks; the earlier night is reused")
		assert.Contains(t, out, "judged=1 reused=1")
		nights := th.judgmentsAt(t, m.Ref(), second)
		require.Len(t, nights, 2)
		assert.Equal(t, nights[0].WindowStart, nights[1].WindowStart, "the night's first window start is the same")
		assert.Equal(t, nights[0].NewMessages, nights[1].NewMessages, "and so are its new messages")
	})
}
