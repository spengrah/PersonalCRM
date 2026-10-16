package decision

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/threadctx"
)

// Adapter sends a masked request to a vendor's route. It accepts nothing but
// the masker's MaskedRequest, so no unmasked content reaches a route through
// it, and keeps no request content.
type Adapter interface {
	Route() Route
	Send(ctx context.Context, req threadctx.MaskedRequest) (Answers, error)
}

// NewMaskedMethod builds a method that masks every request before its adapter
// sends it.
func NewMaskedMethod(def Definition, masker *threadctx.Masker, adapter Adapter) Method {
	return newMethod(def, adapter.Route(), func(ctx context.Context, c threadctx.Context, questions []threadctx.Question) (prepared, error) {
		req, err := masker.Mask(ctx, c, questions)
		if err != nil {
			return prepared{}, fmt.Errorf("mask request: %w", err)
		}
		return prepared{wire: renderMasked(req), send: func(ctx context.Context) (Answers, error) { return adapter.Send(ctx, req) }}, nil
	})
}
