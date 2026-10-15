//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A big-moment outcome is an addition that becomes a flag on the thread under
// the apply rule; a participant's moment and another person's read apart.
// spec: KNW-044.big-moment-flags-distinguishable, KNW-044.addition-below-threshold-claim
func TestBigMomentFlags(t *testing.T) {
	cases := []struct {
		name       string
		moment     judgment.BigMoment
		confidence float64
		state      judgment.OutcomeState
		effects    judgment.Effects
	}{
		{"a participant's new job raises a participant_moment flag", judgment.BigMomentParticipant, 0.8, judgment.OutcomeStateApplied, judgment.Effects{FlagsRaised: 1}},
		{"a sibling's wedding mentioned in passing raises an other_person_moment flag", judgment.BigMomentOtherPerson, 0.8, judgment.OutcomeStateApplied, judgment.Effects{FlagsRaised: 1}},
		{"a participant_moment below the threshold is a claim and raises no flag", judgment.BigMomentParticipant, 0.4, judgment.OutcomeStateClaim, judgment.Effects{ClaimsRaised: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, production)
			c, th := e.contact(t), e.thread(t)
			e.message(t, th, c, false, e.day(1))

			_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", judgment.Outcomes{
				BigMoment: judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: tc.moment, Confidence: tc.confidence},
			}))

			assert.Equal(t, tc.effects, effects)
			o := e.outcome(t, th, prodMethod, judgment.QuestionBigMoment, nil)
			assert.Equal(t, string(tc.moment), o.Outcome)
			assert.Equal(t, tc.state, o.State)
		})
	}
}

// Each outcome reads back in exactly one state; an outcome that removes
// nothing is applied at any confidence.
func TestOutcomeStates(t *testing.T) {
	t.Run("nobody_owes on a thread with no open obligation and no wait it started is applied", func(t *testing.T) {
		// spec: KNW-044.no-effect-applied
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, true, e.day(1))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))
		assert.Equal(t, judgment.Effects{}, effects)
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("nobody_owes at confidence 0.3 that removes nothing is applied", func(t *testing.T) {
		// spec: KNW-044.no-effect-applied
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, true, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.3)}}))
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("nobody_owes from one participant while another keeps the user's reply obligation is applied", func(t *testing.T) {
		// spec: KNW-044.no-effect-applied
		e := newEnv(t, production)
		owed, other, th := e.contact(t), e.contact(t), e.thread(t)
		e.message(t, th, owed, false, e.day(1))
		e.message(t, th, other, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(owed, judgment.BallInCourtIOweReply, 0.9)}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)

		e.message(t, th, other, false, e.day(2))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
				bic(owed, judgment.BallInCourtIOweReply, 0.9), bic(other, judgment.BallInCourtNobodyOwes, 0.9),
			}}))

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		require.Len(t, views, 2)
		for _, o := range views[1].Outcomes {
			assert.Equal(t, judgment.OutcomeStateApplied, o.State, o.Outcome)
		}
	})

	t.Run("commitment none for a party with no open commitment obligation is applied", func(t *testing.T) {
		// spec: KNW-044.no-effect-applied
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateNone, 0.2, nil)}}))
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionCommitmentTheirs, &c).State)
	})

	t.Run("nobody_owes on a thread with an open i_owe_reply obligation is a claim", func(t *testing.T) {
		// spec: KNW-044.removal-always-claim
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		e.message(t, th, c, false, e.day(2))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))
		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		require.Len(t, views, 2)
		assert.Equal(t, judgment.OutcomeStateApplied, views[0].Outcomes[0].State)
		assert.Equal(t, judgment.OutcomeStateClaim, views[1].Outcomes[0].State)
	})
}

// Once a commitment obligation closes through its task, a later current
// judgment of changed content that still finds the commitment opens a new
// obligation with a new task, and the closed one stays closed.
// spec: CAD-057.open-commitment-reappears
func TestCommitmentReappearsAfterItsTaskCloses(t *testing.T) {
	for _, closing := range []repository.ContactTaskState{repository.ContactTaskStateCompleted, repository.ContactTaskStateDismissed} {
		t.Run("the obligation's task "+string(closing), func(t *testing.T) {
			e := newEnv(t, production)
			c, th := e.contact(t), e.thread(t)
			e.message(t, th, c, true, e.day(1))
			e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
				judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)}}))
			first := e.openObligations(t, th)
			require.Len(t, first, 1)
			_, err := e.tasks.UpdateContactTaskState(e.ctx, first[0].TaskID, closing)
			require.NoError(t, err)

			e.message(t, th, c, false, e.day(2))
			_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
				judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)}}))

			assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
			all, err := e.store.Obligations(e.ctx, th.id)
			require.NoError(t, err)
			require.Len(t, all, 2)
			assert.Equal(t, first[0].TaskID, all[0].TaskID)
			assert.False(t, all[0].Open, "the closed obligation stays closed")
			assert.True(t, all[1].Open)
			assert.NotEqual(t, all[0].TaskID, all[1].TaskID)
		})
	}
}

// A method version holds at most one judgment per thread and input hash; a
// run that reuses it records it at its own cut-off and receives the live
// effects a fresh judgment in that run would, never applying an effect twice.
// spec: KNW-043.one-per-method-version-and-input, KNW-045.effect-decided-by-appearing-run
func TestJudgmentReuse(t *testing.T) {
	iOweReply := func(e *env, th thread) judgment.Judgment {
		c := e.contact(t)
		e.message(t, th, c, false, e.day(1))
		return judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}})
	}
	reuse := func(t *testing.T, e *env, kind judgment.RunKind, th thread) (judgment.RunID, judgment.Effects) {
		t.Helper()
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: kind, Method: prodMethod})
		require.NoError(t, err)
		effects, found, err := e.store.ReuseJudgment(e.ctx, run, th.id, e.day(3), "h1")
		require.NoError(t, err)
		require.True(t, found)
		return run, effects
	}

	t.Run("a scheduled production run reusing a judgment first made by a rerun applies it", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		_, effects := e.run(t, judgment.RunKindRerun, iOweReply(e, th))
		require.Equal(t, judgment.Effects{}, effects)

		run, effects := reuse(t, e, judgment.RunKindScheduled, th)

		assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		assert.Len(t, e.openObligations(t, th), 1)
		threads, err := e.store.ThreadsJudgedInRun(e.ctx, run)
		require.NoError(t, err)
		require.Len(t, threads, 1)
		assert.Equal(t, th.id, threads[0].Thread)
		assert.True(t, e.day(3).Equal(threads[0].CutOff), "the run's cut-off")
	})

	t.Run("a rerun reusing a production judgment changes nothing", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		e.run(t, judgment.RunKindScheduled, iOweReply(e, th))
		before := e.openObligations(t, th)

		_, effects := reuse(t, e, judgment.RunKindRerun, th)

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
	})

	t.Run("a second scheduled run reusing a judgment whose obligation is open opens no second one", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		e.run(t, judgment.RunKindScheduled, iOweReply(e, th))

		_, effects := reuse(t, e, judgment.RunKindScheduled, th)

		assert.Equal(t, judgment.Effects{}, effects)
		all, err := e.store.Obligations(e.ctx, th.id)
		require.NoError(t, err)
		assert.Len(t, all, 1)
	})

	t.Run("a scheduled run reusing a judgment whose obligation the user closed through its task reopens nothing", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		e.run(t, judgment.RunKindScheduled, iOweReply(e, th))
		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		_, err := e.tasks.UpdateContactTaskState(e.ctx, open[0].TaskID, repository.ContactTaskStateCompleted)
		require.NoError(t, err)

		_, effects := reuse(t, e, judgment.RunKindScheduled, th)

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Empty(t, e.openObligations(t, th))
	})

	t.Run("an equal input hash on another thread finds nothing", func(t *testing.T) {
		e := newEnv(t, production)
		th, other := e.thread(t), e.thread(t)
		e.run(t, judgment.RunKindScheduled, iOweReply(e, th))
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: prodMethod})
		require.NoError(t, err)

		effects, found, err := e.store.ReuseJudgment(e.ctx, run, other.id, e.day(3), "h1")

		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, judgment.Effects{}, effects)
		threads, err := e.store.ThreadsJudgedInRun(e.ctx, run)
		require.NoError(t, err)
		assert.Empty(t, threads)
	})
}
