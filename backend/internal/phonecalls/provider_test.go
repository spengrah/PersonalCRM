package phonecalls

import (
	"context"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/stretchr/testify/require"
)

// Compile-time guard: Provider must satisfy sync.SyncProvider.
var _ sync.SyncProvider = (*Provider)(nil)

func TestProvider_Config(t *testing.T) {
	cfg := New().Config()
	require.Equal(t, "phone_calls", cfg.Name)
	require.Equal(t, "Phone & FaceTime", cfg.DisplayName)
	require.Equal(t, repository.SyncStrategyPush, cfg.Strategy)
	require.False(t, cfg.SupportsMultiAccount)
	require.False(t, cfg.SupportsDiscovery)
	require.Equal(t, int64(0), int64(cfg.DefaultInterval))
}

func TestProvider_SyncIsNoop(t *testing.T) {
	result, err := New().Sync(context.Background(), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, result.ItemsProcessed)
	require.Equal(t, 0, result.ItemsMatched)
	require.Equal(t, 0, result.ItemsCreated)
}

func TestProvider_ValidateCredentialsIsNoop(t *testing.T) {
	require.NoError(t, New().ValidateCredentials(context.Background(), nil))
	acct := "ignored"
	require.NoError(t, New().ValidateCredentials(context.Background(), &acct))
}
