//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	decisions "personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// judgedWith records a judgment as judged does, with the ball-in-court
// outcomes given.
func (b *benchEnv) judgedWith(t *testing.T, run judgment.RunID, m judgment.MethodRef, th thread, cutOff time.Time, windowStart string, newKeys []string, probabilities map[string][3]float64, outcomes judgment.Outcomes) uuid.UUID {
	t.Helper()
	j := windowed(m, th, cutOff, "h-"+cutOff.UTC().Format(time.RFC3339Nano)+"-"+windowStart, windowStart, newKeys, outcomes)
	for _, key := range newKeys {
		p := probabilities[key]
		j.Flags = append(j.Flags, flagsOf(key, p[0], p[1], p[2])...)
	}
	_, err := b.store.RecordJudgment(b.ctx, run, j)
	require.NoError(t, err)
	return b.viewOf(t, th, m, cutOff).ID
}

// flagLabelsOf reads the thread's flag labels as a map from message and kind
// to whether the message holds the kind.
func (b *benchEnv) flagLabelsOf(t *testing.T, th thread) map[string]bool {
	t.Helper()
	labels, err := b.store.LabelsByThread(b.ctx, th.id)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, l := range labels.Flags {
		out[l.MessageKey+"/"+string(l.Kind)] = l.Holds
	}
	return out
}

func labelKey(key string, kind judgment.FlagKind) string { return key + "/" + string(kind) }

// owedAndNone is a one-to-one judgment of x and y: x is owed a reply by the
// user (an item) and y owes nothing (a negative decision).
func owedAndNone(x, y uuid.UUID) judgment.Outcomes {
	return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
		bic(x, judgment.BallInCourtIOweReply, 0.9), bic(y, judgment.BallInCourtNobodyOwes, 0.8),
	}}
}

// A close labels what each night the user touched left unflagged, all or
// nothing.
func TestCloseThread(t *testing.T) {
	t.Run("a night whose only label is on a reply item labels its unflagged judged messages and its reply negatives", func(t *testing.T) {
		// spec: IXN-092.flag-decisions-labeled-no, IXN-092.reply-negatives-labeled, IXN-092.answers-review-state, IXN-094.returned-with-every-answer
		b := newBenchEnv(t)
		x, y := b.contact(t), b.contact(t)
		th, keys := b.chat(t, 2, x, y)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judgedWith(t, run, m, th, b.cutAfter(2), keys[0], keys, nil, owedAndNone(x, y))
		reply := item(t, b.proposal(t, x, m), "i_owe_reply", x)
		b.act(t, reply, "confirm")
		require.Empty(t, b.flagLabelsOf(t, th))

		w := b.closeThread(t, th, run)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := b.flagLabelsOf(t, th)
		assert.Len(t, got, 6, "two judged messages, three kinds each")
		for _, key := range keys {
			for _, kind := range judgment.FlagKinds {
				holds, labeled := got[labelKey(key, kind)]
				assert.True(t, labeled, labelKey(key, kind))
				assert.False(t, holds, labelKey(key, kind))
			}
		}
		assert.Equal(t, "nobody_owes", b.labels(t)[decision(judgment.QuestionBallInCourt, &y)], "the reply negative gains its judged value")
		assert.Equal(t, "i_owe_reply", b.labels(t)[decision(judgment.QuestionBallInCourt, &x)], "the item keeps its label")
		answer := decodeData[handlers.ThreadReviewResponse](t, w)
		assert.Equal(t, "reviewed", answer.Status, "the answer carries the thread's status")
		night := answer.Nights[0]
		assert.Equal(t, 2, night.LabeledNone, "the answer reports how many unflagged messages the close labeled")
		assert.Equal(t, 2, b.reviewOf(t, th, run).Nights[0].LabeledNone, "and so does every later read")
	})

	t.Run("a night whose only label is on a clearing decision", func(t *testing.T) {
		// spec: IXN-092.flag-decisions-labeled-no
		b := newBenchEnv(t)
		m := b.compared(0)
		hot := b.threshold(t, m, judgment.FlagKindCommitment) + 0.2
		th, keys := b.conversation(t, 3)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		first := b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, 0, 0}})
		later := windowed(m, th, b.cutAfter(3), "h-later", keys[0], keys[1:], judgment.Outcomes{})
		later.Flags = append(flagsOf(keys[1], 0, 0, 0), flagsOf(keys[2], 0, 0, 0)...)
		later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: true, Probability: 0.9}}
		_, err := b.store.RecordJudgment(b.ctx, run, later)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, b.labelClearing(t, th, run, keys[0], b.cutAfter(3), true).Code)

		require.Equal(t, http.StatusOK, b.closeThread(t, th, run).Code)

		got := b.flagLabelsOf(t, th)
		for _, key := range keys[1:] {
			for _, kind := range judgment.FlagKinds {
				holds, labeled := got[labelKey(key, kind)]
				assert.True(t, labeled && !holds, labelKey(key, kind))
			}
		}
		for _, kind := range judgment.FlagKinds {
			_, labeled := got[labelKey(keys[0], kind)]
			if kind == judgment.FlagKindCommitment {
				assert.False(t, labeled, "the shown commitment of the night without a label stays to review")
			}
		}
	})

	t.Run("a night with no label gains nothing", func(t *testing.T) {
		// spec: IXN-092.unreviewed-night-gains-none
		b := newBenchEnv(t)
		x, y := b.contact(t), b.contact(t)
		th, keys := b.conversation(t, 1)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, owedAndNone(x, y))
		before := b.labels(t)

		require.Equal(t, http.StatusOK, b.closeThread(t, th, run).Code)

		assert.Empty(t, b.flagLabelsOf(t, th))
		assert.Equal(t, before, b.labels(t))
	})

	t.Run("a message another compared method's judgment in another run shows as an unreviewed commitment keeps that kind unlabeled", func(t *testing.T) {
		// spec: IXN-092.shown-flag-stays
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(1), judgment.FlagKindCommitment) + 0.2
		th, keys := b.conversation(t, 2)
		mine, theirs := b.compared(0), b.compared(1)
		runA, runB := b.startRun(t, judgment.RunKindHistorical, mine), b.startRun(t, judgment.RunKindHistorical, theirs)
		b.judged(t, runA, mine, th, b.cutAfter(2), keys[0], keys, nil)
		b.judged(t, runB, theirs, th, b.cutAfter(2), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, 0, 0}})
		b.labeled(t, th, runA, keys[1], "claim", true)

		require.Equal(t, http.StatusOK, b.closeThread(t, th, runA).Code)

		got := b.flagLabelsOf(t, th)
		_, shownLabeled := got[labelKey(keys[0], judgment.FlagKindCommitment)]
		assert.False(t, shownLabeled, "the other method shows it as a flag without a label")
		assert.False(t, got[labelKey(keys[0], judgment.FlagKindBigMoment)])
		assert.Contains(t, got, labelKey(keys[0], judgment.FlagKindBigMoment))
		assert.Contains(t, got, labelKey(keys[0], judgment.FlagKindClaim))
		assert.Contains(t, got, labelKey(keys[1], judgment.FlagKindCommitment))
		assert.True(t, got[labelKey(keys[1], judgment.FlagKindClaim)], "a label that exists is never replaced")
	})

	t.Run("a message only a judgment of a method the registry does not list shows as a flag gains not-holding for all three kinds", func(t *testing.T) {
		// spec: IXN-092.shown-flag-stays
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 2)
		m := b.compared(0)
		runA, runB := b.startRun(t, judgment.RunKindHistorical, m), b.startRun(t, judgment.RunKindRerun, otherMethod)
		b.judged(t, runA, m, th, b.cutAfter(2), keys[0], keys, nil)
		b.judged(t, runB, otherMethod, th, b.cutAfter(2), keys[0], keys[:1], map[string][3]float64{keys[0]: {0.95, 0.95, 0.95}})
		b.labeled(t, th, runA, keys[1], "claim", true)

		require.Equal(t, http.StatusOK, b.closeThread(t, th, runA).Code)

		got := b.flagLabelsOf(t, th)
		for _, kind := range judgment.FlagKinds {
			holds, labeled := got[labelKey(keys[0], kind)]
			assert.True(t, labeled && !holds, "an unlisted method never holds a label back: %s", kind)
		}
	})

	t.Run("a message_over_limit message in a labeled night gains no label, and a shown flag without a label stays", func(t *testing.T) {
		// spec: IXN-092.unreviewed-night-gains-none, IXN-092.shown-flag-stays
		b := newBenchEnv(t)
		m := b.compared(0)
		hot := b.threshold(t, m, judgment.FlagKindClaim) + 0.2
		th, keys := b.conversation(t, 3)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		cutOff := b.cutAfter(3)
		b.judged(t, run, m, th, cutOff, keys[0], keys[:2], map[string][3]float64{keys[0]: {0, 0, hot}})
		require.NoError(t, b.store.RecordUnjudged(b.ctx, run, []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: cutOff, MessageKey: keys[2], Reason: judgment.UnjudgedReasonMessageOverLimit},
		}))
		b.labeled(t, th, run, keys[1], "commitment", true)

		require.Equal(t, http.StatusOK, b.closeThread(t, th, run).Code)

		got := b.flagLabelsOf(t, th)
		for _, kind := range judgment.FlagKinds {
			assert.NotContains(t, got, labelKey(keys[2], kind), "an unjudged message gains no label")
		}
		assert.NotContains(t, got, labelKey(keys[0], judgment.FlagKindClaim), "a shown flag without a label stays unlabeled")
		assert.Contains(t, got, labelKey(keys[0], judgment.FlagKindCommitment))
		assert.True(t, got[labelKey(keys[1], judgment.FlagKindCommitment)], "a flag decision already labeled holding keeps its label")
	})

	t.Run("a close that fails partway records nothing", func(t *testing.T) {
		// spec: IXN-092.all-or-nothing
		f := &failingReviews{}
		b := newBenchEnv(t, withAsserter(func(a judgment.Asserter) judgment.Asserter {
			f.Asserter = a
			return f
		}))
		x, y := b.contact(t), b.contact(t)
		th, keys := b.chat(t, 1, x, y)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, owedAndNone(x, y))
		b.act(t, item(t, b.proposal(t, x, m), "i_owe_reply", x), "confirm")
		labels, flags := b.labels(t), b.flagLabelsOf(t, th)
		f.failOn(1)

		w := b.closeThread(t, th, run)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, flags, b.flagLabelsOf(t, th), "the flag labels written before the failure are rolled back")
		assert.Equal(t, labels, b.labels(t))
	})
}

// Confirm Nones labels a night that shows nothing and holds no label.
func TestConfirmNones(t *testing.T) {
	t.Run("a night that shows nothing labels its judged messages and its reply negatives", func(t *testing.T) {
		// spec: IXN-093.labels-the-night, IXN-093.answers-review-state, IXN-094.returned-with-every-answer
		b := newBenchEnv(t)
		x := b.contact(t)
		th, keys := b.conversation(t, 2)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		night := b.judgedWith(t, run, m, th, b.cutAfter(2), keys[0], keys, nil, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)},
		})
		require.True(t, b.reviewOf(t, th, run).Nights[0].ConfirmNones)

		w := b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+run.String(), nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := b.flagLabelsOf(t, th)
		assert.Len(t, got, 6)
		for k, holds := range got {
			assert.False(t, holds, k)
		}
		assert.Equal(t, "nobody_owes", b.labels(t)[decision(judgment.QuestionBallInCourt, &x)])
		review := decodeData[handlers.ThreadReviewResponse](t, w)
		assert.Equal(t, "reviewed", review.Status, "the answer is the thread's review state")
		assert.True(t, review.Nights[0].HoldsLabel)
	})

	refusals := []struct {
		name string
		seed func(t *testing.T, b *benchEnv) (scenario, uuid.UUID)
	}{
		{"a night showing a flag", func(t *testing.T, b *benchEnv) (scenario, uuid.UUID) {
			hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
			s := b.judgedNight(t, [3]float64{0, 0, hot})
			return s, s.night
		}},
		{"a night showing only a clearing decision", func(t *testing.T, b *benchEnv) (scenario, uuid.UUID) {
			m := b.compared(0)
			hot := b.threshold(t, m, judgment.FlagKindCommitment) + 0.2
			th, keys := b.conversation(t, 2)
			run := b.startRun(t, judgment.RunKindHistorical, m)
			first := b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, 0, 0}})
			later := windowed(m, th, b.cutAfter(2), "h-later", keys[0], keys[1:], judgment.Outcomes{})
			later.Flags = flagsOf(keys[1], 0, 0, 0)
			later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: true, Probability: 0.9}}
			_, err := b.store.RecordJudgment(b.ctx, run, later)
			require.NoError(t, err)
			return scenario{th: th, keys: keys, m: m, run: run}, b.viewOf(t, th, m, b.cutAfter(2)).ID
		}},
		{"a night showing only a reply item", func(t *testing.T, b *benchEnv) (scenario, uuid.UUID) {
			th, keys := b.conversation(t, 1)
			m := b.compared(0)
			run := b.startRun(t, judgment.RunKindHistorical, m)
			x := b.contact(t)
			night := b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, judgment.Outcomes{
				BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			})
			return scenario{th: th, keys: keys, m: m, run: run}, night
		}},
		{"a night holding a label", func(t *testing.T, b *benchEnv) (scenario, uuid.UUID) {
			s := b.judgedNight(t, [3]float64{0, 0, 0})
			b.labeled(t, s.th, s.run, s.keys[0], "claim", false)
			return s, s.night
		}},
	}
	for _, c := range refusals {
		t.Run(c.name+" is refused and records nothing", func(t *testing.T) {
			// spec: IXN-093.refuses-busy-night
			b := newBenchEnv(t)
			s, night := c.seed(t, b)
			flags, labels := b.flagLabelsOf(t, s.th), b.labels(t)

			w := b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+s.run.String(), nil)

			assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Equal(t, flags, b.flagLabelsOf(t, s.th))
			assert.Equal(t, labels, b.labels(t))
		})
	}

	t.Run("a judgment that is no night of the run is not found, and a request without a run is refused", func(t *testing.T) {
		// spec: IXN-093.unknown-judgment-not-found
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})
		elsewhere := b.startRun(t, judgment.RunKindHistorical, b.compared(0))

		assert.Equal(t, http.StatusNotFound, b.post(t, "/api/v1/judgments/"+s.night.String()+"/confirm-nones?run_id="+elsewhere.String(), nil).Code)
		assert.Equal(t, http.StatusNotFound, b.post(t, "/api/v1/judgments/"+uuid.NewString()+"/confirm-nones?run_id="+s.run.String(), nil).Code)
		assert.Equal(t, http.StatusBadRequest, b.post(t, "/api/v1/judgments/"+s.night.String()+"/confirm-nones", nil).Code)
		assert.Empty(t, b.flagLabelsOf(t, s.th))
	})

	t.Run("a Confirm Nones that fails partway records nothing", func(t *testing.T) {
		// spec: IXN-093.all-or-nothing
		f := &failingReviews{}
		b := newBenchEnv(t, withAsserter(func(a judgment.Asserter) judgment.Asserter {
			f.Asserter = a
			return f
		}))
		x := b.contact(t)
		th, keys := b.conversation(t, 1)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		night := b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)},
		})
		f.failOn(1)

		w := b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+run.String(), nil)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Empty(t, b.flagLabelsOf(t, th))
		assert.Empty(t, b.labels(t))
	})

	t.Run("a message another compared method shows as an unreviewed flag keeps that kind unlabeled", func(t *testing.T) {
		// spec: IXN-093.shown-flag-stays
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 1)
		mine, theirs := b.compared(0), b.compared(1)
		hot := b.threshold(t, theirs, judgment.FlagKindClaim) + 0.2
		runA, runB := b.startRun(t, judgment.RunKindHistorical, mine), b.startRun(t, judgment.RunKindHistorical, theirs)
		night := b.judged(t, runA, mine, th, b.cutAfter(1), keys[0], keys, nil)
		b.judged(t, runB, theirs, th, b.cutAfter(1), keys[0], keys, map[string][3]float64{keys[0]: {0, 0, hot}})

		w := b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+runA.String(), nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := b.flagLabelsOf(t, th)
		assert.NotContains(t, got, labelKey(keys[0], judgment.FlagKindClaim))
		assert.Contains(t, got, labelKey(keys[0], judgment.FlagKindCommitment))
		assert.Contains(t, got, labelKey(keys[0], judgment.FlagKindBigMoment))
	})

	t.Run("a message only a method the registry does not list shows as a flag gains not-holding", func(t *testing.T) {
		// spec: IXN-093.shown-flag-stays
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 1)
		mine := b.compared(0)
		runA, runB := b.startRun(t, judgment.RunKindHistorical, mine), b.startRun(t, judgment.RunKindRerun, otherMethod)
		night := b.judged(t, runA, mine, th, b.cutAfter(1), keys[0], keys, nil)
		b.judged(t, runB, otherMethod, th, b.cutAfter(1), keys[0], keys, map[string][3]float64{keys[0]: {0.9, 0.9, 0.9}})

		require.Equal(t, http.StatusOK, b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+runA.String(), nil).Code)

		got := b.flagLabelsOf(t, th)
		for _, kind := range judgment.FlagKinds {
			assert.Contains(t, got, labelKey(keys[0], kind))
		}
	})

	t.Run("a night with a message_over_limit message leaves that message unlabeled", func(t *testing.T) {
		// spec: IXN-093.labels-the-night
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 2)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		cutOff := b.cutAfter(2)
		night := b.judged(t, run, m, th, cutOff, keys[0], keys[:1], nil)
		require.NoError(t, b.store.RecordUnjudged(b.ctx, run, []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: cutOff, MessageKey: keys[1], Reason: judgment.UnjudgedReasonMessageOverLimit},
		}))

		require.Equal(t, http.StatusOK, b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+run.String(), nil).Code)

		got := b.flagLabelsOf(t, th)
		assert.Len(t, got, 3)
		for _, kind := range judgment.FlagKinds {
			assert.NotContains(t, got, labelKey(keys[1], kind))
		}
	})
}

// The server computes every thread's and night's status and returns it with the
// list, the thread read and every save.
func TestThreadStatus(t *testing.T) {
	// threeNights judges a thread on three nights, one new message each, and
	// returns it with the nights' judgments.
	threeNights := func(t *testing.T, b *benchEnv, probabilities map[int][3]float64) (scenario, []uuid.UUID) {
		t.Helper()
		th, keys := b.conversation(t, 3)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		var nights []uuid.UUID
		for i := range keys {
			p := map[string][3]float64{keys[i]: probabilities[i]}
			nights = append(nights, b.judged(t, run, m, th, b.cutAfter(i+1), keys[0], keys[i:i+1], p))
		}
		return scenario{th: th, keys: keys, m: m, run: run}, nights
	}
	statusOf := func(t *testing.T, b *benchEnv, s scenario) (string, int) {
		t.Helper()
		r := b.reviewOf(t, s.th, s.run)
		list, _ := b.threadList(t, "run_id="+s.run.String())
		require.Len(t, list.Items, 1)
		assert.Equal(t, r.Status, list.Items[0].Review.Status, "the list and the read agree")
		assert.Equal(t, r.ToReview, list.Items[0].Review.ToReview)
		return r.Status, r.ToReview
	}

	t.Run("a thread whose nights show nothing and hold no label is nothing flagged", func(t *testing.T) {
		// spec: IXN-094.thread-nothing-flagged
		b := newBenchEnv(t)
		s, _ := threeNights(t, b, nil)

		status, _ := statusOf(t, b, s)

		assert.Equal(t, "nothing_flagged", status)
	})

	t.Run("a thread with two unlabeled chips and one empty night offering Confirm Nones is to review with 3", func(t *testing.T) {
		// spec: IXN-094.thread-to-review
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		s, _ := threeNights(t, b, map[int][3]float64{0: {0, 0, hot}, 2: {hot, 0, 0}})

		status, n := statusOf(t, b, s)

		assert.Equal(t, "to_review", status)
		assert.Equal(t, 3, n)
	})

	t.Run("a thread whose shown items all hold labels and whose one empty night offers Confirm Nones is to review with 1", func(t *testing.T) {
		// spec: IXN-094.thread-to-review, IXN-094.returned-with-every-answer
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		s, _ := threeNights(t, b, map[int][3]float64{0: {0, 0, hot}, 2: {hot, 0, 0}})
		b.labeled(t, s.th, s.run, s.keys[0], "claim", true)
		answer := b.labeled(t, s.th, s.run, s.keys[2], "commitment", false)

		status, n := statusOf(t, b, s)

		assert.Equal(t, "to_review", status)
		assert.Equal(t, 1, n)
		assert.Equal(t, status, answer.Status, "a save answers with the thread's new status")
		assert.Equal(t, n, answer.ToReview)
	})

	t.Run("a thread every night of which holds a label is reviewed, and a night without one among labeled nights is to review", func(t *testing.T) {
		// spec: IXN-094.thread-reviewed
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		s, _ := threeNights(t, b, map[int][3]float64{0: {0, 0, hot}, 1: {0, 0, hot}, 2: {0, 0, hot}})
		b.labeled(t, s.th, s.run, s.keys[0], "claim", true)
		partial := b.labeled(t, s.th, s.run, s.keys[1], "claim", false)
		require.Equal(t, "to_review", partial.Status)
		assert.Equal(t, 1, partial.ToReview, "the third night's chip")

		answer := b.labeled(t, s.th, s.run, s.keys[2], "claim", true)

		assert.Equal(t, "reviewed", answer.Status)
		status, _ := statusOf(t, b, s)
		assert.Equal(t, "reviewed", status)
	})
}

// A label on a flag decision or a clearing decision changes no live state.
func TestLabelsChangeNothingLive(t *testing.T) {
	m := decisions.NewRegistry(config.DecisionConfig{}, nil).Keys()[0]
	b := newBenchEnvFor(t, m)
	mref := b.compared(0)
	m2 := mref
	hot := b.threshold(t, m2, judgment.FlagKindCommitment) + 0.2
	x := b.contact(t)
	th, keys := b.chat(t, 3, x)
	run := b.startRun(t, judgment.RunKindScheduled, m2)
	first := b.judged(t, run, m2, th, b.cutAfter(1), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, hot, hot}})
	later := windowed(m2, th, b.cutAfter(3), "h-later", keys[0], keys[1:], judgment.Outcomes{
		BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
	})
	later.Flags = append(flagsOf(keys[1], 0, 0, 0), flagsOf(keys[2], 0, 0, 0)...)
	later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: true, Probability: 0.9}}
	_, err := b.store.RecordJudgment(b.ctx, run, later)
	require.NoError(t, err)
	obligations, err := b.store.Obligations(b.ctx, th.id)
	require.NoError(t, err)
	require.NotEmpty(t, b.openObligations(t, th), "the production method's current reply opened an obligation")
	outcomes := b.outcomes(t, th)

	for _, c := range []struct {
		name string
		do   func() int
	}{
		{"a confirmed commitment chip", func() int { return b.labelFlag(t, th, run, keys[0], "commitment", true).Code }},
		{"an added commitment", func() int { return b.labelFlag(t, th, run, keys[1], "commitment", true).Code }},
		{"a confirmed big moment", func() int { return b.labelFlag(t, th, run, keys[0], "big_moment", true).Code }},
		{"a confirmed claim", func() int { return b.labelFlag(t, th, run, keys[0], "claim", true).Code }},
		{"a confirmed clearing decision judged done", func() int { return b.labelClearing(t, th, run, keys[0], b.cutAfter(3), true).Code }},
		{"a removed commitment and a close", func() int {
			require.Equal(t, http.StatusOK, b.labelFlag(t, th, run, keys[0], "commitment", false).Code)
			return b.closeThread(t, th, run).Code
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// spec: IXN-095.flag-and-clearing-labels-change-nothing-live
			require.Equal(t, http.StatusOK, c.do())

			after, err := b.store.Obligations(b.ctx, th.id)
			require.NoError(t, err)
			assert.Equal(t, obligations, after, "no task or obligation opened, closed or changed")
			assert.Equal(t, outcomes, b.outcomes(t, th), "no outcome, claim or live flag changed")
		})
	}
}

// The review route, given a run, answers with the thread's review state in it.
func TestReviewRouteAnswersReviewState(t *testing.T) {
	t.Run("a review of a reply item with run_id carries the night's new status", func(t *testing.T) {
		// spec: IXN-096.run-answers-review-state, IXN-094.returned-with-every-answer
		b := newBenchEnv(t)
		x := b.contact(t)
		th, keys := b.chat(t, 1, x)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		})
		reply := item(t, b.proposal(t, x, m), "i_owe_reply", x)
		before := b.reviewOf(t, th, run)
		require.Equal(t, "to_review", before.Status)

		w := b.post(t, "/api/v1/judgment-outcomes/"+reply.OutcomeID+"/review?run_id="+run.String(), map[string]any{"right_outcome": "i_owe_reply"})

		got := decodeData[handlers.ReviewJudgmentOutcomeResponse](t, w)
		assert.Equal(t, reply.OutcomeID, got.ID)
		require.NotNil(t, got.ThreadReview)
		assert.Equal(t, "reviewed", got.ThreadReview.Status)
		assert.Equal(t, b.reviewOf(t, th, run), *got.ThreadReview, "as the thread read returns it")
	})

	t.Run("a review without run_id answers with the outcome alone", func(t *testing.T) {
		// spec: IXN-096.no-run-answers-outcome
		b := newBenchEnv(t)
		x := b.contact(t)
		th, keys := b.chat(t, 1, x)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judgedWith(t, run, m, th, b.cutAfter(1), keys[0], keys, nil, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		})
		reply := item(t, b.proposal(t, x, m), "i_owe_reply", x)

		w := b.post(t, "/api/v1/judgment-outcomes/"+reply.OutcomeID+"/review", map[string]any{"right_outcome": "nobody_owes"})

		got := decodeData[handlers.ReviewJudgmentOutcomeResponse](t, w)
		assert.Equal(t, reply.OutcomeID, got.ID)
		assert.Nil(t, got.ThreadReview)
		assert.Equal(t, http.StatusNotFound, b.post(t, "/api/v1/judgment-outcomes/"+reply.OutcomeID+"/review?run_id="+uuid.NewString(), map[string]any{"right_outcome": "nobody_owes"}).Code)
	})
}

// The judgment store reads a thread's current labels.
func TestLabelsByThread(t *testing.T) {
	t.Run("a decision labeled twice reads once, with the later label", func(t *testing.T) {
		// spec: KNW-077.current-label-only
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})
		b.labeled(t, s.th, s.run, s.keys[0], "claim", true)
		b.labeled(t, s.th, s.run, s.keys[0], "claim", false)

		labels, err := b.store.LabelsByThread(b.ctx, s.th.id)

		require.NoError(t, err)
		assert.Equal(t, []judgment.FlagLabel{{Thread: s.th.id, MessageKey: s.keys[0], Kind: judgment.FlagKindClaim, Holds: false}}, labels.Flags)
		assert.Empty(t, labels.Clearings)
	})

	t.Run("labels a close wrote read as labels made one by one", func(t *testing.T) {
		// spec: KNW-077.close-and-confirm-nones-read-as-any
		b := newBenchEnv(t)
		byHand, closed := b.judgedNight(t, [3]float64{0, 0, 0}), b.judgedNight(t, [3]float64{0, 0, 0})
		for _, key := range byHand.keys {
			for _, kind := range []string{"commitment", "big_moment", "claim"} {
				b.labeled(t, byHand.th, byHand.run, key, kind, false)
			}
		}
		b.labeled(t, closed.th, closed.run, closed.keys[0], "claim", false)
		require.Equal(t, http.StatusOK, b.closeThread(t, closed.th, closed.run).Code)

		one, err := b.store.LabelsByThread(b.ctx, byHand.th.id)
		require.NoError(t, err)
		other, err := b.store.LabelsByThread(b.ctx, closed.th.id)
		require.NoError(t, err)

		require.Len(t, one.Flags, 9)
		require.Len(t, other.Flags, 9)
		for i := range one.Flags {
			assert.Equal(t, one.Flags[i].Kind, other.Flags[i].Kind)
			assert.Equal(t, one.Flags[i].Holds, other.Flags[i].Holds)
		}
	})

	t.Run("a thread with no label reads as none", func(t *testing.T) {
		// spec: KNW-077.flag-labels-read
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})

		labels, err := b.store.LabelsByThread(b.ctx, s.th.id)

		require.NoError(t, err)
		assert.Empty(t, labels.Flags)
		assert.Empty(t, labels.Clearings)
	})
}
