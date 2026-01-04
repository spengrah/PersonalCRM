package unit

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	psync "personal-crm/backend/internal/sync"

	"github.com/stretchr/testify/assert"
)

func TestBackoffCalculation(t *testing.T) {
	backoffIntervals := []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		30 * time.Minute,
		1 * time.Hour,
	}

	tests := []struct {
		name            string
		errorCount      int
		expectedBackoff time.Duration
	}{
		{"first error", 0, 1 * time.Minute},
		{"second error", 1, 5 * time.Minute},
		{"third error", 2, 30 * time.Minute},
		{"fourth error", 3, 1 * time.Hour},
		{"max backoff", 10, 1 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := tt.errorCount
			if idx >= len(backoffIntervals) {
				idx = len(backoffIntervals) - 1
			}

			assert.Equal(t, tt.expectedBackoff, backoffIntervals[idx])
		})
	}
}

func TestProviderRegistry(t *testing.T) {
	t.Run("register and get provider", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		provider := &mockSyncProvider{
			config: psync.SourceConfig{
				Name:                 "test",
				DisplayName:          "Test Provider",
				Strategy:             repository.SyncStrategyContactDriven,
				SupportsMultiAccount: false,
				SupportsDiscovery:    false,
				DefaultInterval:      15 * time.Minute,
			},
		}

		registry.Register(provider)

		retrieved, ok := registry.Get("test")
		assert.True(t, ok)
		assert.NotNil(t, retrieved)
		assert.Equal(t, "test", retrieved.Config().Name)
	})

	t.Run("get nonexistent provider", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		_, ok := registry.Get("nonexistent")
		assert.False(t, ok)
	})

	t.Run("list providers", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		provider1 := &mockSyncProvider{
			config: psync.SourceConfig{
				Name:        "gmail",
				DisplayName: "Gmail",
			},
		}
		provider2 := &mockSyncProvider{
			config: psync.SourceConfig{
				Name:        "imessage",
				DisplayName: "iMessage",
			},
		}

		registry.Register(provider1)
		registry.Register(provider2)

		configs := registry.List()
		assert.Len(t, configs, 2)
	})

	t.Run("count providers", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		assert.Equal(t, 0, registry.Count())

		provider := &mockSyncProvider{
			config: psync.SourceConfig{Name: "test"},
		}
		registry.Register(provider)

		assert.Equal(t, 1, registry.Count())
	})

	t.Run("unregister provider", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		provider := &mockSyncProvider{
			config: psync.SourceConfig{Name: "test"},
		}
		registry.Register(provider)
		assert.Equal(t, 1, registry.Count())

		registry.Unregister("test")
		assert.Equal(t, 0, registry.Count())
	})

	t.Run("names returns all registered provider names", func(t *testing.T) {
		registry := psync.NewProviderRegistry()

		provider1 := &mockSyncProvider{config: psync.SourceConfig{Name: "gmail"}}
		provider2 := &mockSyncProvider{config: psync.SourceConfig{Name: "imessage"}}

		registry.Register(provider1)
		registry.Register(provider2)

		names := registry.Names()
		assert.Len(t, names, 2)
		assert.Contains(t, names, "gmail")
		assert.Contains(t, names, "imessage")
	})
}

func TestSyncStatus(t *testing.T) {
	t.Run("sync status constants", func(t *testing.T) {
		assert.Equal(t, repository.SyncStatus("idle"), repository.SyncStatusIdle)
		assert.Equal(t, repository.SyncStatus("syncing"), repository.SyncStatusSyncing)
		assert.Equal(t, repository.SyncStatus("error"), repository.SyncStatusError)
		assert.Equal(t, repository.SyncStatus("disabled"), repository.SyncStatusDisabled)
	})
}

func TestSyncStrategy(t *testing.T) {
	t.Run("sync strategy constants", func(t *testing.T) {
		assert.Equal(t, repository.SyncStrategy("contact_driven"), repository.SyncStrategyContactDriven)
		assert.Equal(t, repository.SyncStrategy("fetch_all"), repository.SyncStrategyFetchAll)
		assert.Equal(t, repository.SyncStrategy("fetch_filtered"), repository.SyncStrategyFetchFiltered)
	})
}

func TestSourceConfig(t *testing.T) {
	t.Run("source config fields", func(t *testing.T) {
		config := psync.SourceConfig{
			Name:                 "gmail",
			DisplayName:          "Gmail",
			Strategy:             repository.SyncStrategyContactDriven,
			SupportsMultiAccount: true,
			SupportsDiscovery:    false,
			DefaultInterval:      15 * time.Minute,
		}

		assert.Equal(t, "gmail", config.Name)
		assert.Equal(t, "Gmail", config.DisplayName)
		assert.Equal(t, repository.SyncStrategyContactDriven, config.Strategy)
		assert.True(t, config.SupportsMultiAccount)
		assert.False(t, config.SupportsDiscovery)
		assert.Equal(t, 15*time.Minute, config.DefaultInterval)
	})
}

func TestSyncResult(t *testing.T) {
	t.Run("sync result fields", func(t *testing.T) {
		result := psync.SyncResult{
			ItemsProcessed: 100,
			ItemsMatched:   50,
			ItemsCreated:   10,
			NewCursor:      "cursor123",
			Metadata:       map[string]any{"key": "value"},
		}

		assert.Equal(t, 100, result.ItemsProcessed)
		assert.Equal(t, 50, result.ItemsMatched)
		assert.Equal(t, 10, result.ItemsCreated)
		assert.Equal(t, "cursor123", result.NewCursor)
		assert.Equal(t, "value", result.Metadata["key"])
	})
}

// mockSyncProvider is a mock implementation of sync.SyncProvider for testing
type mockSyncProvider struct {
	config psync.SourceConfig
}

func (m *mockSyncProvider) Config() psync.SourceConfig {
	return m.config
}

func (m *mockSyncProvider) Sync(ctx context.Context, state *repository.SyncState, contacts []repository.Contact) (*psync.SyncResult, error) {
	return &psync.SyncResult{
		ItemsProcessed: 10,
		ItemsMatched:   5,
		ItemsCreated:   1,
	}, nil
}

func (m *mockSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	return nil
}
