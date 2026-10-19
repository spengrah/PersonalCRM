//go:build integration_testdb

package main

import (
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run judges each thread's cut-offs in order from the run's start and
// reuses a recorded judgment only when what it read is the same: its window,
// its new messages and the earlier judgments whose commitments it re-checked.
// spec: KNW-062.cut-offs-in-order-from-start, KNW-062.reuse-only-when-read-same, KNW-062.different-read-judged-anew, KNW-053.reused-not-sent
func TestJudgeMonthReusesAJudgmentOnlyWhenItReadTheSame(t *testing.T) {
	t.Run("a rerun with everything the same, which reuses every judgment and sends nothing", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, promise)
		th.say(t, jan(4, 9), true, kept)
		th.say(t, jan(5, 9), false, "thanks")
		v := vendorFlagging(t, false, 0.9)
		m := e.method(jevKey, v, roomy)
		set := methods{jevKey: m}
		e.judgeMonth(t, set, jevKey)
		sent := len(v.all())
		before := th.judgments(t, m.Ref())

		out := e.judgeMonth(t, set, jevKey)

		assert.Contains(t, out, "judged=0 reused=3")
		assert.Len(t, v.all(), sent, "nothing is sent")
		assert.Len(t, th.judgments(t, m.Ref()), len(before), "no judgment is added")
	})

	t.Run("a rerun whose earlier cut-off now judges a commitment differently, so a later cut-off that re-checked it is judged again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 8), false, 0)
		th.say(t, jan(3, 9), true, promise)
		th.say(t, jan(4, 9), false, "thanks")
		th.say(t, jan(5, 9), false, "thanks again")
		keys := th.keys(t)
		v := vendorFlagging(t, false, 0.9)
		first := e.method(jevKey, v, roomy)
		e.judgeMonth(t, methods{jevKey: first}, jevKey)
		recheckedBefore := th.judgmentAt(t, first.Ref(), jan(5, 0))
		require.Len(t, recheckedBefore.Clearings, 1)
		// The first cut-off no longer fits whole, so its judgment reads a
		// window that starts after the first message and is another record;
		// the second cut-off's window and new messages are as they were.
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, keys[0]) - 1
		narrower := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})
		require.Equal(t, first.Ref(), narrower.Ref())
		v.reset()

		out := e.judgeMonth(t, methods{jevKey: narrower}, jevKey)

		assert.Contains(t, out, "judged=3 reused=0")
		var second []judgment.JudgmentView
		for _, j := range th.judgments(t, first.Ref()) {
			if j.CutOff.Equal(jan(5, 0)) {
				second = append(second, j)
			}
		}
		require.Len(t, second, 2, "the second cut-off is judged again")
		before, after := second[0], second[1]
		assert.Equal(t, before.WindowStart, after.WindowStart, "over the same window")
		assert.Equal(t, before.NewMessages, after.NewMessages, "and the same new messages")
		require.Len(t, after.Clearings, 1)
		assert.NotEqual(t, before.Clearings[0].CommitmentJudgment, after.Clearings[0].CommitmentJudgment, "it re-checked the commitment of the first cut-off's new judgment")
	})

	t.Run("a rerun in which the thread's first cut-off, which failed, now succeeds, so the next cut-off is judged again over fewer new messages", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		th.sayFiller(t, jan(4, 9), true, 1)
		keys := th.keys(t)
		v := newVendor(t, false)
		v.fail = func(c call) bool { return len(c.Texts) == 1 }
		m := e.method(jevKey, v, roomy)
		set := methods{jevKey: m}
		e.judgeMonth(t, set, jevKey)
		require.Equal(t, keys, th.judgmentAt(t, m.Ref(), jan(5, 0)).NewMessages)
		v.fail = nil
		v.reset()

		out := e.judgeMonth(t, set, jevKey)

		assert.Contains(t, out, "judged=2 reused=0")
		var news [][]string
		for _, j := range th.judgments(t, m.Ref()) {
			if j.CutOff.Equal(jan(5, 0)) {
				news = append(news, j.NewMessages)
			}
		}
		assert.ElementsMatch(t, [][]string{keys, keys[1:]}, news, "the cut-off is judged again, its new messages narrowed to those after the previous scheduled cut-off")
	})
}
