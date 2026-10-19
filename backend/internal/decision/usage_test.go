package decision_test

import (
	"slices"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A judgment carries its execution time, latency, the size Measure gives, the
// billed tokens and cost its source reports (none where it reports none) and
// its question-wording version; its outcomes name the question IDs they
// answer, which name participants by role.
func TestJudge_JudgmentCarriesItsUsage(t *testing.T) {
	executedAt := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	defer accelerated.SetNowForTest(func() time.Time { return executedAt })()
	masker := threadctx.NewMasker(names)
	judge := func(t *testing.T, adapter decision.Adapter, c threadctx.Context) (judgment.Judgment, decision.Size) {
		t.Helper()
		m := decision.NewMaskedMethod(definition(roomy), masker, adapter)
		size, err := m.Measure(t.Context(), c)
		require.NoError(t, err)
		res, err := planAndJudge(t.Context(), m, c)
		require.NoError(t, err)
		j := judged(t, res)
		assert.Equal(t, executedAt, j.JudgedAt, "judged at execution time, not the cut-off")
		assert.Positive(t, j.Usage.Latency)
		assert.Equal(t, size.Tokens, j.Usage.ContextTokens, "an unscaled sizing gives the estimate itself")
		assert.Equal(t, size.Questions, j.Usage.Questions)
		assert.Equal(t, decision.DefaultWording.Version(), j.Method.WordingVersion)
		return j, size
	}

	t.Run("a hosted answer reporting cost", func(t *testing.T) {
		srv := systemOneServer(t, map[string]any{"input_tokens": 1234, "output_tokens": 0, "cost": 0.0042})
		j, _ := judge(t, decision.NewSystemOneAdapter(srv.URL, "key", "vendor-1"), dm("see you friday?"))
		require.NotNil(t, j.Usage.BilledTokens)
		assert.Equal(t, 1234, *j.Usage.BilledTokens)
		require.NotNil(t, j.Usage.CostUSD)
		assert.InDelta(t, 0.0042, *j.Usage.CostUSD, 1e-12)
	})

	t.Run("an answer reporting no cost", func(t *testing.T) {
		srv := systemOneServer(t, map[string]any{"input_tokens": 900, "output_tokens": 12})
		j, _ := judge(t, decision.NewSystemOneAdapter(srv.URL, "", "jev-1.14.0"), dm("see you friday?"))
		assert.Nil(t, j.Usage.CostUSD)
		require.NotNil(t, j.Usage.BilledTokens)
		assert.Equal(t, 900, *j.Usage.BilledTokens)
	})

	t.Run("a sage-masked answer reporting no usage", func(t *testing.T) {
		yes := "yes"
		srv := sageServer(t, &yes, nil, 0)
		j, _ := judge(t, decision.NewSageAdapter(srv.URL, "key"), dm("see you friday?"))
		assert.Nil(t, j.Usage.BilledTokens)
		assert.Nil(t, j.Usage.CostUSD)
	})

	t.Run("two definitions differing only in endpoint", func(t *testing.T) {
		a := decision.NewMaskedMethod(definition(roomy), masker, decision.NewSystemOneAdapter("https://one.example.test/v1/systemone", "", "vendor-1")).Ref()
		b := decision.NewMaskedMethod(definition(roomy), masker, decision.NewSystemOneAdapter("https://two.example.test/v1/systemone", "", "vendor-1")).Ref()
		assert.NotEqual(t, a.Version, b.Version, "the endpoint is part of the version")
		assert.Equal(t, a.WordingVersion, b.WordingVersion, "the wording did not change")

		reworded := definition(roomy)
		reworded.Wording = decision.ReasonedWording
		c := decision.NewMaskedMethod(reworded, masker, decision.NewSystemOneAdapter("https://one.example.test/v1/systemone", "", "vendor-1")).Ref()
		assert.NotEqual(t, a.WordingVersion, c.WordingVersion, "a wording change changes the wording version")
		assert.NotEqual(t, a.Version, c.Version)
	})

	t.Run("a definition changed after its method is built", func(t *testing.T) {
		def := definition(roomy)
		def.Wording.BallInCourt.Options = slices.Clone(def.Wording.BallInCourt.Options)
		f := &fake{answer: plain}
		m := newMethod(def, f)
		ref := m.Ref()
		def.Wording.BallInCourt.Options[0].Description = "reworded"
		_, err := planAndJudge(t.Context(), m, dm("see you friday?"))
		require.NoError(t, err)
		bic := f.requests[0].Questions()[0]
		require.Equal(t, "ball_in_court/THEM", bic.ID)
		assert.NotContains(t, bic.Options[0].Description, "reworded", "the method sends the wording its version derives from")
		assert.Equal(t, ref, m.Ref())
	})

	t.Run("a group thread with two matched contacts", func(t *testing.T) {
		srv := systemOneServer(t, map[string]any{"input_tokens": 10, "output_tokens": 0})
		j, _ := judge(t, decision.NewSystemOneAdapter(srv.URL, "", "vendor-1"), group("PERSON_2: who's bringing drinks?", "ME: I will"))
		got := map[string]string{}
		for _, b := range j.Outcomes.BallInCourt {
			got[b.QuestionID] = b.ContactID.String()
		}
		assert.Equal(t, map[string]string{
			"ball_in_court/PERSON_2": juniper.String(),
			"ball_in_court/PERSON_3": tobias.String(),
		}, got)
	})
}
