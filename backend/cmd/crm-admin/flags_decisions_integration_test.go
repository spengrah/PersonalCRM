//go:build integration_testdb

package main

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const clefKey = "clef-masked"

// say stores a message of the thread at the time, from the contact, with the
// text of the i-th filler message.
func (th *thr) sayFiller(t *testing.T, at time.Time, fromUser bool, i int) {
	t.Helper()
	th.say(t, at, fromUser, filler(i))
}

// Each judgment holds exactly one flag decision per flag kind for each new
// message in its window, each with its yes answer's probability, and none for
// a message new at an earlier cut-off.
// spec: KNW-057.decision-per-kind-per-new-message, KNW-057.none-for-earlier-new-message, KNW-057.first-judged-cut-off-all-new, KNW-057.failed-first-cut-off-carries-forward, KNW-057.later-cut-off-new-since-previous-scheduled, KNW-057.meeting-notes-flagged-no-ball-in-court, KNW-065.one-example-per-context
func TestJudgeMonthRecordsAFlagDecisionPerKindForEachNewMessage(t *testing.T) {
	kinds := len(judgment.FlagKinds)

	t.Run("a thread's first judged cut-off in the method version's history, where every message in the window is new and no older message is", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 6)
		keys := th.keys(t)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, keys[2])
		m := e.method(jevKey, newVendor(t), decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		j := th.judgmentAt(t, m.Ref(), jan(4, 0))
		assert.Equal(t, keys[2], j.WindowStart)
		assert.Equal(t, keys[2:], j.NewMessages)
		assert.Len(t, j.Flags, kinds*len(keys[2:]))
		for _, f := range j.Flags {
			assert.Contains(t, keys[2:], f.MessageKey, "no flag decision for a message before the window")
		}
	})

	t.Run("a thread whose first cut-off failed and whose next cut-off is judged, where every message in that judged cut-off's window is new", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		th.sayFiller(t, jan(4, 9), true, 1)
		keys := th.keys(t)
		v := newVendor(t)
		v.fail = func(c call) bool { return len(c.Texts) == 1 }
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Len(t, th.judgments(t, m.Ref()), 1, "the failed cut-off records no judgment")
		j := th.judgmentAt(t, m.Ref(), jan(5, 0))
		assert.Equal(t, keys, j.NewMessages, "the message the failed cut-off left unjudged is new here")
		assert.Len(t, j.Flags, kinds*len(keys))
	})

	t.Run("a later cut-off, where only messages after the previous scheduled cut-off are new", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		th.sayFiller(t, jan(4, 9), true, 1)
		keys := th.keys(t)
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		first, second := th.judgmentAt(t, m.Ref(), jan(4, 0)), th.judgmentAt(t, m.Ref(), jan(5, 0))
		assert.Equal(t, keys[:1], first.NewMessages)
		assert.Equal(t, keys[1:], second.NewMessages)
		assert.Equal(t, keys[0], second.WindowStart, "the window holds the earlier message as context")
		assert.Len(t, second.Flags, kinds)
		for _, f := range second.Flags {
			assert.Equal(t, keys[1], f.MessageKey, "no flag decision for a message new at an earlier cut-off")
		}
		calls := v.all()
		require.Len(t, calls, 2)
		assert.Len(t, calls[1].Texts, 2)
		assert.Len(t, calls[1].asks("flag"), kinds, "the second call asks about the new message only")
	})

	t.Run("a message sent exactly at the previous scheduled cut-off, which is not new, and one sent exactly at the current cut-off, which is", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 12), false, 0)
		th.sayFiller(t, jan(4, 0), true, 1)
		th.sayFiller(t, jan(5, 0), false, 2)
		keys := th.keys(t)
		m := e.method(jevKey, newVendor(t), roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Equal(t, keys[:2], th.judgmentAt(t, m.Ref(), jan(4, 0)).NewMessages, "a message sent at a cut-off is new at it")
		assert.Equal(t, keys[2:], th.judgmentAt(t, m.Ref(), jan(5, 0)).NewMessages, "and not at the next")
	})

	t.Run("a thread the same version judged in the previous month's run, whose first cut-off in the next month's run treats only messages after the previous scheduled cut-off as new", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(30, 9), false, 0)
		th.sayFiller(t, time.Date(2026, time.February, 1, 12, 0, 0, 0, time.Local), true, 1)
		th.sayFiller(t, time.Date(2026, time.February, 2, 9, 0, 0, 0, time.Local), false, 2)
		keys := th.keys(t)
		m := e.method(jevKey, newVendor(t), roomy)
		set := methods{jevKey: m}

		e.judgeMonth(t, set, jevKey)
		e.judgeMonthOf(t, "2026-02", set, jevKey)

		first := th.judgmentAt(t, m.Ref(), time.Date(2026, time.February, 2, 0, 0, 0, 0, time.Local))
		assert.Equal(t, keys[1:2], first.NewMessages)
		assert.Equal(t, keys[0], first.WindowStart)
	})

	t.Run("a meeting, whose notes are flagged and which is asked no ball-in-court question", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		meeting := e.newMeeting(t)
		meeting.note(t, jan(3, 9), "we agreed the garden plan")
		meeting.note(t, jan(3, 10), "the budget goes to the committee")
		keys := meeting.keys(t)
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		j := meeting.judgmentAt(t, m.Ref(), jan(4, 0))
		assert.Equal(t, keys, j.NewMessages)
		assert.Len(t, j.Flags, kinds*len(keys))
		assert.Empty(t, j.Outcomes)
		require.Len(t, v.all(), 1)
		assert.Empty(t, v.all()[0].asks("ball_in_court"))
	})

	t.Run("a yes answer below the display threshold and one at or above it, each recorded with its probability", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), false, "note about the garden")
		th.say(t, jan(3, 10), true, "note about the weather")
		keys := th.keys(t)
		threshold, err := e.store.DisplayThreshold(e.ctx, jevKey, judgment.FlagKindCommitment)
		require.NoError(t, err)
		v := newVendor(t)
		v.flag = func(a ask) float64 {
			switch {
			case a.Flag != judgment.FlagKindCommitment:
				return 0.01
			case strings.Contains(a.Text, "garden"):
				return threshold - 0.1
			default:
				return threshold
			}
		}
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		probabilities := flagsOf(th.judgmentAt(t, m.Ref(), jan(4, 0)))
		assert.InDelta(t, threshold-0.1, probabilities[keys[0]+"/commitment"], 1e-9, "below the display threshold")
		assert.InDelta(t, threshold, probabilities[keys[1]+"/commitment"], 1e-9, "at the display threshold")
		assert.InDelta(t, 0.01, probabilities[keys[0]+"/claim"], 1e-9)
	})

	t.Run("the same message carries the same key at every cut-off and for every method", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), false, 0)
		th.sayFiller(t, jan(4, 9), true, 1)
		keys := th.keys(t)
		jev, clef := e.method(jevKey, newVendor(t), roomy), e.method(clefKey, newVendor(t), roomy)

		e.judgeMonth(t, methods{jevKey: jev}, jevKey)
		e.judgeMonth(t, methods{clefKey: clef}, clefKey)

		for _, m := range []decision.Method{jev, clef} {
			assert.Equal(t, keys[:1], th.judgmentAt(t, m.Ref(), jan(4, 0)).NewMessages, m.Ref().Key)
			assert.Equal(t, keys[1:], th.judgmentAt(t, m.Ref(), jan(5, 0)).NewMessages, m.Ref().Key)
		}
		assert.Equal(t, keys[0], th.context(t, jan(4, 0)).Entries[0].Key)
		assert.Equal(t, keys[0], th.context(t, jan(5, 0)).Entries[0].Key)
	})
}

// A new message that no judgment answered is recorded as unjudged with its
// reason and counted as unjudged in the run's report, never as a flag decision
// answered no; rerunning its failed cut-off judges it.
// spec: KNW-058.recorded-unjudged-with-reason, KNW-058.counted-unjudged-never-no, KNW-058.rerun-judges-it, KNW-054.summary-counts-unjudged, KNW-052.failed-calls-retried, KNW-052.judged-or-incomplete
func TestJudgeMonthRecordsMessagesNoJudgmentAnsweredAsUnjudged(t *testing.T) {
	t.Run("a cut-off whose call failed after its retries, and a rerun of it that judges those messages", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 3)
		keys := th.keys(t)
		v := newVendor(t)
		v.fail = func(call) bool { return true }
		m := e.method(jevKey, v, roomy)
		set := methods{jevKey: m}

		out := e.judgeMonth(t, set, jevKey)

		assert.Len(t, v.all(), 3, "the call is asked three times in all")
		assert.Contains(t, out, "incomplete reason=call_failed n=1")
		assert.Contains(t, out, "unjudged=3")
		assert.Contains(t, out, "unjudged reason=call_failed n=3")
		assert.Empty(t, th.judgments(t, m.Ref()), "no flag decision answered no")
		failed := e.lastRun(t, m.Ref())
		assert.Equal(t, map[judgment.UnjudgedReason]int{judgment.UnjudgedReasonCallFailed: 3}, failed.UnjudgedCounts)
		var recorded []string
		for _, u := range failed.Unjudged {
			recorded = append(recorded, u.MessageKey)
			assert.Equal(t, th.venue, u.Thread)
			assert.True(t, jan(4, 0).Equal(u.CutOff))
		}
		assert.ElementsMatch(t, keys, recorded)

		v.fail = nil
		rerun := e.judgeMonth(t, set, jevKey)

		assert.Contains(t, rerun, "unjudged=0")
		j := th.judgmentAt(t, m.Ref(), jan(4, 0))
		assert.Len(t, j.Flags, 3*len(keys), "the judgment holds flag decisions for those messages")
	})

	t.Run("a cut-off reported message_over_limit", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 3)
		keys := th.keys(t)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, keys[2]) - 1
		m := e.method(jevKey, newVendor(t), decision.Limits{Tokens: limit, Questions: 1 << 20})

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Contains(t, out, "unjudged reason=message_over_limit n=3")
		assert.Empty(t, th.judgments(t, m.Ref()))
	})
}
