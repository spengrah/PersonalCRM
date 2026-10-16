//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only scheduled and historical runs of the designated production method
// change live state; every other judgment is evidence.
func TestOnlyProductionRunsChangeLiveState(t *testing.T) {
	addition := func(e *env, th thread) judgment.Outcomes {
		c := e.contact(t)
		e.message(t, th, c, false, e.day(1))
		return judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 1.0)},
			BigMoment:   judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: judgment.BigMomentParticipant, Confidence: 1.0},
		}
	}
	evidence := func(t *testing.T, e *env, th thread, effects judgment.Effects) {
		t.Helper()
		assert.Equal(t, judgment.Effects{}, effects)
		assert.Empty(t, e.openObligations(t, th))
		outcomes := e.outcomes(t, th)
		require.Len(t, outcomes, 2)
		for _, o := range outcomes {
			assert.Equal(t, judgment.OutcomeStateEvidence, o.State, o.Question)
		}
	}

	t.Run("a non-production method's addition at full confidence is evidence", func(t *testing.T) {
		// spec: KNW-045.others-are-evidence
		e := newEnv(t, production)
		th := e.thread(t)
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(otherMethod, th, e.day(1), "h1", addition(e, th)))
		evidence(t, e, th, effects)
	})

	t.Run("a rerun of the production method is evidence", func(t *testing.T) {
		// spec: KNW-045.others-are-evidence
		e := newEnv(t, production)
		th := e.thread(t)
		_, effects := e.run(t, judgment.RunKindRerun, judgmentOf(prodMethod, th, e.day(1), "h1", addition(e, th)))
		evidence(t, e, th, effects)
	})

	t.Run("with no method designated every judgment is evidence", func(t *testing.T) {
		// spec: KNW-045.others-are-evidence
		e := newEnv(t, "")
		th := e.thread(t)
		_, ok := e.store.ProductionMethod()
		require.False(t, ok)
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", addition(e, th)))
		evidence(t, e, th, effects)
	})

	t.Run("a historical run of the production method applies", func(t *testing.T) {
		// spec: KNW-045.effect-decided-by-appearing-run
		e := newEnv(t, production)
		th := e.thread(t)
		_, effects := e.run(t, judgment.RunKindHistorical, judgmentOf(prodMethod, th, e.day(1), "h1", addition(e, th)))
		assert.Equal(t, judgment.Effects{ObligationsOpened: 1, FlagsRaised: 1}, effects)
		assert.Len(t, e.openObligations(t, th), 1)
		for _, o := range e.outcomes(t, th) {
			assert.Equal(t, judgment.OutcomeStateApplied, o.State, o.Question)
		}
	})
}

// Only a current judgment, one whose cut-off is at or after the thread's
// latest content, changes live state.
func TestOnlyCurrentJudgmentsChangeLiveState(t *testing.T) {
	t.Run("a January judgment on a thread the user answered in February is evidence", func(t *testing.T) {
		// spec: KNW-045.not-current-is-evidence
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		january, february := e.day(1), e.day(32)
		e.message(t, th, c, false, january)
		e.message(t, th, c, true, february)

		_, effects := e.run(t, judgment.RunKindHistorical, judgmentOf(prodMethod, th, january, "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))

		assert.Equal(t, judgment.Effects{}, effects)
		assert.Empty(t, e.openObligations(t, th))
		assert.Equal(t, judgment.OutcomeStateEvidence, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("a January judgment on a thread quiet since January opens its obligation", func(t *testing.T) {
		// spec: KNW-045.not-current-is-evidence
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		january := e.day(1)
		e.message(t, th, c, false, january)

		_, effects := e.run(t, judgment.RunKindHistorical, judgmentOf(prodMethod, th, january, "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))

		assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		assert.Len(t, e.openObligations(t, th), 1)
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})
}
