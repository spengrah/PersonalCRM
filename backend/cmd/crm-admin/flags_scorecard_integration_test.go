//go:build integration_testdb

package main

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reviewedThread judges a thread of a promise and its keeping with the
// method over January and reviews its second cut-off's ball-in-court outcome,
// leaving the first cut-off unreviewed.
func reviewedThread(t *testing.T, e *flagsEnv, set methods, key string) *thr {
	t.Helper()
	th := e.newThread(t)
	th.say(t, jan(3, 9), true, promise)
	th.say(t, jan(4, 9), true, kept)
	e.judgeMonth(t, set, key)
	reviewed := th.judgmentAt(t, set[key].Ref(), jan(5, 0))
	require.NotEmpty(t, reviewed.Outcomes)
	_, err := e.store.ReviewOutcome(e.ctx, reviewed.Outcomes[0].ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
	require.NoError(t, err)
	return th
}

// The scorecard's rerun of a method judges each reviewed thread's cut-offs in
// order from the start of the run the reviewed judgment appeared in, under the
// rule that reuses a judgment only when what it read is the same, and prints
// only IDs, counts, probabilities and reasons.
// spec: KNW-053.rerun-reviewed-examples, KNW-062.cut-offs-in-order-from-start, KNW-062.reuse-only-when-read-same, KNW-054.content-free-never-blocking
func TestScorecardRerunReplaysReviewedThreadsInOrder(t *testing.T) {
	t.Run("a reviewed thread whose later cut-off re-checks a commitment flagged at an earlier, unreviewed cut-off", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		jev := e.method(jevKey, vendorFlagging(t, false, 0.9), roomy)
		th := reviewedThread(t, e, methods{jevKey: jev}, jevKey)
		v := vendorFlagging(t, false, 0.9)
		clef := e.method(clefKey, v, roomy)

		out := e.scorecard(t, methods{clefKey: clef}, clefKey)

		calls := v.all()
		require.Len(t, calls, 2, "the rerun judges the unreviewed cut-off before the reviewed one")
		assert.Empty(t, calls[0].asks("clearing"))
		assert.Len(t, calls[1].asks("clearing"), 1, "the reviewed cut-off re-checks the commitment the earlier one flagged")
		reviewed := th.judgmentAt(t, clef.Ref(), jan(5, 0))
		require.Len(t, reviewed.Clearings, 1)
		assert.True(t, reviewed.Clearings[0].Done)
		assert.Contains(t, out, "scorecard: reviewed_examples=1")
		assertContentFree(t, out, th)
	})

	t.Run("a reviewed thread whose input another method judged in an earlier month's run", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		jev := e.method(jevKey, vendorFlagging(t, false, 0.9), roomy)
		th := reviewedThread(t, e, methods{jevKey: jev}, jevKey)
		other := judgment.MethodRef{Key: "unrelated", Version: "v", WordingVersion: "v"}
		from, to := jan(1, 0).AddDate(0, -1, 0), jan(1, 0)
		earlier, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: other, From: &from, To: &to})
		require.NoError(t, err)
		_, err = e.store.RecordJudgment(e.ctx, earlier, judgment.Judgment{
			Method: other, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: jan(5, 0), JudgedAt: jan(5, 0),
			InputHash: th.context(t, jan(5, 0)).InputHash,
		})
		require.NoError(t, err)
		v := vendorFlagging(t, false, 0.9)
		clef := e.method(clefKey, v, roomy)

		e.scorecard(t, methods{clefKey: clef}, clefKey)

		calls := v.all()
		require.Len(t, calls, 2, "the rerun starts from the run the reviewed judgment appeared in")
		assert.Len(t, calls[1].asks("clearing"), 1)
		reviewed := th.judgmentAt(t, clef.Ref(), jan(5, 0))
		require.Len(t, reviewed.Clearings, 1)
		assert.True(t, reviewed.Clearings[0].Done)
	})

	t.Run("a rerun where nothing changed, which reuses and sends nothing", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		v := vendorFlagging(t, false, 0.9)
		jev := e.method(jevKey, v, roomy)
		set := methods{jevKey: jev}
		th := reviewedThread(t, e, set, jevKey)
		sent := len(v.all())

		out := e.scorecard(t, set, jevKey)

		assert.Len(t, v.all(), sent, "nothing is sent")
		assert.Len(t, th.judgments(t, jev.Ref()), 2, "no judgment is added")
		rerun := e.lastRun(t, jev.Ref())
		assert.Equal(t, judgment.RunKindRerun, rerun.Kind)
		assert.Equal(t, 2, rerun.JudgedCount, "both cut-offs appear in the rerun, reused")
		assertContentFree(t, out, th)
	})

	t.Run("a rerun that leaves a message unjudged", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		jev := e.method(jevKey, vendorFlagging(t, false, 0.9), roomy)
		th := reviewedThread(t, e, methods{jevKey: jev}, jevKey)
		v := vendorFlagging(t, false, 0.9)
		v.fail = func(c call) bool { return len(c.Texts) == 1 }
		clef := e.method(clefKey, v, roomy)

		out := e.scorecard(t, methods{clefKey: clef}, clefKey)

		assert.Contains(t, out, "unjudged reason=call_failed n=1")
		assert.Contains(t, out, "scorecard: reviewed_examples=1")
		assert.False(t, strings.Contains(out, "unjudged reason=before_window"))
		assertContentFree(t, out, th)
	})
}

// The scorecard scores a method on the judgments its rerun appeared with, not
// on another run's judgment of the same example: a method version's judgment
// over a narrower window of the same example does not replace the one the
// rerun reused.
// spec: KNW-053.rerun-reviewed-examples
func TestScorecardScoresTheJudgmentsOfItsRerun(t *testing.T) {
	t.Run("a rerun that reuses a wide-window judgment beside a later narrow-window judgment of the same example", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		v := newVendor(t, false)
		th := e.newThread(t)
		th.say(t, jan(3, 9), false, filler(0))
		th.say(t, jan(4, 9), true, filler(1))
		wide := e.method(jevKey, v, roomy)
		e.judgeMonth(t, methods{jevKey: wide}, jevKey)
		keys := th.keys(t)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(5, 0))}, keys[1])
		v.bic = func(string) string { return "i_owe_reply" }
		narrow := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})
		e.judgeMonth(t, methods{jevKey: narrow}, jevKey)
		var widest judgment.JudgmentView
		for _, j := range th.judgments(t, wide.Ref()) {
			if j.CutOff.Equal(jan(5, 0)) && j.WindowStart == keys[0] {
				widest = j
			}
		}
		require.NotEmpty(t, widest.Outcomes, "the wide-window judgment of the reviewed cut-off")
		_, err := e.store.ReviewOutcome(e.ctx, widest.Outcomes[0].ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
		require.NoError(t, err)
		sent := len(v.all())

		out := e.scorecard(t, methods{jevKey: wide}, jevKey)

		assert.Len(t, v.all(), sent, "the rerun reuses the wide-window judgment")
		assert.Contains(t, out, "corrections_caused=0")
		assert.Contains(t, out, "reviewed=nobody_owes judged=nobody_owes n=1")
		assert.NotContains(t, out, "judged=i_owe_reply")
	})
}
