package judgerun

import (
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/judgment"
)

// Measurement is what a measuring run found, content-free: how many contexts
// it built for the method and how their requests, each with every message new,
// sit against the method's limits.
type Measurement struct {
	Method    judgment.MethodRef
	Days      Days
	CutOffs   int
	Contexts  int
	Tokens    Distribution
	Questions Distribution
	// TokenLimit and QuestionLimit are the method's own limits, zero when
	// no context was built.
	TokenLimit, QuestionLimit int
	// Windowed counts the contexts whose request is over the token limit, which
	// judging reads a window of; MessageOverLimit those whose newest message
	// alone does not fit, which judging reports message_over_limit; MultiCall
	// those whose questions exceed the question limit, which judging sends in
	// further calls.
	Windowed, MessageOverLimit, MultiCall int
}

// Distribution summarizes counts by order statistics, each the nearest-rank
// value; all zero when there are none.
type Distribution struct {
	Min, P50, P90, P99, Max int
}

// Measure builds every context a historical run of the days would judge,
// and sizes its whole request against the method's limits. It sends nothing
// to a vendor and writes no judgment, no run and no live state; masking the
// requests stores the name spans it asks the name model for, as any run's
// masking does. It reports every size and refuses none.
func (r *Runner) Measure(ctx context.Context, methodKey string, days Days) (Measurement, error) {
	m, err := r.methods.Get(methodKey)
	if err != nil {
		return Measurement{}, err
	}
	out := Measurement{Method: m.Ref(), Days: days, CutOffs: len(days.CutOffs())}
	var tokens, questions []int
	schedule, err := eligible(ctx, r.contexts, days)
	if err != nil {
		return Measurement{}, err
	}
	for _, e := range schedule {
		for _, thread := range e.threads {
			c, err := r.contexts.Build(ctx, thread, e.cutOff)
			if err != nil {
				return Measurement{}, fmt.Errorf("build thread %s at %s: %w", thread, e.cutOff.Format(time.RFC3339), err)
			}
			size, err := m.Measure(ctx, c)
			if err != nil {
				return Measurement{}, err
			}
			out.TokenLimit, out.QuestionLimit = size.TokenLimit, size.QuestionLimit
			tokens = append(tokens, size.Tokens)
			questions = append(questions, size.Questions)
			switch {
			case !size.NewestFits:
				out.MessageOverLimit++
			case size.Exceeds():
				out.Windowed++
			}
			if size.Calls > 1 {
				out.MultiCall++
			}
		}
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
