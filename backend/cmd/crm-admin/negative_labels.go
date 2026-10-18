package main

import (
	"context"
	"fmt"
)

// negativeLabeler is the narrow interface --infer-negative-labels needs.
// Production wires it to a *judgment.Store.
type negativeLabeler interface {
	InferNegativeLabels(ctx context.Context) (int, error)
}

// runInferNegativeLabels runs the bench's labeling rule once over every
// labeled content, so a proposal holding a label before the rule existed
// holds no unlabeled negative decision, and prints how many labels it
// recorded.
func runInferNegativeLabels(ctx context.Context, deps adminDeps) error {
	n, err := deps.negativeLabels.InferNegativeLabels(ctx)
	if err != nil {
		return fmt.Errorf("infer negative labels: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "infer-negative-labels: labeled=%d\n", n); err != nil {
		return fmt.Errorf("write infer-negative-labels report: %w", err)
	}
	return nil
}
