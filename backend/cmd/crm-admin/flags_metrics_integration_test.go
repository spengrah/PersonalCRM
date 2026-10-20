//go:build integration_testdb

package main

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scoredThread is a thread jev-masked judged over January, whose vendor gives
// each message the flag probabilities its text names, and the Threads tab
// that labels it under that run.
type scoredThread struct {
	*thr
	set     methods
	run     judgment.RunID
	threads *judgment.Threads
}

// said is a message of a thread: its January day and its text.
type said struct {
	day  int
	text string
}

// judgedThread stores the messages, a minute apart from 09:00 on their day,
// the user's first and every second one after, and judges January with a vendor that flags each message's text
// at the probabilities flags names per kind, 0.05 where it names none.
func judgedThread(t *testing.T, flags map[string]map[judgment.FlagKind]float64, messages ...said) *scoredThread {
	t.Helper()
	byText := map[string]map[judgment.FlagKind]float64{}
	for text, p := range flags {
		byText[strings.TrimSpace(text)] = p
	}
	v := newVendor(t)
	v.flag = func(a ask) float64 {
		if p, ok := byText[a.Text][a.Flag]; ok {
			return p
		}
		return 0.05
	}
	return judgeThread(t, v, messages...)
}

// clearedThread is a promise on January 3 and the contact's reply on January
// 4, judged over January with a vendor that flags the promise a commitment and
// answers its clearing on January 4's night at done.
func clearedThread(t *testing.T, done float64) *scoredThread {
	t.Helper()
	v := vendorFlagging(t, 0.9)
	v.clear = func(ask) float64 { return done }
	return judgeThread(t, v, said{3, promise}, said{4, filler(0)})
}

// judgeThread stores the messages, a minute apart from 09:00 on their day,
// the user's first and every second one after, and judges January with the
// vendor.
func judgeThread(t *testing.T, v *vendor, messages ...said) *scoredThread {
	t.Helper()
	e := newFlagsEnv(t, "")
	th := e.newThread(t)
	for i, m := range messages {
		th.say(t, jan(m.day, 9).Add(time.Duration(i)*time.Minute), i%2 == 0, m.text)
	}
	set := methods{jevKey: e.method(jevKey, v, roomy)}
	e.judgeMonth(t, set, jevKey)
	return &scoredThread{thr: th, set: set, run: e.lastRun(t, set[jevKey].Ref()).ID, threads: judgment.NewThreads(e.store, set)}
}

// labelClearing records, under the run, whether the commitment its judgment at
// the cut-off re-checked is done.
func (s *scoredThread) labelClearing(t *testing.T, cutOff time.Time, done bool) {
	t.Helper()
	j := s.judgmentAt(t, s.set[jevKey].Ref(), cutOff)
	require.Len(t, j.Clearings, 1)
	_, err := s.threads.LabelClearing(s.e.ctx, s.run, s.venue, j.Clearings[0].CommitmentMessageKey, j.CutOff, done)
	require.NoError(t, err)
}

// label records, under the run, whether the i-th message holds the kind.
func (s *scoredThread) label(t *testing.T, i int, kind judgment.FlagKind, holds bool) {
	t.Helper()
	_, err := s.threads.LabelFlag(s.e.ctx, s.run, s.venue, s.keys(t)[i], kind, holds)
	require.NoError(t, err)
}

// close closes the thread under the run.
func (s *scoredThread) close(t *testing.T) {
	t.Helper()
	_, err := s.threads.Close(s.e.ctx, s.run, s.venue)
	require.NoError(t, err)
}

// scorecard runs crm-admin --scorecard for jev-masked.
func (s *scoredThread) scorecard(t *testing.T) string {
	t.Helper()
	return s.e.scorecard(t, s.set, jevKey)
}

// line returns the scorecard's line that starts with prefix after its indent.
func line(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.TrimSpace(l)
		}
	}
	require.Failf(t, "no scorecard line", "%q in:\n%s", prefix, out)
	return ""
}

// The scorecard reports, per method and flag kind, precision and recall over
// the labeled flag decisions of its rerun: a decision shows when its
// probability reaches the method's display threshold, and a decision without a
// label counts in neither.
func TestScorecardScoresFlagDecisions(t *testing.T) {
	t.Run("a shown flag labeled as holding", func(t *testing.T) {
		// spec: KNW-085.rerun-covers-labeled-decisions, KNW-085.precision-recall-over-labels
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9}}, said{3, promise})
		s.label(t, 0, judgment.FlagKindCommitment, true)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=commitment precision=1.000 recall=1.000 shown_held=1 shown_not_held=0 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=commitment "))
		assertContentFree(t, out, s.thr)
	})

	t.Run("a shown flag labeled no", func(t *testing.T) {
		// spec: KNW-085.precision-recall-over-labels, KNW-085.rate-without-labels-none
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9}}, said{3, promise})
		s.label(t, 0, judgment.FlagKindCommitment, false)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=commitment precision=0.000 recall=none shown_held=0 shown_not_held=1 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=commitment "))
	})

	t.Run("an unflagged decision labeled as holding: an Added flag", func(t *testing.T) {
		// spec: KNW-085.precision-recall-over-labels, KNW-085.rate-without-labels-none
		s := judgedThread(t, nil, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindClaim, true)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=claim precision=none recall=0.000 shown_held=0 shown_not_held=0 unshown_held=1 unshown_not_held=0",
			line(t, out, "flag kind=claim "))
	})

	t.Run("an unflagged decision labeled no by a close", func(t *testing.T) {
		// spec: KNW-085.precision-recall-over-labels
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9}}, said{3, promise}, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindCommitment, true)
		s.close(t)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=commitment precision=1.000 recall=1.000 shown_held=1 shown_not_held=0 unshown_held=0 unshown_not_held=1",
			line(t, out, "flag kind=commitment "), "the close labeled the other message's commitment no")
		assert.Equal(t, "flag kind=claim precision=none recall=none shown_held=0 shown_not_held=0 unshown_held=0 unshown_not_held=2",
			line(t, out, "flag kind=claim "))
	})

	t.Run("a decision without a label", func(t *testing.T) {
		// spec: KNW-085.unlabeled-counted-in-neither, KNW-087.judged-unjudged-flagged-counted
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9}, filler(0): {judgment.FlagKindCommitment: 0.9}},
			said{3, promise}, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindCommitment, true)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=commitment precision=1.000 recall=1.000 shown_held=1 shown_not_held=0 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=commitment "), "the second message's shown commitment holds no label")
		assert.Equal(t, "flagged kind=commitment n=2", line(t, out, "flagged kind=commitment "))
	})

	t.Run("a probability exactly at the display threshold, counted as flagged", func(t *testing.T) {
		// spec: KNW-085.shown-at-threshold
		threshold, err := newFlagsEnv(t, "").store.DisplayThreshold(t.Context(), jevKey, judgment.FlagKindBigMoment)
		require.NoError(t, err)
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{filler(0): {judgment.FlagKindBigMoment: threshold}}, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindBigMoment, true)

		out := s.scorecard(t)

		assert.Equal(t, "flag kind=big_moment precision=1.000 recall=1.000 shown_held=1 shown_not_held=0 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=big_moment "))
	})
}

// The scorecard reports, per method, clearing accuracy over the labeled
// clearing decisions of its rerun; a clearing decision without a label counts
// in neither.
func TestScorecardScoresClearingDecisions(t *testing.T) {
	t.Run("a done answer labeled done", func(t *testing.T) {
		// spec: KNW-086.accuracy-over-labeled-clearings
		s := clearedThread(t, 0.9)
		s.labelClearing(t, jan(5, 0), true)

		out := s.scorecard(t)

		assert.Equal(t, "clearing accuracy=1.000 done_labeled_done=1 done_labeled_not_done=0 not_done_labeled_done=0 not_done_labeled_not_done=0",
			line(t, out, "clearing "))
		assertContentFree(t, out, s.thr)
	})

	t.Run("a done answer labeled not done", func(t *testing.T) {
		// spec: KNW-086.accuracy-over-labeled-clearings
		s := clearedThread(t, 0.9)
		s.labelClearing(t, jan(5, 0), false)

		out := s.scorecard(t)

		assert.Equal(t, "clearing accuracy=0.000 done_labeled_done=0 done_labeled_not_done=1 not_done_labeled_done=0 not_done_labeled_not_done=0",
			line(t, out, "clearing "))
	})

	t.Run("a not-done answer labeled not done", func(t *testing.T) {
		// spec: KNW-086.accuracy-over-labeled-clearings
		s := clearedThread(t, 0.1)
		s.labelClearing(t, jan(5, 0), false)

		out := s.scorecard(t)

		assert.Equal(t, "clearing accuracy=1.000 done_labeled_done=0 done_labeled_not_done=0 not_done_labeled_done=0 not_done_labeled_not_done=1",
			line(t, out, "clearing "))
	})

	t.Run("a clearing decision without a label", func(t *testing.T) {
		// spec: KNW-086.unlabeled-clearing-counted-in-neither
		s := clearedThread(t, 0.9)
		s.label(t, 0, judgment.FlagKindCommitment, true)

		out := s.scorecard(t)

		require.Equal(t, "flag kind=commitment precision=1.000 recall=1.000 shown_held=1 shown_not_held=0 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=commitment "), "the rerun scored the thread")
		assert.Equal(t, "clearing accuracy=none done_labeled_done=0 done_labeled_not_done=0 not_done_labeled_done=0 not_done_labeled_not_done=0",
			line(t, out, "clearing "))
	})
}

// The scorecard reports, per method, how many new messages its rerun judged,
// left unjudged by reason, and flagged per kind; no metric counts an unjudged
// message as a no.
func TestScorecardCountsJudgedUnjudgedAndFlaggedMessages(t *testing.T) {
	t.Run("a message left unjudged after a failed call that the user labeled as holding a commitment", func(t *testing.T) {
		// spec: KNW-087.judged-unjudged-flagged-counted, KNW-087.unjudged-never-a-no
		s := judgedThread(t, nil, said{3, promise})
		s.label(t, 0, judgment.FlagKindCommitment, true)
		v := newVendor(t)
		v.fail = func(call) bool { return true }

		out := s.e.scorecard(t, methods{clefKey: s.e.method(clefKey, v, roomy)}, clefKey)

		assert.Equal(t, "messages judged=0", line(t, out, "messages "))
		assert.Equal(t, "unjudged reason=call_failed n=1", line(t, out, "unjudged "))
		assert.Equal(t, "flag kind=commitment precision=none recall=none shown_held=0 shown_not_held=0 unshown_held=0 unshown_not_held=0",
			line(t, out, "flag kind=commitment "), "outside recall's denominator")
	})

	t.Run("a message_over_limit message in a night whose other messages were judged", func(t *testing.T) {
		// spec: KNW-087.judged-unjudged-flagged-counted, KNW-087.unjudged-never-a-no
		e := newFlagsEnv(t, "")
		th, keys, _ := e.nightOf(t, filler(0), filler(1), filler(2), huge, filler(4), filler(5))
		limit := e.windowLimit(t, fakeDefinition(jevKey, roomy), th, jan(5, 0), 4, 4)
		set := methods{jevKey: e.method(jevKey, newVendor(t), decision.Limits{Tokens: limit, Questions: 1 << 20})}
		e.judgeMonth(t, set, jevKey)
		s := &scoredThread{thr: th, set: set, run: e.lastRun(t, set[jevKey].Ref()).ID, threads: judgment.NewThreads(e.store, set)}
		s.label(t, 1, judgment.FlagKindCommitment, true)
		s.label(t, 3, judgment.FlagKindCommitment, true)
		require.Equal(t, keys[3], e.lastRun(t, set[jevKey].Ref()).Unjudged[0].MessageKey)

		out := s.scorecard(t)

		assert.Equal(t, "messages judged=5", line(t, out, "messages "), "the first night's message and the second night's four")
		assert.Equal(t, "unjudged reason=message_over_limit n=1", line(t, out, "unjudged "))
		assert.Equal(t, "flag kind=commitment precision=none recall=0.000 shown_held=0 shown_not_held=0 unshown_held=1 unshown_not_held=0",
			line(t, out, "flag kind=commitment "), "the oversized message's label counts in no cell")
	})

	t.Run("a run with every message judged", func(t *testing.T) {
		// spec: KNW-087.judged-unjudged-flagged-counted
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9, judgment.FlagKindClaim: 0.6}},
			said{3, promise}, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindCommitment, true)

		out := s.scorecard(t)

		assert.Equal(t, "messages judged=2", line(t, out, "messages "))
		assert.NotContains(t, out, "unjudged reason=")
		assert.Equal(t, "flagged kind=commitment n=1", line(t, out, "flagged kind=commitment "))
		assert.Equal(t, "flagged kind=big_moment n=0", line(t, out, "flagged kind=big_moment "))
		assert.Equal(t, "flagged kind=claim n=1", line(t, out, "flagged kind=claim "))
	})
}

// Flag metrics count only flag and clearing labels: the user's reviews of an
// earlier method version's whole-thread commitment and big-moment outcomes
// count toward no flag metric.
func TestScorecardFlagMetricsCountOnlyFlagAndClearingLabels(t *testing.T) {
	t.Run("a January thread holding an earlier version's reviewed commitment outcome and no flag label", func(t *testing.T) {
		// spec: KNW-085.only-flag-labels-count
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), true, promise)
		earlier := judgment.MethodRef{Key: jevKey, Version: "whole-thread", WordingVersion: "whole-thread"}
		from, to := jan(1, 0), jan(1, 0).AddDate(0, 1, 0)
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: earlier, From: &from, To: &to})
		require.NoError(t, err)
		_, err = e.store.RecordJudgment(e.ctx, run, judgment.Judgment{
			Method: earlier, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: jan(4, 0), JudgedAt: jan(4, 0),
			InputHash: th.context(t, jan(4, 0)).InputHash,
			Outcomes: judgment.Outcomes{
				Commitments: []judgment.CommitmentOutcome{{QuestionID: "commitment_mine", ContactID: th.contact,
					Direction: judgment.CommitmentDirectionMine, Outcome: judgment.CommitmentStateOpen, Confidence: 0.9}},
				BigMoment: judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: judgment.BigMomentParticipant, Confidence: 0.8},
			},
		})
		require.NoError(t, err)
		require.NoError(t, e.store.FinishRun(e.ctx, run))
		reviewed := th.judgmentAt(t, earlier, jan(4, 0))
		for _, o := range reviewed.Outcomes {
			_, err := e.store.ReviewOutcome(e.ctx, o.ID, judgment.ReviewRequest{RightOutcome: o.Outcome})
			require.NoError(t, err)
		}
		set := methods{jevKey: e.method(jevKey, vendorFlagging(t, 0.9), roomy)}

		out := e.scorecard(t, set, jevKey)

		require.Equal(t, "messages judged=1", line(t, out, "messages "), "the rerun judged the reviewed thread")
		require.Equal(t, "flagged kind=commitment n=1", line(t, out, "flagged kind=commitment "))
		for _, kind := range judgment.FlagKinds {
			assert.Equal(t, "flag kind="+string(kind)+" precision=none recall=none shown_held=0 shown_not_held=0 unshown_held=0 unshown_not_held=0",
				line(t, out, "flag kind="+string(kind)+" "))
		}
		assert.Equal(t, "clearing accuracy=none done_labeled_done=0 done_labeled_not_done=0 not_done_labeled_done=0 not_done_labeled_not_done=0",
			line(t, out, "clearing "))
	})
}
