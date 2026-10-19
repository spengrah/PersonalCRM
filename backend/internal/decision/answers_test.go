package decision_test

import (
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ball-in-court answers become outcomes carrying the chosen option's
// probability; reasoned options of one action sum.
func TestJudge_BallInCourtAnswersBecomeOutcomes(t *testing.T) {
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
	res, err := planAndJudge(t.Context(), newMethod(def, f), dm("can you send me the address?"))
	require.NoError(t, err)
	bic := judged(t, res).Outcomes.BallInCourt
	require.Len(t, bic, 1)
	assert.Equal(t, judgment.BallInCourtIOweReply, bic[0].Outcome)
	assert.InDelta(t, 0.6, bic[0].Confidence, 1e-9)
}
