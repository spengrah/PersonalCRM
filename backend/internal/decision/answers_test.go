package decision_test

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Answers become outcomes carrying the chosen option's probability; reasoned
// options of one action sum; answers made irrelevant higher in the tree yield
// no outcome.
func TestJudge_AnswersBecomeOutcomes(t *testing.T) {
	t.Run("two reasoned yes options splitting one action", func(t *testing.T) {
		f := &fake{answer: func(q threadctx.Question) decision.Answer {
			if q.ID != "ball_in_court/THEM" {
				return plain(q)
			}
			a := decision.Answer{Probabilities: map[string]float64{}, Chosen: "nobody_owes.closed"}
			for _, o := range q.Options {
				a.Probabilities[o.Key] = 0
			}
			a.Probabilities["i_owe_reply.question"] = 0.3
			a.Probabilities["i_owe_reply.request"] = 0.3
			a.Probabilities["nobody_owes.closed"] = 0.4
			return a
		}}
		def := definition(roomy)
		def.Wording = decision.ReasonedWording
		res, err := newMethod(def, f).Judge(t.Context(), dm("can you send me the address?"))
		require.NoError(t, err)
		bic := judged(t, res).Outcomes.BallInCourt
		require.Len(t, bic, 1)
		assert.Equal(t, judgment.BallInCourtIOweReply, bic[0].Outcome)
		assert.InDelta(t, 0.6, bic[0].Confidence, 1e-9)
	})

	t.Run("a commitment date answered where the commitment is absent", func(t *testing.T) {
		f := &fake{answer: func(q threadctx.Question) decision.Answer {
			switch {
			case q.ID == "commitment_theirs/THEM":
				return pick(q, 0.8, key("none"))
			case strings.HasSuffix(q.ID, "/date"):
				return pick(q, 0.9, func(o threadctx.Option) bool { return o.Key == "2026-01-16" })
			default:
				return plain(q)
			}
		}}
		res, err := newMethod(definition(roomy), f).Judge(t.Context(), dm("maybe I'll drop by on Friday"))
		require.NoError(t, err)
		j := judged(t, res)
		theirs := commitment(t, j, judgment.CommitmentDirectionTheirs)
		assert.Equal(t, judgment.CommitmentStateNone, theirs.Outcome)
		assert.InDelta(t, 0.8, theirs.Confidence, 1e-9)
		assert.Nil(t, theirs.DueDate, "no commitment, no date")
		assert.InDelta(t, 0.9, answered(j, "commitment_theirs/THEM/date").Probabilities["2026-01-16"], 1e-9, "the date answer is kept as answered")
	})

	t.Run("an explicit answer less probable than another option", func(t *testing.T) {
		f := &fake{answer: func(q threadctx.Question) decision.Answer {
			switch q.ID {
			case "commitment_theirs/THEM":
				return decision.Answer{Probabilities: map[string]float64{"open": 0.35, "none": 0.65}, Chosen: "open"}
			case "commitment_theirs/THEM/date":
				a := pick(q, 0.3, key("2026-01-16"))
				a.Probabilities["unknown"], a.Probabilities["unlisted"] = 0.7, 0
				return a
			default:
				return plain(q)
			}
		}}
		res, err := newMethod(definition(roomy), f).Judge(t.Context(), dm("I'll send the photos on Friday"))
		require.NoError(t, err)
		j := judged(t, res)
		theirs := commitment(t, j, judgment.CommitmentDirectionTheirs)
		assert.Equal(t, judgment.CommitmentStateOpen, theirs.Outcome, "the chosen option, not the most probable")
		assert.InDelta(t, 0.35, theirs.Confidence, 1e-9)
		assert.Equal(t, day(2026, 1, 16), theirs.DueDate, "the chosen date, not the most probable option")
		assert.Equal(t, "open", answered(j, "commitment_theirs/THEM").OriginalAnswer)
	})

	t.Run("per-participant answers on a not_applicable thread", func(t *testing.T) {
		f := &fake{answer: func(q threadctx.Question) decision.Answer {
			switch {
			case q.ID == "ball_in_court/THEM":
				return pick(q, 0.7, key("not_applicable"))
			case strings.HasPrefix(q.ID, "commitment_") && strings.Count(q.ID, "/") == 1:
				return pick(q, 0.95, key("open"))
			case q.ID == "big_moment":
				return pick(q, 0.9, key("participant_moment"))
			default:
				return plain(q)
			}
		}}
		res, err := newMethod(definition(roomy), f).Judge(t.Context(), dm("Your order has shipped! We'll deliver it by Friday."))
		require.NoError(t, err)
		j := judged(t, res)
		require.Len(t, j.Outcomes.BallInCourt, 1)
		assert.Equal(t, judgment.BallInCourtNotApplicable, j.Outcomes.BallInCourt[0].Outcome)
		assert.Empty(t, j.Outcomes.Commitments, "commitments on a thread that is no conversation are ignored")
		assert.Empty(t, j.Outcomes.BigMoment.Outcome, "a big moment on a thread that is no conversation is ignored")
		assert.InDelta(t, 0.95, answered(j, "commitment_mine/THEM").Probabilities["open"], 1e-9, "ignored answers are kept as answered")
	})
}

func commitment(t *testing.T, j judgment.Judgment, d judgment.CommitmentDirection) judgment.CommitmentOutcome {
	t.Helper()
	for _, c := range j.Outcomes.Commitments {
		if c.Direction == d {
			return c
		}
	}
	t.Fatalf("no %s commitment outcome", d)
	return judgment.CommitmentOutcome{}
}
