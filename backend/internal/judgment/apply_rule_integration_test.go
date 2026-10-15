//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The apply rule: an addition at or above the catalog's threshold takes
// effect at once, one below it is a claim with no task, and a removal is
// always a claim that leaves the obligation, the wait and the date as they
// were.
func TestApplyRule(t *testing.T) {
	t.Run("an i_owe_reply addition at exactly the threshold opens its obligation", func(t *testing.T) {
		// spec: KNW-044.addition-at-threshold-applies
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))

		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.5)}}))

		assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationDebtorUser, open[0].Debtor)
		assert.Equal(t, c, open[0].ContactID)
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("an i_owe_reply addition just below the threshold is a claim with no task", func(t *testing.T) {
		// spec: KNW-044.addition-below-threshold-claim
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))

		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.49)}}))

		assert.Equal(t, judgment.Effects{ClaimsRaised: 1}, effects)
		assert.Empty(t, e.openObligations(t, th))
		assert.Equal(t, judgment.OutcomeStateClaim, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("nobody_owes on a thread with an open i_owe_reply obligation is a claim that leaves it open", func(t *testing.T) {
		// spec: KNW-044.removal-always-claim
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)

		e.message(t, th, c, false, e.day(2))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.99)}}))

		assert.Equal(t, judgment.Effects{ClaimsRaised: 1}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		require.Len(t, views, 2)
		assert.Equal(t, judgment.OutcomeStateClaim, views[1].Outcomes[0].State)
	})

	t.Run("a commitment dated later than its open obligation is a claim that keeps the date", func(t *testing.T) {
		// spec: KNW-044.removal-always-claim
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, true, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, date(e.day(10)))}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)
		require.Equal(t, *date(e.day(10)), before[0].DueDate)

		e.message(t, th, c, false, e.day(2))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, date(e.day(20)))}}))

		assert.Equal(t, judgment.Effects{ClaimsRaised: 1}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
	})

	t.Run("a judgment that omits a party's open commitment obligation raises a claim and leaves it open", func(t *testing.T) {
		// spec: KNW-044.omission-is-claim
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, nil)}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)

		e.message(t, th, c, true, e.day(2))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))

		assert.Equal(t, judgment.Effects{ClaimsRaised: 1}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		var omission *judgment.OutcomeView
		for i, o := range views[1].Outcomes {
			if o.Question == judgment.QuestionCommitmentTheirs {
				omission = &views[1].Outcomes[i]
			}
		}
		require.NotNil(t, omission, "the omission is an outcome of the later judgment")
		assert.Equal(t, judgment.OutcomeStateClaim, omission.State)
		assert.Equal(t, string(judgment.CommitmentStateNone), omission.Outcome)
	})
}
