// Integration coverage for SyncService.ReconcileGChatSyncStates (GChat
// integration PR 3 — the GO-LIVE enablement reconciliation). These tests drive
// the REAL service method against a REAL external_sync_state table via the
// repository (no raw SQL), with a stub GoogleAccountLister (no OAuth). They
// prove the chat-scope enablement gate + the idempotency contract that makes
// the store-only / event-free feature safe to run on every boot AND every OAuth
// connect:
//   - one enabled (gchat, account) state created per SCOPED Google credential;
//   - an account missing the chat scope gets NO state (the enablement gate —
//     the one behavioral departure from the email reconciliation);
//   - re-running creates no duplicates and resets no cursor/metadata;
//   - a user-disabled state is never re-enabled;
//   - a re-consent (scope appears on a second pass) opens the gate;
//   - the per-account shape invariant holds (no (gchat, NULL) row is produced);
//   - empty account list / nil lister are no-ops;
//   - the OAuth-connect reconciler closure is idempotent on re-connect;
//   - the status data path (GetSyncStatus) surfaces the gchat state (DD-5).
//
// Account ids are synthetic placeholders (NO real PII). All assertions go
// through the SyncRepository; timestamps are accelerated.GetCurrentTime()-safe.
// The reconcileEnv + reconcileStubLister + uniqueAccount helpers are shared with
// gmail_enablement_reconcile_test.go (same package).
package tests

import (
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

// The full chat-scope set the enablement gate requires (all three). A partial
// grant (any subset) must NOT enable the account.
const (
	gchatSpacesReadonlyScopeForTest      = "https://www.googleapis.com/auth/chat.spaces.readonly"
	gchatMessagesReadonlyScopeForTest    = "https://www.googleapis.com/auth/chat.messages.readonly"
	gchatMembershipsReadonlyScopeForTest = "https://www.googleapis.com/auth/chat.memberships.readonly"
)

// allChatScopesForTest returns the full chat-scope set a credential needs to be
// enabled by ReconcileGChatSyncStates, optionally prefixed with extra scopes.
func allChatScopesForTest(extra ...string) []string {
	return append(extra,
		gchatSpacesReadonlyScopeForTest,
		gchatMessagesReadonlyScopeForTest,
		gchatMembershipsReadonlyScopeForTest,
	)
}

// chatScopedCred builds a credential carrying the full chat-scope set (the
// enablement-positive case).
func chatScopedCred(accountID string) repository.OAuthCredentialStatus {
	return credWithScope(accountID, allChatScopesForTest()...)
}

func (e *reconcileEnv) getGChatState(t *testing.T, accountID string) *repository.SyncState {
	t.Helper()
	acct := accountID
	st, err := e.syncRepo.GetSyncStateBySource(e.ctx, "gchat", &acct)
	require.NoError(t, err)
	return st
}

// countGChatStatesForAccount counts live (gchat, account) rows via the
// repository's ListSyncStates (no raw SQL).
func (e *reconcileEnv) countGChatStatesForAccount(t *testing.T, accountID string) int {
	t.Helper()
	states, err := e.syncRepo.ListSyncStates(e.ctx)
	require.NoError(t, err)
	n := 0
	for _, st := range states {
		if st.Source == "gchat" && st.AccountID != nil && *st.AccountID == accountID {
			n++
		}
	}
	return n
}

// --- creates one state per SCOPED credential; unscoped gets none ------------

func TestReconcileGChat_CreatesStatePerScopedCredential(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	scoped1 := uniqueAccount("gchat-scoped1")
	scoped2 := uniqueAccount("gchat-scoped2")
	unscoped := uniqueAccount("gchat-unscoped")
	for _, acct := range []string{scoped1, scoped2, unscoped} {
		e.cleanupAccount(t, acct)
	}

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(scoped1),
		chatScopedCred(scoped2),
		// Unscoped: connected but never re-consented to the chat scopes.
		credWithScope(unscoped, "openid", "email", "profile"),
	}}
	e.service.SetGChatAccountLister(lister)

	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	// Both scoped accounts get an enabled state with the expected shape.
	for _, acct := range []string{scoped1, scoped2} {
		st := e.getGChatState(t, acct)
		require.Equal(t, "gchat", st.Source)
		require.NotNil(t, st.AccountID, "per-account shape: account_id must be set")
		require.Equal(t, acct, *st.AccountID)
		require.True(t, st.Enabled, "scoped account is enabled")
		require.Equal(t, repository.SyncStrategyContactDriven, st.Strategy)
		require.Equal(t, repository.SyncStatusIdle, st.Status)
		require.Nil(t, st.SyncCursor, "cursor starts empty")
		require.Nil(t, st.NextSyncAt, "next_sync_at NULL → immediately due on first tick")
		require.Equal(t, "2026-01-01", st.Metadata["backfill_since"])
	}

	// The UNSCOPED account has NO gchat state — the enablement gate.
	unscopedAcct := unscoped
	_, err := e.syncRepo.GetSyncStateBySource(e.ctx, "gchat", &unscopedAcct)
	require.ErrorIs(t, err, db.ErrNotFound, "unscoped account must NOT get a gchat state (enablement gate)")
}

// --- scope gate is the CREATE gate, not warn-and-create ---------------------

func TestReconcileGChat_ScopeGate_NoStateForUnscopedAccount(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-gate")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, "openid", "email", "profile", gmailReadonlyScopeForTest),
	}}
	e.service.SetGChatAccountLister(lister)

	// Reconciliation succeeds (nil) but creates NO state for the unscoped
	// account — contrast with email, which creates + warns.
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	acctID := acct
	_, err := e.syncRepo.GetSyncStateBySource(e.ctx, "gchat", &acctID)
	require.ErrorIs(t, err, db.ErrNotFound, "scope gate: unscoped account gets no state")
	require.Equal(t, 0, e.countGChatStatesForAccount(t, acct))
}

// --- scope gate requires the FULL chat-scope set (partial grants rejected) ---

func TestReconcileGChat_ScopeGate_PartialChatScopesNotEnabled(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)

	// Each subset is missing at least one required scope, so none must enable.
	partials := map[string][]string{
		"spaces-only":      {gchatSpacesReadonlyScopeForTest},
		"messages-only":    {gchatMessagesReadonlyScopeForTest},
		"memberships-only": {gchatMembershipsReadonlyScopeForTest},
		"spaces+messages":  {gchatSpacesReadonlyScopeForTest, gchatMessagesReadonlyScopeForTest},
		"missing-spaces":   {gchatMessagesReadonlyScopeForTest, gchatMembershipsReadonlyScopeForTest},
	}

	for name, scopes := range partials {
		t.Run(name, func(t *testing.T) {
			acct := uniqueAccount("gchat-partial-" + name)
			e.cleanupAccount(t, acct)

			lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
				credWithScope(acct, append([]string{"openid", "email"}, scopes...)...),
			}}
			e.service.SetGChatAccountLister(lister)

			require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

			acctID := acct
			_, err := e.syncRepo.GetSyncStateBySource(e.ctx, "gchat", &acctID)
			require.ErrorIs(t, err, db.ErrNotFound, "partial chat-scope grant must NOT enable gchat sync")
			require.Equal(t, 0, e.countGChatStatesForAccount(t, acct))
		})
	}
}

// --- idempotent re-run: no duplicates, no cursor/metadata reset -------------

func TestReconcileGChat_IdempotentRerun_NoDuplicatesNoReset(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-idem")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(acct),
	}}
	e.service.SetGChatAccountLister(lister)

	// First run creates the state.
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
	created := e.getGChatState(t, acct)

	// Simulate steady-state progress: advance metadata as a sweep would
	// (per-space cursors + a different backfill floor).
	advanced := map[string]any{
		"backfill_since": "2026-03-15",
		"space_cursors":  map[string]any{"spaces/AAA": "token-123"},
		"extra":          "keep-me",
	}
	_, err := e.syncRepo.UpdateSyncStateMetadata(e.ctx, created.ID, advanced)
	require.NoError(t, err)

	// Re-run reconciliation.
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	// Still exactly one (gchat, account) state, metadata intact.
	after := e.getGChatState(t, acct)
	require.Equal(t, created.ID, after.ID, "no duplicate state; same row")
	require.Equal(t, "2026-03-15", after.Metadata["backfill_since"], "backfill NOT reset")
	require.Equal(t, "keep-me", after.Metadata["extra"], "existing metadata preserved")
	require.Contains(t, after.Metadata, "space_cursors", "space_cursors NOT reset")
	cursors, ok := after.Metadata["space_cursors"].(map[string]any)
	require.True(t, ok, "space_cursors preserved as a map")
	require.Equal(t, "token-123", cursors["spaces/AAA"], "per-space cursor preserved")

	require.Equal(t, 1, e.countGChatStatesForAccount(t, acct))
}

// --- respects a user-disabled state (never re-enabled) ----------------------

func TestReconcileGChat_RespectsUserDisabled(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-disabled")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(acct),
	}}
	e.service.SetGChatAccountLister(lister)

	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
	created := e.getGChatState(t, acct)

	// User disables the account via the existing enable/disable path.
	_, err := e.syncRepo.UpdateSyncStateEnabled(e.ctx, created.ID, false)
	require.NoError(t, err)

	// Re-run reconciliation — must NOT re-enable (create-if-absent only).
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	after := e.getGChatState(t, acct)
	require.Equal(t, created.ID, after.ID)
	require.False(t, after.Enabled, "user-disabled state must stay disabled (never re-enabled)")
}

// --- re-consent transition: the gate opens when the scope appears -----------

func TestReconcileGChat_ReConsentTransition_GateOpens(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-reconsent")
	e.cleanupAccount(t, acct)

	// Start unscoped: no state created.
	unscopedLister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, "openid", "email", "profile"),
	}}
	e.service.SetGChatAccountLister(unscopedLister)
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
	require.Equal(t, 0, e.countGChatStatesForAccount(t, acct), "no state before re-consent")

	// A PARTIAL grant (only spaces.readonly, missing messages/memberships) must
	// NOT open the gate — enabling on a subset would only fail mid-sweep.
	partialLister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, "openid", "email", "profile", gchatSpacesReadonlyScopeForTest),
	}}
	e.service.SetGChatAccountLister(partialLister)
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
	require.Equal(t, 0, e.countGChatStatesForAccount(t, acct), "partial chat-scope grant must not enable")

	// Simulate full re-consent: the same account now carries ALL THREE chat scopes.
	scopedLister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, allChatScopesForTest("openid", "email", "profile")...),
	}}
	e.service.SetGChatAccountLister(scopedLister)
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	st := e.getGChatState(t, acct)
	require.True(t, st.Enabled, "state created + enabled after full re-consent")
	require.Equal(t, 1, e.countGChatStatesForAccount(t, acct))
}

// --- per-account shape invariant: no (gchat, NULL) row ever produced --------

func TestReconcileGChat_PerAccountShape_NoNullAccountRow(t *testing.T) {
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-shape")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(acct),
		// An empty-account-id credential (even fully chat-scoped) must be
		// skipped, not turned into a (gchat, NULL) row the provider rejects.
		chatScopedCred(""),
	}}
	e.service.SetGChatAccountLister(lister)

	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	// The per-account row exists with a non-NULL account_id.
	st := e.getGChatState(t, acct)
	require.NotNil(t, st.AccountID)
	require.Equal(t, acct, *st.AccountID)

	// No (gchat, NULL) orphan row exists.
	_, err := e.syncRepo.GetSyncStateBySource(e.ctx, "gchat", nil)
	require.ErrorIs(t, err, db.ErrNotFound, "reconciliation must never create a (gchat, NULL) row")
}

// --- empty account list is a no-op ------------------------------------------

func TestReconcileGChat_EmptyAccountList_NoOp(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	lister := &reconcileStubLister{accounts: nil}
	e.service.SetGChatAccountLister(lister)

	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
	require.Equal(t, 1, lister.calls, "lister was consulted")
}

// --- nil lister is a no-op (does not even consult a lister) ------------------

func TestReconcileGChat_NilLister_NoOp(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	// No SetGChatAccountLister call → nil lister.
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))
}

// --- OAuth-connect reconciler closure is idempotent on re-connect -----------

func TestReconcileGChat_ConnectPath_IdempotentOnReconnect(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	scoped := uniqueAccount("gchat-connect")
	unscoped := uniqueAccount("gchat-connect-unscoped")
	e.cleanupAccount(t, scoped)
	e.cleanupAccount(t, unscoped)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(scoped),
		credWithScope(unscoped, "openid", "email", "profile"),
	}}
	e.service.SetGChatAccountLister(lister)

	// The OAuth-connect hook (main.go) is exactly this closure shape.
	reconcile := func() error {
		return e.service.ReconcileGChatSyncStates(e.ctx)
	}

	// First connect creates the scoped state; the unscoped one gets nothing.
	require.NoError(t, reconcile())
	first := e.getGChatState(t, scoped)
	require.Equal(t, 0, e.countGChatStatesForAccount(t, unscoped), "unscoped connect creates no state")

	// Second connect of the same accounts is a no-op (no duplicate, untouched).
	require.NoError(t, reconcile())
	second := e.getGChatState(t, scoped)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, e.countGChatStatesForAccount(t, scoped))
}

// --- status data path surfaces the gchat state (DD-5 verification) ----------

func TestReconcileGChat_StatusSurfacesGChatState(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("gchat-status")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(acct),
	}}
	e.service.SetGChatAccountLister(lister)
	require.NoError(t, e.service.ReconcileGChatSyncStates(e.ctx))

	// GetSyncStatus is the data path behind GET /api/v1/sync/status. The gchat
	// state must surface there with the generic SyncState shape (no new fields)
	// once reconciliation enables it — this is what "mirror Gmail's status
	// shape" means (DD-5): GChat surfaces through the identical generic row.
	states, err := e.syncRepo.ListSyncStates(e.ctx)
	require.NoError(t, err)

	var found *repository.SyncState
	for i := range states {
		st := states[i]
		if st.Source == "gchat" && st.AccountID != nil && *st.AccountID == acct {
			found = &states[i]
			break
		}
	}
	require.NotNil(t, found, "gchat state must surface via GetSyncStatus (the /sync/status data path)")
	require.True(t, found.Enabled)
	require.Equal(t, "2026-01-01", found.Metadata["backfill_since"])
}
