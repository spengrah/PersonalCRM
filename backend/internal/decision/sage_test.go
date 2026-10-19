package decision_test

import (
	"net/http"
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sageMethod(url string) decision.Method {
	return decision.NewMaskedMethod(definition(roomy), threadctx.NewMasker(names), decision.NewSageAdapter(url, "key"))
}

// Every option of the ball-in-court question carries a probability in a
// sage-masked judgment.
func TestSage_EveryOptionHasAProbability(t *testing.T) {
	yes := "yes"
	t.Run("a four-option ball-in-court question", func(t *testing.T) {
		res, err := planAndJudge(t.Context(), sageMethod(sageServer(t, &yes, nil, 0).URL), dm("are you free saturday?"))
		require.NoError(t, err)
		j := judged(t, res)
		q := answered(j, "ball_in_court/THEM")
		require.Len(t, q.Probabilities, 4)
		assert.InDelta(t, 0.6, q.Probabilities["i_owe_reply"], 1e-9)
		for _, other := range []string{"they_owe_reply", "nobody_owes", "not_applicable"} {
			assert.InDelta(t, 0.4/3, q.Probabilities[other], 1e-9, other)
		}
		assert.Equal(t, "i_owe_reply", q.OriginalAnswer)
		for _, aq := range j.Questions {
			assert.NotEmpty(t, aq.Probabilities, "%s is answered", aq.ID)
		}
	})
}

// When Sage refuses a call, as when the plan's usage runs out, sage-masked
// reports the context incomplete as call_failed instead of ending the run.
func TestSage_RefusedCallIsCallFailed(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			res, err := planAndJudge(t.Context(), sageMethod(sageServer(t, nil, nil, status).URL), dm("are you free saturday?"))
			require.NoError(t, err)
			assert.Equal(t, decision.Incomplete{Reason: judgment.IncompleteReasonCallFailed}, res)
		})
	}
}
