package service

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	syncpkg "personal-crm/backend/internal/sync"
)

const (
	// emailSyncSource is the external_sync_state.source value for Gmail email
	// sync. It matches google.GmailSourceName and comms_message.source /
	// interaction.source value 'email' (spec §6.2/§6.3). Duplicated as a local
	// literal so the service layer does not import the google package (which
	// would invert the dependency direction).
	emailSyncSource = "email"

	// gmailReadonlyScope is the OAuth scope a connected Google account needs for
	// the email sweep to authenticate against Gmail. Defined as a literal so the
	// service layer does not import the gmail API client just to name a scope
	// string. Matches gmail.GmailReadonlyScope.
	gmailReadonlyScope = "https://www.googleapis.com/auth/gmail.readonly"
)

// GoogleAccountLister is the narrow account-enumeration seam
// ReconcileEmailSyncStates needs. Production satisfies it with
// *google.OAuthService; tests satisfy it with a stub so the reconciliation
// needs no real OAuth. Follows the narrow-interface convention used across the
// google package and keeps the service package free of a google import.
type GoogleAccountLister interface {
	ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error)
}

// SetEmailAccountLister wires the Google account lister used by
// ReconcileEmailSyncStates. Nil-default: when unset (tests / no-Google builds)
// ReconcileEmailSyncStates is a no-op. Safe to call once at boot; not safe to
// call concurrently with an in-flight ReconcileEmailSyncStates.
func (s *SyncService) SetEmailAccountLister(lister GoogleAccountLister) {
	s.emailAccountLister = lister
}

// ReconcileEmailSyncStates ensures exactly one enabled email sync state exists
// per connected Google credential. It is the headless enablement routine that
// flips the (store-only, NO-UI) Gmail feature on: nothing ever calls
// POST /sync/trigger for email, so without this no email state is ever created
// and the scheduler runs no sweep.
//
// Idempotency contract (run on every boot AND on every OAuth connect):
//   - create-if-absent ONLY. For each account it probes
//     GetSyncStateBySource("email", &accountID); on db.ErrNotFound it creates a
//     per-account state (enabled=true, strategy=contact_driven,
//     metadata={"backfill_since": sync.EmailBackfillSinceDefault}, empty cursor, NULL
//     next_sync_at → immediately due on the next tick).
//   - a found state is left COMPLETELY untouched: no re-enable, no metadata
//     rewrite, no cursor reset. Re-running must never duplicate states, reset
//     cursors/backfill on already-enabled accounts, or re-enable an account the
//     user manually disabled.
//
// The scope check runs per account on BOTH branches so an account whose
// gmail.readonly was granted only after its state was created still surfaces the
// warning until reconnected.
//
// A no-op when no account lister is wired (nil-default) or no Google accounts
// are connected. A per-account create error is returned (callers log it WARN and
// do not block boot); a benign concurrent-create unique violation is treated as
// "already exists, skip" rather than a failure.
func (s *SyncService) ReconcileEmailSyncStates(ctx context.Context) error {
	if s.emailAccountLister == nil {
		logger.Debug().Msg("email sync reconciliation: no account lister wired; skipping")
		return nil
	}

	accounts, err := s.emailAccountLister.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list google accounts: %w", err)
	}

	for _, account := range accounts {
		accountID := account.AccountID
		if accountID == "" {
			// A credential with no account id cannot be probed per-account
			// (GetSyncStateBySource COALESCE-matches account_id) and would
			// produce a stray (email, NULL) row the provider rejects. Skip it.
			logger.Warn().Msg("email sync reconciliation: skipping google credential with empty account id")
			continue
		}

		// Scope check runs unconditionally (create AND found branches).
		s.warnIfMissingGmailScope(account)

		acctID := accountID
		_, getErr := s.syncRepo.GetSyncStateBySource(ctx, emailSyncSource, &acctID)
		if getErr == nil {
			// Found: leave it completely untouched (idempotency contract).
			continue
		}
		if !errors.Is(getErr, db.ErrNotFound) {
			return fmt.Errorf("probe email sync state for account: %w", getErr)
		}

		// Absent: create the per-account enabled state.
		if _, createErr := s.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    emailSyncSource,
			AccountID: &acctID,
			Enabled:   true,
			Strategy:  repository.SyncStrategyContactDriven,
			Metadata:  map[string]any{"backfill_since": syncpkg.EmailBackfillSinceDefault},
			// NextSyncAt left nil → NULL → immediately due on the next tick.
		}); createErr != nil {
			if isUniqueViolation(createErr) {
				// Benign concurrent create (another connect/boot raced us).
				// The row now exists and is untouched, which is the desired
				// end state — skip rather than fail the whole reconciliation.
				logger.Debug().Msg("email sync reconciliation: state already created concurrently; skipping")
				continue
			}
			return fmt.Errorf("create email sync state for account: %w", createErr)
		}

		logger.Info().
			Str("source", emailSyncSource).
			Str("account", accountID).
			Msg("email sync reconciliation: created enabled email sync state")
	}

	return nil
}

// warnIfMissingGmailScope logs a single WARN when a connected Google account is
// missing the gmail.readonly scope. The email sweep for that account will fail
// its OAuth messages.list call until the user reconnects through the existing
// Google connect flow (which re-requests the full scope set). Store-only / NO-UI
// feature: there is no reconnect prompt to surface, so the operator log is the
// honest minimal handling. The account id is the user's own connected mailbox
// address (already in oauth_credential); logging it at value level is
// operational provenance, not third-party PII.
func (s *SyncService) warnIfMissingGmailScope(account repository.OAuthCredentialStatus) {
	for _, scope := range account.Scopes {
		if scope == gmailReadonlyScope {
			return
		}
	}
	logger.Warn().
		Str("account", account.AccountID).
		Msg("gmail sync: account missing gmail.readonly scope; email sync for this account will fail auth until reconnected")
}
