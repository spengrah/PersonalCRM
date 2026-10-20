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

// promise is a message that states a commitment, and kept one that shows it
// done.
const (
	promise = "i will send the photos tonight"
	kept    = "sent it just now"
)

// vendorFlagging is a vendor that gives a commitment flag to a message
// stating a promise at the probability, and clears a commitment when a later
// message shows it kept.
func vendorFlagging(t *testing.T, p float64) *vendor {
	t.Helper()
	v := newVendor(t)
	v.flag = func(a ask) float64 {
		if a.Flag == judgment.FlagKindCommitment && strings.Contains(a.Text, "i will") {
			return p
		}
		return 0.05
	}
	v.clear = func(a ask) float64 {
		for _, later := range a.After {
			if strings.Contains(later, "sent it") {
				return 0.9
			}
		}
		return 0.1
	}
	return v
}

// At each cut-off a method asks, for each of its own earlier commitment flag
// decisions above its clearing floor, whose message lies inside its window and
// which it has not cleared, whether the commitment is done as of the cut-off,
// and records each answer as a clearing decision.
// spec: KNW-059.rechecked-and-recorded, KNW-059.cleared-once-answered-done, KNW-059.outside-window-neither-cleared-nor-kept, KNW-067.inputs-are-window-and-own-earlier-judgments
func TestJudgeMonthRechecksEarlierCommitments(t *testing.T) {
	t.Run("one above the floor inside the window, which is asked, and one at the floor, which is not", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		floor, err := e.store.ClearingFloor(e.ctx, jevKey)
		require.NoError(t, err)
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, "i will send the photos tonight")
		th.say(t, jan(3, 10), true, "i will call you on the weekend")
		th.say(t, jan(4, 9), false, "thanks for the photos")
		keys := th.keys(t)
		v := newVendor(t)
		v.flag = func(a ask) float64 {
			switch {
			case a.Flag != judgment.FlagKindCommitment:
				return 0.05
			case strings.Contains(a.Text, "photos tonight"):
				return floor + 0.05
			case strings.Contains(a.Text, "call you"):
				return floor
			}
			return 0.05
		}
		v.clear = func(ask) float64 { return 0.9 }
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		first, second := th.judgmentAt(t, m.Ref(), jan(4, 0)), th.judgmentAt(t, m.Ref(), jan(5, 0))
		assert.Empty(t, first.Clearings, "nothing is earlier at the first judged cut-off")
		require.Len(t, second.Clearings, 1)
		c := second.Clearings[0]
		assert.Equal(t, keys[0], c.CommitmentMessageKey)
		assert.Equal(t, first.ID, c.CommitmentJudgment)
		assert.True(t, c.Done)
		assert.InDelta(t, 0.9, c.Probability, 1e-9)
		clearings := v.all()[1].asks("clearing")
		require.Len(t, clearings, 1)
		assert.Contains(t, clearings[0].Text, "photos tonight")
	})

	t.Run("one an earlier cut-off already cleared, which is not asked again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, promise)
		th.say(t, jan(4, 9), true, kept)
		th.say(t, jan(5, 9), false, "thanks")
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()
		require.Len(t, calls, 3)
		assert.Len(t, calls[1].asks("clearing"), 1, "asked at the cut-off after it was flagged")
		assert.Empty(t, calls[2].asks("clearing"), "not asked once cleared")
		assert.Empty(t, th.judgmentAt(t, m.Ref(), jan(6, 0)).Clearings)
	})

	t.Run("one answered not done, which is asked again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, promise)
		th.say(t, jan(4, 9), false, "thanks")
		th.say(t, jan(5, 9), false, "thanks again")
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, roomy)

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		calls := v.all()
		require.Len(t, calls, 3)
		assert.Len(t, calls[1].asks("clearing"), 1)
		assert.Len(t, calls[2].asks("clearing"), 1)
		assert.False(t, th.judgmentAt(t, m.Ref(), jan(5, 0)).Clearings[0].Done)
	})

	t.Run("one whose message lies before the window start, which is neither asked nor cleared nor kept", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.sayFiller(t, jan(3, 9), true, 0)
		th.sayFiller(t, jan(3, 10), false, 1)
		th.sayFiller(t, jan(4, 10), true, 2)
		th.sayFiller(t, jan(4, 11), false, 3)
		keys := th.keys(t)
		previous := jan(4, 0)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(5, 0)), NewAfter: &previous}, keys[2])
		v := newVendor(t)
		v.flag = func(a ask) float64 {
			if a.Flag == judgment.FlagKindCommitment && strings.Contains(a.Text, strings.TrimSpace(filler(0))) {
				return 0.9
			}
			return 0.05
		}
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: m}, jevKey)

		require.InDelta(t, 0.9, flagsOf(th.judgmentAt(t, m.Ref(), jan(4, 0)))[keys[0]+"/commitment"], 1e-9, "the commitment is flagged at the first cut-off")
		second := th.judgmentAt(t, m.Ref(), jan(5, 0))
		require.Equal(t, keys[2], second.WindowStart)
		assert.Empty(t, second.Clearings)
		assert.Empty(t, v.all()[1].asks("clearing"))
	})

	t.Run("another method's commitment flag on the same message, which is not asked", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, promise)
		th.say(t, jan(4, 9), true, kept)
		flagging, silent := vendorFlagging(t, 0.9), vendorFlagging(t, 0.05)
		clef, jev := e.method(clefKey, flagging, roomy), e.method(jevKey, silent, roomy)

		e.judgeMonth(t, methods{clefKey: clef}, clefKey)
		e.judgeMonth(t, methods{jevKey: jev}, jevKey)

		assert.Len(t, flagging.all()[1].asks("clearing"), 1)
		assert.Empty(t, silent.all()[1].asks("clearing"))
		assert.Empty(t, th.judgmentAt(t, jev.Ref(), jan(5, 0)).Clearings)
	})

	t.Run("a commitment flagged in the previous month's run of the same version, asked at the next month's cut-off whose window holds its message", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(30, 9), true, promise)
		th.say(t, time.Date(2026, time.February, 2, 9, 0, 0, 0, time.Local), true, kept)
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, roomy)
		set := methods{jevKey: m}

		e.judgeMonth(t, set, jevKey)
		january := th.judgmentAt(t, m.Ref(), jan(31, 0))
		e.judgeMonthOf(t, "2026-02", set, jevKey)

		february := th.judgmentAt(t, m.Ref(), time.Date(2026, time.February, 3, 0, 0, 0, 0, time.Local))
		require.Len(t, february.Clearings, 1)
		assert.Equal(t, january.ID, february.Clearings[0].CommitmentJudgment)
		assert.True(t, february.Clearings[0].Done)
	})

	t.Run("a commitment judged at the previous month's final cut-off, asked at the next month's cut-off without flagging the earlier message again", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(31, 9), true, promise)
		th.say(t, time.Date(2026, time.February, 2, 9, 0, 0, 0, time.Local), true, kept)
		keys := th.keys(t)
		v := vendorFlagging(t, 0.9)
		m := e.method(jevKey, v, roomy)
		set := methods{jevKey: m}

		e.judgeMonth(t, set, jevKey)
		january := th.judgmentAt(t, m.Ref(), time.Date(2026, time.February, 1, 0, 0, 0, 0, time.Local))
		e.judgeMonthOf(t, "2026-02", set, jevKey)

		february := th.judgmentAt(t, m.Ref(), time.Date(2026, time.February, 3, 0, 0, 0, 0, time.Local))
		assert.Equal(t, keys[1:], february.NewMessages, "the earlier message is not new again")
		require.Len(t, february.Clearings, 1)
		assert.Equal(t, january.ID, february.Clearings[0].CommitmentJudgment)
		assert.True(t, february.Clearings[0].Done)
	})
}
