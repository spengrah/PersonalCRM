package decision_test

import (
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A call that fails returns call_failed, not an error that ends the run.
func TestJudge_EndpointErrorIsCallFailed(t *testing.T) {
	f := &fake{err: errEndpoint}
	res, err := planAndJudge(t.Context(), newMethod(definition(roomy), f), dm("are you around?"))
	require.NoError(t, err)
	assert.Equal(t, decision.Incomplete{Reason: judgment.IncompleteReasonCallFailed}, res)
	assert.Len(t, f.requests, 1)
}
