package google

import (
	"context"

	"github.com/google/uuid"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
)

// accountLister is the narrow account-enumeration seam the rematch handler
// needs. Production satisfies it with *OAuthService; tests satisfy it with a
// stub so the handler needs no real OAuth. Mirrors the consumer-side
// narrow-interface convention.
type accountLister interface {
	ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error)
}

// GmailRematchHandler implements service.RematchHandler for the "email"
// identifier type. On contact_methods.added, it runs a one-shot
// identifier-scoped historical Gmail scan across ALL connected Google accounts
// for the newly-added address, publishing email.received/sent so the phase-3
// EmailInteractionConsumer derives interactions. Match-only (never creates a
// contact); fans out to every contact sharing the address. Steady-state account
// cursors are NOT rewound (spec §3.3).
//
// INERT in production until phase 5: NOT registered in main.go. No production
// code path constructs it.
type GmailRematchHandler struct {
	provider  *GmailSyncProvider                 // owns fetch/process/persist + bus + pool
	accounts  accountLister                      // enumerate connected accounts (prod: *OAuthService)
	commsRepo *repository.CommsMessageRepository // load known-contact map (ListEmailIdentitiesForSync)
}

// NewGmailRematchHandler constructs a GmailRematchHandler. Production passes the
// *OAuthService as the accountLister.
func NewGmailRematchHandler(p *GmailSyncProvider, accounts accountLister, comms *repository.CommsMessageRepository) *GmailRematchHandler {
	return &GmailRematchHandler{
		provider:  p,
		accounts:  accounts,
		commsRepo: comms,
	}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *GmailRematchHandler) IdentifierType() string { return "email" }

// Rematch runs an identifier-scoped historical Gmail scan for the newly-added
// address across every connected account. The scan is address-scoped, not
// contact-scoped: it fans out to all contacts sharing the address via the
// known-contact map (which already includes the just-added pair). contactID is
// referenced only for log traceability.
func (h *GmailRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	// Defensive normalization, mirroring CalendarRematchHandler.
	addr := matching.NormalizeEmail(valueNormalized)
	if addr == "" {
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

	// The rematch scan holds no per-account external_sync_state row and must
	// not read/write one (spec §3.3), so it floors at the default backfill
	// since-date directly. backfillSinceEpoch is unexported but in-package.
	afterEpoch := backfillSinceEpoch(nil)

	accounts, err := h.accounts.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}

	matched := 0
	allErrored := len(accounts) > 0
	var lastErr error
	for _, a := range accounts {
		n, scanErr := h.provider.ScanIdentifier(ctx, a.AccountID, addr, knownMap, meSet, afterEpoch)
		matched += n
		if scanErr != nil {
			// Continue-on-error: one account's Gmail outage should not fail the
			// whole rematch (mirrors CalendarRematchHandler's housekeeping-error
			// tolerance). Record the error in case every account fails.
			lastErr = scanErr
			logger.Warn().Err(scanErr).
				Str("account", a.AccountID).
				Str("contactId", contactID.String()).
				Str("address", addr).
				Msg("gmail rematch: account scan failed; continuing")
			continue
		}
		allErrored = false
	}

	// Only fail the job (so River retries the whole method set) when EVERY
	// account errored AND nothing matched — otherwise partial success returns
	// nil and the job completes.
	if allErrored && matched == 0 && lastErr != nil {
		return 0, lastErr
	}
	return matched, nil
}
