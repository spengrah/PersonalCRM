// Package messages contains the Pi-side wiring for the "messages"
// source — Apple Messages.app data delivered via the Mac daemon's push
// pipeline. The package is producer-agnostic; the daemon itself lives
// outside this repo (Swift code in a sibling project) and submits
// raw_message.* events to /api/v1/ingest/events.
package messages

import (
	"context"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"
)

// Provider registers the "messages" source with the sync registry. The
// source is push-only: data lands via the daemon's ingest POSTs, never
// via the scheduler. The provider exists so:
//
//   - the scheduler's push-strategy exclusion has a registered entry to
//     match against (instead of an implicit "any unknown source is
//     pollable" gap);
//   - the /api/v1/sync/providers endpoint reports the Messages source
//     in the registry list (no frontend consumes that endpoint today,
//     but it's the correct ground truth for ops queries).
//
// Sync() is a no-op. ValidateCredentials() returns nil — the Mac
// daemon authenticates on its own with the host-bearer key minted
// during the pairing flow.
type Provider struct{}

// New constructs a Messages provider.
func New() *Provider { return &Provider{} }

// Config implements sync.SyncProvider.
func (p *Provider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 "messages",
		DisplayName:          "Messages",
		Strategy:             repository.SyncStrategyPush,
		SupportsMultiAccount: false,
		SupportsDiscovery:    false, // spec §2: messages is NOT a discovery surface
		DefaultInterval:      0,     // push: scheduler never enqueues
	}
}

// Sync implements sync.SyncProvider. Push-strategy: data lands via
// /api/v1/ingest/events. The scheduler already excludes us via
// ListDueAccounts; the no-op return makes a direct invocation safe.
func (p *Provider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*sync.SyncResult, error) {
	return &sync.SyncResult{}, nil
}

// ValidateCredentials implements sync.SyncProvider. No credentials are
// stored on the Pi for the messages source — host-bearer auth is the
// daemon's own concern.
func (p *Provider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}
