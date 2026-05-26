// Package phonecalls contains the Pi-side wiring for the "phone_calls"
// source — Apple Phone & FaceTime call history delivered via the Mac
// daemon's push pipeline. The package is producer-agnostic; the daemon
// itself lives outside this repo (Swift code in mac-daemon/) and submits
// call.received / call.sent events to /api/v1/ingest/events.
package phonecalls

import (
	"context"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"
)

// SourceName is the canonical envelope source for phone-call events.
// Exposed for symmetry with other source packages and used by the
// IngestService's allow-list.
const SourceName = "phone_calls"

// Provider registers the "phone_calls" source with the sync registry.
// The source is push-only: data lands via the daemon's ingest POSTs
// (call.received / call.sent), never via the scheduler. The provider
// exists so:
//
//   - the scheduler's push-strategy exclusion has a registered entry to
//     match against (instead of an implicit "any unknown source is
//     pollable" gap);
//   - the /api/v1/sync/providers endpoint reports the phone_calls source
//     in the registry list (no frontend consumes that endpoint today,
//     but it's the correct ground truth for ops queries).
//
// Sync() is a no-op. ValidateCredentials() returns nil — the Mac
// daemon authenticates on its own with the host-bearer key minted
// during the pairing flow.
type Provider struct{}

// New constructs a phone-calls provider.
func New() *Provider { return &Provider{} }

// Config implements sync.SyncProvider.
func (p *Provider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 SourceName,
		DisplayName:          "Phone & FaceTime",
		Strategy:             repository.SyncStrategyPush,
		SupportsMultiAccount: false,
		SupportsDiscovery:    false, // event sync; daemon owns CallHistoryDB reads
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
// stored on the Pi for the phone_calls source — host-bearer auth is
// the daemon's own concern.
func (p *Provider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}
