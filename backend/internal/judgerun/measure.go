package judgerun

import (
	"context"
	"slices"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"
)

// Measurement is what a measuring run found, content-free: how many contexts
// it built for the method and how their sizes sit against the method's limits.
type Measurement struct {
	Method    judgment.MethodRef
	Month     Month
	CutOffs   int
	Contexts  int
	Tokens    Distribution
	Questions Distribution
	// TokenLimit and QuestionLimit are the method's own limits, zero when
	// no context was built.
	TokenLimit, QuestionLimit int
	// OverTokenLimit and OverQuestionLimit count the contexts over each limit;
	// OverLimit counts the contexts over either, which judging would report
	// over_limit.
	OverTokenLimit, OverQuestionLimit, OverLimit int
}

// Distribution summarizes counts by order statistics, each the nearest-rank
// value; all zero when there are none.
type Distribution struct {
	Min, P50, P90, P99, Max int
}

// Measure builds every context a historical run of the month would judge,
// with the method's question tree, and sizes each against the method's limits.
// It sends nothing and writes nothing: no judgment, no run and no live state.
// It reports every size and refuses none.
func (r *Runner) Measure(ctx context.Context, methodKey string, month Month) (Measurement, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return Measurement{}, err
	}
	out := Measurement{Method: m.Ref(), Month: month, CutOffs: len(month.CutOffs())}
	var tokens, questions []int
	err = eachEligible(ctx, r.contexts, month, func(c threadctx.Context) error {
		size, err := m.Measure(ctx, c)
		if err != nil {
			return err
		}
		out.TokenLimit, out.QuestionLimit = size.TokenLimit, size.QuestionLimit
		tokens = append(tokens, size.Tokens)
		questions = append(questions, size.Questions)
		if size.Tokens > size.TokenLimit {
			out.OverTokenLimit++
		}
		if size.Questions > size.QuestionLimit {
			out.OverQuestionLimit++
		}
		if size.Exceeds() {
			out.OverLimit++
		}
		return nil
	})
	if err != nil {
		return Measurement{}, err
	}
	out.Contexts = len(tokens)
	out.Tokens, out.Questions = distribution(tokens), distribution(questions)
	return out, nil
}

func distribution(values []int) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := func(p int) int {
		i := (p*len(sorted)+99)/100 - 1
		return sorted[max(i, 0)]
	}
	return Distribution{Min: sorted[0], P50: rank(50), P90: rank(90), P99: rank(99), Max: sorted[len(sorted)-1]}
}
