// Package sync provides the interface and registry for external data sync providers.
package sync

import (
	"context"
	"sync"
	"time"

	"personal-crm/backend/internal/repository"
)

// SourceConfig defines configuration for a sync source
type SourceConfig struct {
	Name                 string                  `json:"name"`                   // e.g., "gmail", "imessage", "telegram"
	DisplayName          string                  `json:"display_name"`           // e.g., "Gmail", "iMessage", "Telegram"
	Strategy             repository.SyncStrategy `json:"strategy"`               // contact_driven, fetch_all, fetch_filtered
	SupportsMultiAccount bool                    `json:"supports_multi_account"` // true for Google/Telegram, false for iMessage
	SupportsDiscovery    bool                    `json:"supports_discovery"`     // true if can discover new contacts
	DefaultInterval      time.Duration           `json:"default_interval"`       // e.g., 15 * time.Minute
}

// SyncResult represents the outcome of a sync operation
type SyncResult struct {
	ItemsProcessed int            `json:"items_processed"`
	ItemsMatched   int            `json:"items_matched"`
	ItemsCreated   int            `json:"items_created"`
	NewCursor      string         `json:"new_cursor,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// SyncProvider defines the interface for external data sync providers.
// Each external data source (Gmail, iMessage, Telegram, etc.) implements this interface.
type SyncProvider interface {
	// Config returns the provider's configuration
	Config() SourceConfig

	// Sync performs the actual sync operation.
	// ctx: context for cancellation
	// state: current sync state (includes cursor, account info)
	// contacts: list of contacts to match against (for contact_driven strategy)
	// Returns the sync result and any error that occurred.
	Sync(ctx context.Context, state *repository.SyncState, contacts []repository.Contact) (*SyncResult, error)

	// ValidateCredentials checks if the provider's credentials are valid.
	// For multi-account providers, accountID specifies which account to validate.
	ValidateCredentials(ctx context.Context, accountID *string) error
}

// ProviderRegistry manages registered sync providers.
// It is thread-safe for concurrent access.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]SyncProvider
}

// NewProviderRegistry creates a new empty provider registry
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[string]SyncProvider),
	}
}

// Register adds a provider to the registry.
// The provider's Config().Name is used as the key.
// If a provider with the same name already exists, it will be replaced.
func (r *ProviderRegistry) Register(provider SyncProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	config := provider.Config()
	r.providers[config.Name] = provider
}

// Unregister removes a provider from the registry.
func (r *ProviderRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.providers, name)
}

// Get retrieves a provider by name.
// Returns the provider and true if found, nil and false otherwise.
func (r *ProviderRegistry) Get(name string) (SyncProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	provider, ok := r.providers[name]
	return provider, ok
}

// List returns a list of all registered provider configurations.
func (r *ProviderRegistry) List() []SourceConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	configs := make([]SourceConfig, 0, len(r.providers))
	for _, p := range r.providers {
		configs = append(configs, p.Config())
	}
	return configs
}

// Names returns a list of all registered provider names.
func (r *ProviderRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}

// Count returns the number of registered providers.
func (r *ProviderRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.providers)
}
