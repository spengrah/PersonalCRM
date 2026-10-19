//go:build integration_testdb

package main

import (
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type probabilities map[string]float64

func (p probabilities) values() []float64 {
	var out []float64
	for _, v := range p {
		out = append(out, v)
	}
	return out
}

func openObligations(t *testing.T, th *thr) []judgment.Obligation {
	t.Helper()
	all, err := th.e.store.Obligations(th.e.ctx, th.venue)
	require.NoError(t, err)
	var open []judgment.Obligation
	for _, o := range all {
		if o.Open {
			open = append(open, o)
		}
	}
	return open
}

// With the method designated production, no flag decision, clearing decision
// or unjudged record opens, closes or changes a task, an obligation or a live
// flag, and the run's live effects count none for them.
// spec: KNW-060.opens-nothing, KNW-060.closes-and-changes-nothing
func TestJudgeMonthFlagsChangeNoLiveState(t *testing.T) {
	// The promise is flagged as a commitment whatever the case, so that a later
	// cut-off re-checks it.
	flagAt1 := func(kind judgment.FlagKind) func(ask) float64 {
		return func(a ask) float64 {
			switch {
			case a.Flag == kind:
				return 1
			case a.Flag == judgment.FlagKindCommitment && a.Text == promise:
				return 0.9
			}
			return 0.05
		}
	}
	cases := []struct {
		name string
		flag func(ask) float64
	}{
		{"a commitment flag at probability 1", flagAt1(judgment.FlagKindCommitment)},
		{"a big-moment flag at probability 1, which raises no live big-moment flag", flagAt1(judgment.FlagKindBigMoment)},
		{"a claim flag at probability 1", flagAt1(judgment.FlagKindClaim)},
		{"a clearing decision answered done on a commitment flagged at an earlier cut-off", flagAt1(judgment.FlagKindCommitment)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, jevKey)
			th := e.newThread(t)
			th.say(t, jan(3, 9), true, promise)
			th.say(t, jan(4, 9), true, kept)
			v := newVendor(t, false)
			v.flag, v.clear = tc.flag, func(ask) float64 { return 1 }
			m := e.method(jevKey, v, roomy)

			out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

			assert.Contains(t, flagsOf(th.judgmentAt(t, m.Ref(), jan(4, 0))).values(), 1.0, "the flag is recorded at probability 1")
			assert.NotEmpty(t, th.judgmentAt(t, m.Ref(), jan(5, 0)).Clearings, "the commitment is re-checked and cleared")
			assert.Contains(t, out, "production=true")
			assert.Contains(t, out, "live_effects obligations_opened=0 flags_raised=0 claims_raised=0")
			assert.Empty(t, openObligations(t, th))
		})
	}

	t.Run("a reply the method says is owed still opens its obligation", func(t *testing.T) {
		e := newFlagsEnv(t, jevKey)
		th := e.newThread(t)
		th.say(t, jan(3, 9), false, "can you send me the recipe")
		v := newVendor(t, false)
		v.bic = func(string) string { return "i_owe_reply" }
		m := e.method(jevKey, v, roomy)

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Contains(t, out, "live_effects obligations_opened=1")
		assert.Len(t, openObligations(t, th), 1)
	})
}

// With the method designated production, ball-in-court is asked per
// participant at every cut-off over the window under today's apply rule; a
// reply obligation's evidence is the thread's newest message at the cut-off
// that opened it, and a judgment whose window starts after that message
// neither keeps nor removes the obligation.
// spec: CAD-061.evidence-is-newest-message-at-opening, CAD-061.window-after-evidence-neither-keeps-nor-removes, CAD-061.window-at-evidence-counts-inside, CAD-061.unknown-evidence-preserved, KNW-060.reply-obligations-keep-apply-rule
func TestJudgeMonthAppliesReplyObligationsWithinTheWindow(t *testing.T) {
	// The thread holds m0 to m3: m1 is the newest message at the cut-off that
	// opens a reply obligation; the next run judges a later cut-off over a
	// window starting at m0, m1 or m2 and says nobody owes a reply.
	cases := []struct {
		name   string
		start  int
		claims int
	}{
		{"evidence inside the window, where today's apply rule acts", 0, 1},
		{"a window starting exactly at the evidence message, which counts as inside", 1, 1},
		{"evidence before the window start, where the obligation stays as it was", 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, jevKey)
			th := e.newThread(t)
			th.sayFiller(t, jan(2, 9), false, 0)
			th.sayFiller(t, jan(3, 9), false, 1)
			v := newVendor(t, false)
			v.bic = func(string) string { return "i_owe_reply" }
			opening := e.method(jevKey, v, roomy)
			out := e.judgeMonth(t, methods{jevKey: opening}, jevKey)
			require.Contains(t, out, "live_effects obligations_opened=1")
			th.sayFiller(t, jan(10, 9), false, 2)
			th.sayFiller(t, jan(10, 10), true, 3)
			keys := th.keys(t)
			previous := jan(10, 0)
			limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(11, 0)), NewAfter: &previous}, keys[tc.start])
			v.bic = func(string) string { return "nobody_owes" }
			windowed := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})
			require.Equal(t, opening.Ref(), windowed.Ref(), "limits are no part of a method's version")

			out = e.judgeMonth(t, methods{jevKey: windowed}, jevKey)

			last := th.judgmentAt(t, windowed.Ref(), jan(11, 0))
			require.Equal(t, keys[tc.start], last.WindowStart)
			assert.Contains(t, out, "live_effects obligations_opened=0 flags_raised=0 claims_raised="+string(rune('0'+tc.claims)))
			assert.Len(t, openObligations(t, th), 1, "a claim removes nothing")
		})
	}

	t.Run("an obligation with no recorded evidence under a windowed judgment, which is preserved", func(t *testing.T) {
		e := newFlagsEnv(t, jevKey)
		th := e.newThread(t)
		// The obligation opens before the thread holds any message.
		ref := judgment.MethodRef{Key: jevKey, Version: "earlier", WordingVersion: "earlier"}
		runID, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: ref})
		require.NoError(t, err)
		effects, err := e.store.RecordJudgment(e.ctx, runID, judgment.Judgment{
			Method: ref, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: jan(1, 0), JudgedAt: jan(1, 0), InputHash: "empty",
			Outcomes: judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
				{QuestionID: "ball_in_court/THEM", ContactID: th.contact, Outcome: judgment.BallInCourtIOweReply, Confidence: 0.9},
			}},
		})
		require.NoError(t, err)
		require.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		th.sayFiller(t, jan(3, 9), false, 0)
		v := newVendor(t, false)
		m := e.method(jevKey, v, roomy)

		out := e.judgeMonth(t, methods{jevKey: m}, jevKey)

		assert.Contains(t, out, "live_effects obligations_opened=0 flags_raised=0 claims_raised=0")
		assert.Len(t, openObligations(t, th), 1)
	})
}
