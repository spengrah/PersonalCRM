//go:build integration_testdb

package judgment_test

import (
	"slices"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messageKeys returns the keys of the thread's messages in thread order, each
// message once.
func (e *env) messageKeys(t *testing.T, th thread) []string {
	t.Helper()
	rows, err := repository.NewThreadContentRepository(e.database.Queries).ListThreadContent(e.ctx, th.id, accelerated.GetCurrentTime())
	require.NoError(t, err)
	var keys []string
	for _, r := range rows {
		if len(keys) == 0 || keys[len(keys)-1] != r.MessageKey {
			keys = append(keys, r.MessageKey)
		}
	}
	return keys
}

// windowed is a judgment of a version that reads a window: it starts at
// windowStart and answers the flag questions of the new messages.
func windowed(m judgment.MethodRef, th thread, cutOff time.Time, inputHash, windowStart string, newMessages []string, outcomes judgment.Outcomes) judgment.Judgment {
	j := judgmentOf(m, th, cutOff, inputHash, outcomes)
	j.WindowStart, j.NewMessages = windowStart, newMessages
	return j
}

// flagsOf flags each message as the probabilities give, in FlagKinds order.
func flagsOf(key string, commitment, bigMoment, claim float64) []judgment.FlagDecision {
	return []judgment.FlagDecision{
		{MessageKey: key, Kind: judgment.FlagKindCommitment, Probability: commitment},
		{MessageKey: key, Kind: judgment.FlagKindBigMoment, Probability: bigMoment},
		{MessageKey: key, Kind: judgment.FlagKindClaim, Probability: claim},
	}
}

// viewOf returns the one judgment of the method on the thread at the cut-off.
func (e *env) viewOf(t *testing.T, th thread, m judgment.MethodRef, cutOff time.Time) judgment.JudgmentView {
	t.Helper()
	views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
	require.NoError(t, err)
	var found []judgment.JudgmentView
	for _, v := range views {
		if v.Method == m && v.CutOff.Equal(cutOff) {
			found = append(found, v)
		}
	}
	require.Len(t, found, 1, "judgments of %s at %s", m.Key, cutOff)
	return found[0]
}

// The store's judgment reads return what each judgment read and answered about
// messages beside today's fields, and its run read the messages no judgment
// answered.
func TestJudgmentReadsReturnFlagRecords(t *testing.T) {
	t.Run("a judgment that read a window returns its window start, new messages, flags and clearings", func(t *testing.T) {
		// spec: KNW-063.judgment-reads-carry-window-and-decisions
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		for day := 1; day <= 3; day++ {
			e.message(t, th, c, false, e.day(day))
		}
		keys := e.messageKeys(t, th)
		require.Len(t, keys, 3)
		earlier := windowed(otherMethod, th, e.day(1), "h1", keys[0], keys[:1], judgment.Outcomes{})
		earlier.Flags = flagsOf(keys[0], 0.8, 0.1, 0.2)
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindRerun, Method: otherMethod})
		require.NoError(t, err)
		_, err = e.store.RecordJudgment(e.ctx, run, earlier)
		require.NoError(t, err)
		first := e.viewOf(t, th, otherMethod, e.day(1))

		later := windowed(otherMethod, th, e.day(3), "h3", keys[0], keys[1:], judgment.Outcomes{})
		later.Flags = append(flagsOf(keys[1], 0.05, 0.9, 0.4), flagsOf(keys[2], 0.6, 0.0, 1.0)...)
		later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first.ID, Done: true, Probability: 0.85}}
		_, err = e.store.RecordJudgment(e.ctx, run, later)
		require.NoError(t, err)

		byThread := e.viewOf(t, th, otherMethod, e.day(3))
		assert.Equal(t, keys[0], byThread.WindowStart)
		assert.Equal(t, keys[1:], byThread.NewMessages, "the new messages in thread order")
		assert.Equal(t, later.Flags, byThread.Flags, "one flag decision per new message and kind, in thread order")
		assert.Equal(t, later.Clearings, byThread.Clearings)
		assert.Equal(t, flagsOf(keys[0], 0.8, 0.1, 0.2), first.Flags)
		assert.Empty(t, first.Clearings)
		byRun, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Run: &run})
		require.NoError(t, err)
		require.Len(t, byRun, 2)
		assert.Equal(t, byThread, byRun[1], "the read by run returns what the read by thread returns")
	})

	t.Run("a judgment of a version before the windows returns no flags, clearings or new messages, and its window at the context's first message", func(t *testing.T) {
		// spec: KNW-063.earlier-version-reads-back-empty
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.message(t, th, c, false, e.day(2))
		keys := e.messageKeys(t, th)
		e.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, e.day(2), "h2", judgment.Outcomes{}))

		view := e.viewOf(t, th, otherMethod, e.day(2))

		assert.Equal(t, keys[0], view.WindowStart)
		assert.Empty(t, view.NewMessages)
		assert.Empty(t, view.Flags)
		assert.Empty(t, view.Clearings)
	})

	t.Run("one message judged by two methods is returned under the same message key and kind by each", func(t *testing.T) {
		// spec: KNW-063.judgment-reads-carry-window-and-decisions
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		key := e.messageKeys(t, th)[0]
		for _, m := range []judgment.MethodRef{prodMethod, otherMethod} {
			j := windowed(m, th, e.day(1), "h1", key, []string{key}, judgment.Outcomes{})
			j.Flags = flagsOf(key, 0.7, 0.2, 0.1)
			e.run(t, judgment.RunKindRerun, j)
		}

		for _, m := range []judgment.MethodRef{prodMethod, otherMethod} {
			assert.Equal(t, flagsOf(key, 0.7, 0.2, 0.1), e.viewOf(t, th, m, e.day(1)).Flags, m.Key)
		}
	})

	t.Run("a run's unjudged messages are listed and counted by reason", func(t *testing.T) {
		// spec: KNW-063.run-read-counts-unjudged-by-reason, KNW-058.recorded-unjudged-with-reason, KNW-058.counted-unjudged-never-no
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		for day := 1; day <= 3; day++ {
			e.message(t, th, c, false, e.day(day))
		}
		keys := e.messageKeys(t, th)
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: prodMethod})
		require.NoError(t, err)
		unjudged := []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: e.day(1), MessageKey: keys[0], Reason: judgment.UnjudgedReasonCallFailed},
			{Thread: th.id, CutOff: e.day(3), MessageKey: keys[1], Reason: judgment.UnjudgedReasonBeforeWindow},
			{Thread: th.id, CutOff: e.day(3), MessageKey: keys[2], Reason: judgment.UnjudgedReasonMessageOverLimit},
			{Thread: th.id, CutOff: e.day(2), MessageKey: keys[1], Reason: judgment.UnjudgedReasonCallFailed},
		}
		require.NoError(t, e.store.RecordUnjudged(e.ctx, run, unjudged))
		require.NoError(t, e.store.FinishRun(e.ctx, run))

		s := runSummary(t, mustListRuns(t, e), run)

		require.Len(t, s.Unjudged, len(unjudged))
		for _, m := range unjudged {
			assert.Condition(t, func() bool {
				return slices.ContainsFunc(s.Unjudged, func(got judgment.UnjudgedMessage) bool {
					return got.Thread == m.Thread && got.CutOff.Equal(m.CutOff) && got.MessageKey == m.MessageKey && got.Reason == m.Reason
				})
			}, "unjudged %+v", m)
		}
		assert.Equal(t, map[judgment.UnjudgedReason]int{
			judgment.UnjudgedReasonCallFailed: 2, judgment.UnjudgedReasonBeforeWindow: 1, judgment.UnjudgedReasonMessageOverLimit: 1,
		}, s.UnjudgedCounts)
		assert.Equal(t, s.UnjudgedCounts, finishedRun(t, e, run).Unjudged)
		assert.Empty(t, s.Incomplete, "an unjudged message is no incomplete thread")
		assert.Empty(t, e.outcomes(t, th), "and no flag decision answered no")
	})

	t.Run("a context reported message_over_limit is listed among the run's incomplete threads with that reason", func(t *testing.T) {
		// spec: KNW-055.newest-message-over-limit-incomplete
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: prodMethod})
		require.NoError(t, err)
		require.NoError(t, e.store.RecordIncomplete(e.ctx, run, th.id, e.day(1), judgment.IncompleteReasonMessageOverLimit))

		s := runSummary(t, mustListRuns(t, e), run)

		require.Len(t, s.Incomplete, 1)
		assert.Equal(t, th.id, s.Incomplete[0].Thread)
		assert.Equal(t, judgment.IncompleteReasonMessageOverLimit, s.Incomplete[0].Reason)
	})
}

func mustListRuns(t *testing.T, e *env) []judgment.RunSummary {
	t.Helper()
	runs, err := e.store.ListRuns(e.ctx)
	require.NoError(t, err)
	return runs
}

// A judgment that read a window speaks only to the reply obligations whose
// evidence, the thread's newest message at the cut-off that opened them, lies
// inside the window.
func TestWindowedJudgmentSpeaksToReplyObligationsInsideItsWindow(t *testing.T) {
	// The thread holds messages 1 to 4 on days 1 to 4. A judgment at day 2 opens
	// a reply obligation, whose evidence is message 2: the user's when the
	// contact is owed a reply, else the contact's. A windowed judgment at day 4
	// then judges "nobody owes", or omits the contact.
	setup := func(t *testing.T, opening judgment.BallInCourt) (*env, uuid.UUID, thread, []string) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.message(t, th, c, false, e.day(2))
		keys := e.messageKeys(t, th)
		_, effects := e.run(t, judgment.RunKindScheduled, windowed(prodMethod, th, e.day(2), "h2", keys[0], keys[:2], judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, opening, 0.9)},
		}))
		require.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		e.message(t, th, c, false, e.day(3))
		e.message(t, th, c, false, e.day(4))
		return e, c, th, e.messageKeys(t, th)
	}
	nobodyOwes := func(c uuid.UUID) judgment.Outcomes {
		return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}
	}
	cases := []struct {
		name        string
		start       int
		wantClaims  int
		description string
	}{
		{"evidence inside the window, where today's apply rule acts", 0, 1, "a removal claim"},
		{"a window starting exactly at the evidence message counts as inside", 1, 1, "a removal claim"},
		{"evidence before the window start, where the obligation stays as it was", 2, 0, "no removal claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name+", for a judgment that says nobody owes", func(t *testing.T) {
			// spec: CAD-061.evidence-is-newest-message-at-opening, CAD-061.window-at-evidence-counts-inside, CAD-061.window-after-evidence-neither-keeps-nor-removes
			e, c, th, keys := setup(t, judgment.BallInCourtIOweReply)
			before := e.openObligations(t, th)

			_, effects := e.run(t, judgment.RunKindScheduled, windowed(prodMethod, th, e.day(4), "h4", keys[tc.start], keys[2:], nobodyOwes(c)))

			assert.Equal(t, judgment.Effects{ClaimsRaised: tc.wantClaims}, effects, tc.description)
			assert.Equal(t, before, e.openObligations(t, th), "a claim removes nothing")
		})
		t.Run(tc.name+", for a judgment that omits the contact", func(t *testing.T) {
			// spec: CAD-061.window-after-evidence-neither-keeps-nor-removes, KNW-044.omission-is-claim
			e, c, th, keys := setup(t, judgment.BallInCourtTheyOweReply)
			other := e.contact(t)
			e.message(t, th, other, false, e.day(4).Add(time.Minute))

			_, effects := e.run(t, judgment.RunKindScheduled, windowed(prodMethod, th, e.day(5), "h5", keys[tc.start], keys[2:], judgment.Outcomes{
				BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(other, judgment.BallInCourtNobodyOwes, 0.9)},
			}))

			var omissions int
			for _, o := range e.outcomes(t, th) {
				if o.QuestionID == "" && o.ContactID != nil && *o.ContactID == c {
					omissions++
				}
			}
			assert.Equal(t, tc.wantClaims, omissions, "an omission is raised only for evidence inside the window")
			assert.Equal(t, judgment.Effects{ClaimsRaised: tc.wantClaims}, effects)
		})
	}

	t.Run("an obligation whose evidence is unknown is preserved under a windowed judgment", func(t *testing.T) {
		// spec: CAD-061.unknown-evidence-preserved
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		// The obligation opens before the thread holds any message.
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h0", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)},
		}))
		require.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		e.message(t, th, c, false, e.day(2))
		keys := e.messageKeys(t, th)
		before := e.openObligations(t, th)

		_, windowedEffects := e.run(t, judgment.RunKindScheduled, windowed(prodMethod, th, e.day(2), "h2", keys[0], keys, nobodyOwes(c)))

		assert.Equal(t, judgment.Effects{}, windowedEffects, "no removal claim")
		assert.Equal(t, before, e.openObligations(t, th))
	})
}

// A judgment of a new version compiles into a proposal from its ball-in-court
// outcomes alone.
func TestBenchReadCompilesNewVersionProposals(t *testing.T) {
	t.Run("a conversation judgment with flags and ball-in-court outcomes", func(t *testing.T) {
		// spec: IXN-052.answers-not-questions
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		key := b.messageKey(t, th)
		j := windowed(prodMethod, th, b.day(1), "h1", key, []string{key}, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		})
		j.Flags = flagsOf(key, 0.9, 0.2, 0.1)
		b.run(t, judgment.RunKindScheduled, j)

		p := b.proposal(t, x, prodMethod)

		assert.Equal(t, []string{x.String()}, listContacts(p, "i_owe_reply"))
		assert.Len(t, p.Items, 1, "the flags add no item")
		assert.Equal(t, []string{"i_owe_reply", "owes_me_reply", "commitments", "big_moment"}, listNames(p))
	})

	t.Run("a meeting judgment, which holds no outcome", func(t *testing.T) {
		// spec: IXN-053.lists-shown
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		key := b.messageKey(t, th)
		j := windowed(prodMethod, th, b.day(1), "h1", key, []string{key}, judgment.Outcomes{})
		j.Kind = threadctx.KindMeeting
		j.Flags = flagsOf(key, 0.9, 0.2, 0.1)
		b.run(t, judgment.RunKindScheduled, j)

		p := b.proposal(t, x, prodMethod)

		assert.Empty(t, p.Items)
		assert.Equal(t, []string{"commitments", "big_moment"}, listNames(p))
	})
}
