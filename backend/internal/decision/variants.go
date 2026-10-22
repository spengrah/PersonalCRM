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

// NewMaskedMethod builds a method that masks every request with the name
// spans of the model version before its adapter sends it: it covers each
// context with spans before it plans or measures it, and a context the name
// model cannot cover is not sent. Its version derives from the masking
// version of the model version, which no masker's dictionary changes, so a
// registry built with no masker names the same version.
func NewMaskedMethod(def Definition, model threadctx.ModelVersion, masker *threadctx.Masker, adapter Adapter) Method {
	cover := func(ctx context.Context, c threadctx.Context) (threadctx.Context, error) {
		return masker.Cover(ctx, model, c)
	}
	return newMethod(def, adapter.Route(), threadctx.MaskingVersion(model), cover, func(ctx context.Context, c threadctx.Context, questions []threadctx.Question) (prepared, error) {
		req, err := masker.Mask(ctx, c, questions)
		if err != nil {
			return prepared{}, fmt.Errorf("mask request: %w", err)
		}
		return prepared{wire: renderMasked(req), send: func(ctx context.Context) (Answers, error) { return adapter.Send(ctx, req) }}, nil
	})
}
