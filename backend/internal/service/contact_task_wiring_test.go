package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/todoist"

	"github.com/stretchr/testify/require"
)

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
	_, err := client.QuickAdd(context.Background(), "x", "")
	require.ErrorIs(t, err, todoist.ErrNonProdWriteRefused,
		"staging cfg must produce a write-refusing client")
	// Positive (production-writes) direction is covered by
	// todoist.TestNewClientFactory_EnvWiring to avoid issuing unmocked external HTTP.
}
