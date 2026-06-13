package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/todoist"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRoundTripper records how many HTTP requests reach it. Injecting it
// turns a "factory regressed to the prod-default client" bug into a non-zero
// call count instead of a real outbound Todoist request.
type countingRoundTripper struct {
	calls atomic.Int32
}

func (rt *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	return nil, http.ErrServerClosed // never reached when the guard refuses
}

// TestNewContactTaskService_WiresEnvAwareFactory verifies the production
// constructor wires the CRM_ENV-aware Todoist factory: a staging cfg must
// produce a client that refuses outbound writes. This catches a regression
// where NewContactTaskService kept the bare DefaultClientFactory (prod default)
// and silently wrote from a non-prod instance.
//
// nil repos/oauth are safe: the guard returns before any OAuth lookup or HTTP,
// so the test never dereferences them. Being in package service lets the test
// read the unexported todoistClientFunc field directly.
func TestNewContactTaskService_WiresEnvAwareFactory(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Runtime: config.RuntimeConfig{CRMEnvironment: "staging"},
	}

	svc := NewContactTaskService(nil, nil, nil, nil, cfg)
	require.NotNil(t, svc.todoistClientFunc, "factory should be wired")

	client := svc.todoistClientFunc("any-token")

	// Inject a recording transport so a regression that produced a writing
	// client surfaces as a non-zero call count, never a real Todoist request.
	rt := &countingRoundTripper{}
	sc, ok := client.(*todoist.SyncClient)
	require.True(t, ok, "factory should return *todoist.SyncClient")
	sc.SetHTTPClient(&http.Client{Transport: rt})

	_, err := sc.QuickAdd(context.Background(), "x", "")
	require.ErrorIs(t, err, todoist.ErrNonProdWriteRefused,
		"staging cfg must produce a write-refusing client")
	assert.Equal(t, int32(0), rt.calls.Load(), "no HTTP request should be issued")
	// Positive (production-writes) direction is covered by
	// todoist.TestNewClientFactory_EnvWiring to avoid issuing unmocked external HTTP.
}
