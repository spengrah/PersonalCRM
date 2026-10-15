//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user reviews any outcome by stating its right outcome; the verdict is
// derived from it, and reviewing an applied or claim outcome gives the thread
// the right outcome's live effect while reviewing evidence records the label
// only.
func TestReviewOutcome(t *testing.T) {
	t.Run("confirming an addition claim opens its obligation and task", func(t *testing.T) {
		// spec: KNW-046.verdict-derived, KNW-046.live-review-gives-right-effect
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.3)}}))
		claim := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		require.Equal(t, judgment.OutcomeStateClaim, claim.State)
		require.Empty(t, e.openObligations(t, th))

		reviewed, err := e.store.ReviewOutcome(e.ctx, claim.ID, string(judgment.BallInCourtIOweReply))

		require.NoError(t, err)
		require.NotNil(t, reviewed.Review)
		assert.Equal(t, judgment.VerdictConfirmed, reviewed.Review.Verdict)
		assert.Equal(t, string(judgment.BallInCourtIOweReply), reviewed.Review.RightOutcome)
		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationDebtorUser, open[0].Debtor)
	})

	t.Run("an auto-applied i_owe_reply reviewed as nobody_owes is rejected and its obligation closes", func(t *testing.T) {
		// spec: KNW-046.verdict-derived, KNW-046.live-review-gives-right-effect
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		applied := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		require.Equal(t, judgment.OutcomeStateApplied, applied.State)
		require.Len(t, e.openObligations(t, th), 1)

		reviewed, err := e.store.ReviewOutcome(e.ctx, applied.ID, string(judgment.BallInCourtNobodyOwes))

		require.NoError(t, err)
		require.NotNil(t, reviewed.Review)
		assert.Equal(t, judgment.VerdictRejected, reviewed.Review.Verdict)
		assert.Empty(t, e.openObligations(t, th))
		all, err := e.store.Obligations(e.ctx, th.id)
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.False(t, all[0].Open, "the obligation's task is closed")
	})

	t.Run("a nobody_owes claim reviewed as they_owe_reply is corrected", func(t *testing.T) {
		// spec: KNW-046.verdict-derived, KNW-046.live-review-gives-right-effect
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		e.message(t, th, c, true, e.day(2))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))
		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		claim := views[1].Outcomes[0]
		require.Equal(t, judgment.OutcomeStateClaim, claim.State)

		reviewed, err := e.store.ReviewOutcome(e.ctx, claim.ID, string(judgment.BallInCourtTheyOweReply))

		require.NoError(t, err)
		require.NotNil(t, reviewed.Review)
		assert.Equal(t, judgment.VerdictCorrected, reviewed.Review.Verdict)
		open := e.openObligations(t, th)
		require.Len(t, open, 1, "the user's reply obligation closed and the contact's opened")
		assert.Equal(t, repository.ObligationDebtorContact, open[0].Debtor)
		assert.Equal(t, c, open[0].ContactID)
	})

	t.Run("an empty right outcome, or one not among the question's values, is refused and nothing is recorded", func(t *testing.T) {
		// spec: KNW-046.invalid-right-outcome-refused
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.3)}}))
		claim := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)

		for _, right := range []string{"", string(judgment.CommitmentStateOpen)} {
			_, err := e.store.ReviewOutcome(e.ctx, claim.ID, right)
			require.ErrorIs(t, err, judgment.ErrInvalidRightOutcome, "right outcome %q", right)
		}

		after := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		assert.Nil(t, after.Review)
		assert.Equal(t, judgment.OutcomeStateClaim, after.State)
		assert.Empty(t, e.openObligations(t, th))
	})

	t.Run("an evidence outcome reviewed as i_owe_reply is corrected and nothing opens", func(t *testing.T) {
		// spec: KNW-046.evidence-review-labels-only
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(otherMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))
		evidence := e.outcome(t, th, otherMethod, judgment.QuestionBallInCourt, &c)
		require.Equal(t, judgment.OutcomeStateEvidence, evidence.State)

		reviewed, err := e.store.ReviewOutcome(e.ctx, evidence.ID, string(judgment.BallInCourtIOweReply))

		require.NoError(t, err)
		require.NotNil(t, reviewed.Review)
		assert.Equal(t, judgment.VerdictCorrected, reviewed.Review.Verdict)
		assert.Equal(t, judgment.OutcomeStateEvidence, reviewed.State)
		assert.Empty(t, e.openObligations(t, th))
	})

	t.Run("an unknown outcome is not found", func(t *testing.T) {
		e := newEnv(t, production)
		_, err := e.store.ReviewOutcome(e.ctx, e.contact(t), string(judgment.BallInCourtIOweReply))
		require.ErrorIs(t, err, judgment.ErrNotFound)
	})
}

// A later judgment of a reviewed input never re-proposes an answer the user
// rejected and never overwrites the user's correction; changed content
// reopens the question.
func TestUserVerdictWins(t *testing.T) {
	t.Run("the same method on the same input after a rejection re-proposes nothing", func(t *testing.T) {
		// spec: KNW-047.rejected-answer-not-reproposed
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		j := judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}})
		e.run(t, judgment.RunKindScheduled, j)
		applied := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		_, err := e.store.ReviewOutcome(e.ctx, applied.ID, string(judgment.BallInCourtNobodyOwes))
		require.NoError(t, err)

		_, effects := e.run(t, judgment.RunKindScheduled, j)

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Empty(t, e.openObligations(t, th))
		again := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		require.NotNil(t, again.Review)
		assert.Equal(t, judgment.VerdictRejected, again.Review.Verdict)
	})

	t.Run("another method on the same input after a correction leaves the correction in effect", func(t *testing.T) {
		// spec: KNW-047.correction-not-overwritten
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.9)}}))
		judged := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		corrected, err := e.store.ReviewOutcome(e.ctx, judged.ID, string(judgment.BallInCourtIOweReply))
		require.NoError(t, err)
		require.Equal(t, judgment.VerdictCorrected, corrected.Review.Verdict)
		before := e.openObligations(t, th)
		require.Len(t, before, 1)

		nextVersion := judgment.MethodRef{Key: production, Version: "v2", WordingVersion: "w1"}
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(nextVersion, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.99)}}))

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Equal(t, before, e.openObligations(t, th))
		assert.Equal(t, judgment.OutcomeStateEvidence, e.outcome(t, th, nextVersion, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("new content after a rejection reopens the question", func(t *testing.T) {
		// spec: KNW-047.changed-content-reopens
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		applied := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		_, err := e.store.ReviewOutcome(e.ctx, applied.ID, string(judgment.BallInCourtNobodyOwes))
		require.NoError(t, err)

		e.message(t, th, c, false, e.day(2))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))

		assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		assert.Len(t, e.openObligations(t, th), 1)
	})
}
