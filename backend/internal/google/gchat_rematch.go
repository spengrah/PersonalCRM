package google

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
)

// GChatSyncStateLister is the narrow seam the GChat rematch handlers use to find
// the accounts whose GChat sync is enabled. Production satisfies it with
// *repository.SyncRepository; tests satisfy it with a fake lister. Mirrors the
// emailSyncStateLister convention so main.go can pass the block-scoped syncRepo
// through a hoisted interface var.
type GChatSyncStateLister interface {
	ListEnabledSyncStates(ctx context.Context) ([]repository.SyncState, error)
}

// gchatRematchBase holds the shared dependencies + the inert-by-construction
// gate that both GChat rematch handlers run. On a contact_methods.added event it
// runs a one-shot identifier-scoped historical GChat scan for the newly-added
// address across the connected accounts whose GChat sync is ENABLED, upserting
// comms_message(source='gchat') rows (NO events — the aggregation engine derives
// interactions on its own pass). It is provably a no-op until an enabled gchat
// sync state exists, because it gates FIRST on ListEnabledSyncStates filtered to
// source='gchat' and returns (0, nil) when that set is empty.
type gchatRematchBase struct {
	provider  *GChatSyncProvider
	states    GChatSyncStateLister
	commsRepo *repository.CommsMessageRepository
	engine    *aggregation.Engine
}

// rematch runs the gate-then-scan-then-aggregate flow for one normalized
// address. It is shared by both handlers (which differ only in IdentifierType).
func (b *gchatRematchBase) rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	addr := matching.NormalizeEmail(valueNormalized)
	if addr == "" {
		return 0, nil
	}

	// Gate on the enabled GChat sync states FIRST so the no-op path does zero
	// work (no identity-map build, no me-set, no fetcher). This is what makes the
	// handlers inert until a gchat sync state is enabled.
	states, err := b.states.ListEnabledSyncStates(ctx)
	if err != nil {
		return 0, err
	}
	var gchatStates []repository.SyncState
	for _, st := range states {
		if st.Source == GChatSourceName && st.AccountID != nil && strings.TrimSpace(*st.AccountID) != "" {
			gchatStates = append(gchatStates, st)
		}
	}
	if len(gchatStates) == 0 {
		return 0, nil
	}

	// Build the FULL dual-source known-contact map once (so co-member/direction
	// resolution matches the steady-state sweep — the just-added pair is already
	// committed and included), and the me-set once through the provider seam.
	knownMap, err := buildKnownMapFromIdentities(ctx, b.commsRepo)
	if err != nil {
		return 0, err
	}
	meSet, err := b.provider.MeSet(ctx)
	if err != nil {
		return 0, err
	}

	matched := 0
	var scanErrs []error
	for _, st := range gchatStates {
		afterCursor := gchatBackfillFloor(st.Metadata)
		n, scanErr := b.provider.ScanIdentifier(ctx, *st.AccountID, addr, knownMap, meSet, afterCursor)
		matched += n
		if scanErr != nil {
			// The account id is the operator's OWN connected account, kept raw for
			// triage; the rematched contact address is third-party PII and is NOT
			// included here.
			scanErrs = append(scanErrs, fmt.Errorf("account %s: %w", *st.AccountID, scanErr))
		}
	}
	if len(scanErrs) > 0 {
		// ANY enabled-account scan failed → fail the job so River retries the
		// whole set (re-running is idempotent via the upsert dedup).
		return matched, errors.Join(scanErrs...)
	}

	// Drive aggregation once for the contact so the just-upserted rows derive
	// interactions in this rematch pass (rather than waiting for the next sweep).
	if matched > 0 {
		if aggErr := b.engine.AggregateForContactBatch(ctx, contactID); aggErr != nil {
			return matched, fmt.Errorf("aggregate for contact: %w", aggErr)
		}
	}
	return matched, nil
}

// GChatHandleRematchHandler implements service.RematchHandler for the "gchat"
// identifier type.
type GChatHandleRematchHandler struct {
	base gchatRematchBase
}

// NewGChatHandleRematchHandler constructs the gchat-handle rematch handler.
func NewGChatHandleRematchHandler(p *GChatSyncProvider, states GChatSyncStateLister, comms *repository.CommsMessageRepository, engine *aggregation.Engine) *GChatHandleRematchHandler {
	return &GChatHandleRematchHandler{base: gchatRematchBase{provider: p, states: states, commsRepo: comms, engine: engine}}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *GChatHandleRematchHandler) IdentifierType() string { return "gchat" }

// Rematch runs the gated historical GChat scan for the newly-added gchat handle.
func (h *GChatHandleRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	return h.base.rematch(ctx, contactID, valueNormalized)
}

// GChatEmailRematchHandler implements service.RematchHandler for the "email"
// identifier type. It co-registers alongside the Gmail and Calendar email
// handlers; its gate is gchat-scoped, so it no-ops while the other email
// handlers do their real work.
type GChatEmailRematchHandler struct {
	base gchatRematchBase
}

// NewGChatEmailRematchHandler constructs the gchat-email rematch handler.
func NewGChatEmailRematchHandler(p *GChatSyncProvider, states GChatSyncStateLister, comms *repository.CommsMessageRepository, engine *aggregation.Engine) *GChatEmailRematchHandler {
	return &GChatEmailRematchHandler{base: gchatRematchBase{provider: p, states: states, commsRepo: comms, engine: engine}}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *GChatEmailRematchHandler) IdentifierType() string { return "email" }

// Rematch runs the gated historical GChat scan for the newly-added email
// address (GChat sender addresses ARE emails, so an email method is a valid
// gchat identity).
func (h *GChatEmailRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	return h.base.rematch(ctx, contactID, valueNormalized)
}
