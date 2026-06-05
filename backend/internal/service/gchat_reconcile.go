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
	// gchatSyncSource is the external_sync_state.source value for Google Chat
	// sync. It matches google.GChatSourceName and comms_message.source /
	// interaction.source value 'gchat'. Duplicated as a local literal so the
	// service layer does not import the google package (which would invert the
	// dependency direction).
	gchatSyncSource = "gchat"

	// The GChat sweep authenticates against three Chat scopes: spaces.list needs
	// chat.spaces.readonly, messages.list needs chat.messages.readonly, and
	// spaces.members.list needs chat.memberships.readonly under User auth. The
	// enablement gate requires ALL THREE in the stored requested-scope list — a
	// partial grant (e.g. spaces-only) would let the sweep start but fail mid-run
	// on the first messages/members call, so we do not enable until the account
	// has fully re-consented. Defined as literals so the service layer does not
	// import the chat API client just to name scope strings; each MUST match the
	// corresponding chat.Chat*ReadonlyScope constant.
	gchatSpacesReadonlyScope      = "https://www.googleapis.com/auth/chat.spaces.readonly"
	gchatMessagesReadonlyScope    = "https://www.googleapis.com/auth/chat.messages.readonly"
	gchatMembershipsReadonlyScope = "https://www.googleapis.com/auth/chat.memberships.readonly"
)

// gchatRequiredScopes is the full set of Chat scopes an account must carry in
// its stored requested-scope list before GChat sync is enabled for it.
var gchatRequiredScopes = []string{
	gchatSpacesReadonlyScope,
	gchatMessagesReadonlyScope,
	gchatMembershipsReadonlyScope,
}

// SetGChatAccountLister wires the Google account lister used by
// ReconcileGChatSyncStates. Nil-default: when unset (tests / no-Google builds)
// ReconcileGChatSyncStates is a no-op. Reuses the same GoogleAccountLister
// interface as the email reconciliation (both are satisfied by
// *google.OAuthService); a dedicated field keeps gchat enablement independently
// nil-gated from email wiring. Safe to call once at boot; not safe to call
// concurrently with an in-flight ReconcileGChatSyncStates.
func (s *SyncService) SetGChatAccountLister(lister GoogleAccountLister) {
	s.gchatAccountLister = lister
}

// ReconcileGChatSyncStates ensures one enabled GChat sync state exists per
// connected Google credential THAT HAS THE CHAT SCOPES GRANTED. It is the
// headless enablement routine that flips the (store-only, event-free) Google
// Chat feature on: nothing creates a gchat state automatically, so without this
// no state is ever created and the scheduler runs no sweep.
//
// The chat-scope gate is the one behavioral departure from
// ReconcileEmailSyncStates (which creates a state regardless and only warns on a
// missing scope). For GChat the full chat-scope set is the ENABLEMENT gate: an
// account that has not re-consented to ALL THREE chat scopes gets NO state and
// NO sweep, which is what keeps the feature inert until the user re-consents
// (spec §7). Requiring the full set (not just spaces.list's scope) avoids
// enabling an account whose sweep would start but fail mid-run on the first
// messages/members call. The stored scope list is the requested-scope proxy for
// "re-consented since the chat scopes were added"; the provider's clean
// 403-degrade backstops a proxy-positive-but-not-truly-granted account.
//
// Idempotency contract (run on every boot AND on every OAuth connect):
//   - create-if-absent ONLY. For each scoped account it probes
//     GetSyncStateBySource("gchat", &accountID); on db.ErrNotFound it creates a
//     per-account state (enabled=true, strategy=contact_driven,
//     metadata={"backfill_since": sync.EmailBackfillSinceDefault}, empty cursor,
//     NULL next_sync_at → immediately due on the next tick). The empty
//     space_cursors map is materialized by the provider on its first sweep, so
//     it is not seeded here.
//   - a found state is left COMPLETELY untouched: no re-enable, no metadata
//     rewrite, no cursor reset. Re-running must never duplicate states, reset
//     cursors/backfill on already-enabled accounts, or re-enable an account the
//     user manually disabled.
//
// A no-op when no account lister is wired (nil-default) or no Google accounts
// are connected. A per-account create error is returned (callers log it WARN and
// do not block boot); a benign concurrent-create unique violation is treated as
// "already exists, skip" rather than a failure.
func (s *SyncService) ReconcileGChatSyncStates(ctx context.Context) error {
	if s.gchatAccountLister == nil {
		logger.Debug().Msg("gchat sync reconciliation: no account lister wired; skipping")
		return nil
	}

	accounts, err := s.gchatAccountLister.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list google accounts: %w", err)
	}

	for _, account := range accounts {
		accountID := account.AccountID
		if accountID == "" {
			// A credential with no account id cannot be probed per-account
			// (GetSyncStateBySource COALESCE-matches account_id) and would
			// produce a stray (gchat, NULL) row the provider rejects. Skip it.
			logger.Warn().Msg("gchat sync reconciliation: skipping google credential with empty account id")
			continue
		}

		// Scope gate FIRST: an account missing ANY of the three chat scopes is
		// not enabled (no state created). This is the enablement gate — unlike
		// the email reconciliation, which creates regardless and only warns. The
		// account id is the user's own connected mailbox address (operational
		// provenance, already in oauth_credential), not third-party PII.
		if !accountHasChatScopes(account) {
			logger.Info().
				Str("account", accountID).
				Msg("gchat sync: account missing one or more chat scopes; not enabling until reconnected")
			continue
		}

		acctID := accountID
		_, getErr := s.syncRepo.GetSyncStateBySource(ctx, gchatSyncSource, &acctID)
		if getErr == nil {
			// Found: leave it completely untouched (idempotency contract).
			continue
		}
		if !errors.Is(getErr, db.ErrNotFound) {
			return fmt.Errorf("probe gchat sync state for account: %w", getErr)
		}

		// Absent: create the per-account enabled state.
		if _, createErr := s.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:    gchatSyncSource,
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
				logger.Debug().Msg("gchat sync reconciliation: state already created concurrently; skipping")
				continue
			}
			return fmt.Errorf("create gchat sync state for account: %w", createErr)
		}

		logger.Info().
			Str("source", gchatSyncSource).
			Str("account", accountID).
			Msg("gchat sync reconciliation: created enabled gchat sync state")
	}

	return nil
}

// accountHasChatScopes reports whether a connected Google account's stored scope
// list contains ALL THREE chat scopes — the enablement gate for GChat sync. A
// partial grant returns false: the sweep needs every scope (spaces.list,
// messages.list, members.list), so enabling on a subset would only fail mid-run.
func accountHasChatScopes(account repository.OAuthCredentialStatus) bool {
	have := make(map[string]struct{}, len(account.Scopes))
	for _, scope := range account.Scopes {
		have[scope] = struct{}{}
	}
	for _, required := range gchatRequiredScopes {
		if _, ok := have[required]; !ok {
			return false
		}
	}
	return true
}
