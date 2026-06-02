package google

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
)

// emailSyncStateLister is the narrow seam the rematch handler uses to find the
// accounts whose email sync is enabled. Production satisfies it with
// *repository.SyncRepository; tests satisfy it with the real repository seeded
// with enabled email sync states. Mirrors the consumer-side narrow-interface
// convention.
type emailSyncStateLister interface {
	ListEnabledSyncStates(ctx context.Context) ([]repository.SyncState, error)
}

// GmailRematchHandler implements service.RematchHandler for the "email"
// identifier type. On contact_methods.added, it runs a one-shot
// identifier-scoped historical Gmail scan for the newly-added address across
// the connected accounts whose email sync is ENABLED, publishing
// email.received/sent so the EmailInteractionConsumer derives interactions.
// Match-only (never creates a contact); fans out to every contact sharing the
// address via the known-contact map. Steady-state account cursors are NOT
// rewound (spec §3.3).
type GmailRematchHandler struct {
	provider  *GmailSyncProvider                 // owns fetch/process/persist + bus + pool
	states    emailSyncStateLister               // enumerate ENABLED email sync states (prod: *repository.SyncRepository)
	commsRepo *repository.CommsMessageRepository // load known-contact map (ListEmailIdentitiesForSync)
}

// NewGmailRematchHandler constructs a GmailRematchHandler. Production passes the
// *repository.SyncRepository as the enabled-email-states lister.
func NewGmailRematchHandler(p *GmailSyncProvider, states emailSyncStateLister, comms *repository.CommsMessageRepository) *GmailRematchHandler {
	return &GmailRematchHandler{
		provider:  p,
		states:    states,
		commsRepo: comms,
	}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *GmailRematchHandler) IdentifierType() string { return "email" }

// Rematch runs an identifier-scoped historical Gmail scan for the newly-added
// address across every connected account whose email sync is enabled. The scan
// is address-scoped, not contact-scoped: it fans out to all contacts sharing
// the address via the known-contact map (which already includes the just-added
// pair). contactID is referenced only for the rematch fan-out semantics, not
// logged.
func (h *GmailRematchHandler) Rematch(ctx context.Context, _ uuid.UUID, valueNormalized string) (int, error) {
	// Defensive normalization, mirroring CalendarRematchHandler.
	addr := matching.NormalizeEmail(valueNormalized)
	if addr == "" {
		return 0, nil
	}

	// Gate on the enabled email sync states FIRST so the no-op path does zero
	// work (no identity-map build, no me-set, no fetcher). Only scan accounts
	// whose email sync is enabled and non-disabled — the same gate the
	// scheduler uses (ListEnabledSyncStates: enabled = TRUE AND status !=
	// 'disabled'). An account that is never enabled (or disabled by the user)
	// must not be scanned by the rematch.
	states, err := h.states.ListEnabledSyncStates(ctx)
	if err != nil {
		return 0, err
	}
	var emailStates []repository.SyncState
	for _, st := range states {
		if st.Source == GmailSourceName && st.AccountID != nil && strings.TrimSpace(*st.AccountID) != "" {
			emailStates = append(emailStates, st)
		}
	}
	if len(emailStates) == 0 {
		// No enabled email account → nothing to scan. No-op (no map/me-set/
		// fetcher work, no writes).
		return 0, nil
	}

	// Build the known-contact map ONCE from committed contact_method rows. The
	// FULL map (not just {addr: [contactID]}) is required so processMessage's
	// participant/direction resolution sees the complete A_C address set for
	// each candidate contact — a shared address may already belong to several
	// contacts, and a contact may have other addresses too. Reusing
	// ListEmailIdentitiesForSync guarantees identical resolution semantics to
	// the steady-state Sync.
	identities, err := h.commsRepo.ListEmailIdentitiesForSync(ctx)
	if err != nil {
		return 0, err
	}
	knownMap := make(map[string][]uuid.UUID)
	for _, id := range identities {
		knownMap[id.ValueNormalized] = append(knownMap[id.ValueNormalized], id.ContactID)
	}

	// Build the me-set ONCE through the provider's seam (tests override it via
	// SetMeSetForTest, so no real OAuth is touched).
	meSet, err := h.provider.MeSet(ctx)
	if err != nil {
		return 0, err
	}

	// Scan each enabled account with its OWN backfill floor (per-state
	// metadata["backfill_since"], default 2026-01-01 — NOT a hardcoded floor).
	// Re-running the whole scan is safe: the comms_message upsert dedup + the
	// event (source, source_id) unique collapse re-fetched messages, so a River
	// retry that re-scans an already-succeeded account produces no duplicate
	// rows/interactions. This makes "fail the job if ANY enabled account scan
	// fails" safe — River retries the whole set, bounded by the job's
	// MaxAttempts.
	matched := 0
	var scanErrs []error
	for _, st := range emailStates {
		afterEpoch := backfillSinceEpoch(st.Metadata)
		n, scanErr := h.provider.ScanIdentifier(ctx, *st.AccountID, addr, knownMap, meSet, afterEpoch)
		matched += n
		if scanErr != nil {
			// The account id is the operator's OWN connected mailbox, kept raw
			// for triage (operational provenance); the rematched contact
			// address is third-party PII and is deliberately NOT included here.
			scanErrs = append(scanErrs, fmt.Errorf("account %s: %w", *st.AccountID, scanErr))
		}
	}
	if len(scanErrs) > 0 {
		// ANY enabled-account scan failed → fail the job so River retries the
		// whole set rather than permanently stranding that account's history.
		return matched, errors.Join(scanErrs...)
	}
	return matched, nil
}
