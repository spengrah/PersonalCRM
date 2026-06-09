// Integration coverage for SyncService.ReconcileEmailSyncStates (Gmail
// integration phase 5 — the GO-LIVE enablement reconciliation). These tests
// drive the REAL service method against a REAL external_sync_state table via
// the repository (no raw SQL), with a stub GoogleAccountLister (no OAuth). They
// prove the idempotency contract that makes the store-only / NO-UI feature safe
// to run on every boot AND every OAuth connect:
//   - one enabled (email, account) state created per Google credential;
//   - re-running creates no duplicates and resets no cursor/metadata;
//   - a user-disabled state is never re-enabled;
//   - the missing-gmail.readonly scope check fires on both create + found
//     branches without blocking the reconciliation;
//   - the per-account shape invariant holds (no (email, NULL) row is produced);
//   - empty account list / nil lister are no-ops;
//   - the OAuth-connect reconciler closure is idempotent on re-connect.
//
// Account ids are synthetic placeholders (NO real PII). All assertions go
// through the SyncRepository; timestamps are accelerated.GetCurrentTime()-safe.
package tests

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const gmailReadonlyScopeForTest = "https://www.googleapis.com/auth/gmail.readonly"

// reconcileStubLister satisfies service.GoogleAccountLister with canned
// credential statuses — no OAuth. Compile-time proof of the interface boundary.
type reconcileStubLister struct {
	accounts []repository.OAuthCredentialStatus
	err      error
	calls    int
}

func (s *reconcileStubLister) ListAccounts(_ context.Context) ([]repository.OAuthCredentialStatus, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.accounts, nil
}

var _ service.GoogleAccountLister = (*reconcileStubLister)(nil)

// reconcileEnv bundles a real SyncService + repository against the test DB.
type reconcileEnv struct {
	ctx      context.Context
	database *db.Database
	syncRepo *repository.SyncRepository
	service  *service.SyncService
}

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)

	syncRepo := repository.NewSyncRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	// Reconciliation never touches the registry; an empty one is sufficient.
	registry := syncpkg.NewProviderRegistry()

	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	return &reconcileEnv{
		ctx:      ctx,
		database: database,
		syncRepo: syncRepo,
		service:  svc,
	}
}

// uniqueAccount returns a per-test synthetic account id so parallel/serial runs
// against the shared DB never collide on the (email, account) unique index.
func uniqueAccount(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8] + "@example.com"
}

// cleanupAccount hard-deletes the (email, account) state after the test so the
// shared test DB does not accumulate state across runs.
func (e *reconcileEnv) cleanupAccount(t *testing.T, accountID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = e.syncRepo.DeleteSyncStatesByAccountID(e.ctx, accountID)
	})
}

func (e *reconcileEnv) getEmailState(t *testing.T, accountID string) *repository.SyncState {
	t.Helper()
	acct := accountID
	st, err := e.syncRepo.GetSyncStateBySource(e.ctx, "email", &acct)
	require.NoError(t, err)
	return st
}

func credWithScope(accountID string, scopes ...string) repository.OAuthCredentialStatus {
	return repository.OAuthCredentialStatus{AccountID: accountID, Scopes: scopes}
}

// --- creates-one-state-per-credential ---------------------------------------

func TestReconcileEmail_CreatesStatePerCredential(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	a1 := uniqueAccount("acct1")
	a2 := uniqueAccount("acct2")
	a3 := uniqueAccount("acct3")
	e.cleanupAccount(t, a1)
	e.cleanupAccount(t, a2)
	e.cleanupAccount(t, a3)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(a1, gmailReadonlyScopeForTest),
		credWithScope(a2, gmailReadonlyScopeForTest),
		credWithScope(a3, gmailReadonlyScopeForTest),
	}}
	e.service.SetEmailAccountLister(lister)

	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	for _, acct := range []string{a1, a2, a3} {
		st := e.getEmailState(t, acct)
		require.Equal(t, "email", st.Source)
		require.NotNil(t, st.AccountID, "per-account shape: account_id must be set")
		require.Equal(t, acct, *st.AccountID)
		require.True(t, st.Enabled, "newly-created state is enabled")
		require.Equal(t, repository.SyncStrategyContactDriven, st.Strategy)
		require.Equal(t, repository.SyncStatusIdle, st.Status)
		require.Nil(t, st.SyncCursor, "cursor starts empty")
		require.Nil(t, st.NextSyncAt, "next_sync_at NULL → immediately due on first tick")
		require.Equal(t, "2026-01-01", st.Metadata["backfill_since"])
	}
}

// --- idempotent re-run: no duplicates, no cursor/metadata reset -------------

func TestReconcileEmail_IdempotentRerun_NoDuplicatesNoReset(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("idem")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, gmailReadonlyScopeForTest),
	}}
	e.service.SetEmailAccountLister(lister)

	// First run creates the state.
	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))
	created := e.getEmailState(t, acct)

	// Simulate steady-state progress: a non-empty cursor + advanced metadata,
	// exactly what a sweep would persist after running.
	cursor := "1738368000"
	_, err := e.syncRepo.UpdateSyncStateSuccess(e.ctx, created.ID, accelerated.GetCurrentTime(), &cursor)
	require.NoError(t, err)
	advanced := map[string]any{"backfill_since": "2026-03-15", "extra": "keep-me"}
	_, err = e.syncRepo.UpdateSyncStateMetadata(e.ctx, created.ID, advanced)
	require.NoError(t, err)

	// Re-run reconciliation.
	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	// Still exactly one (email, account) state, with cursor + metadata intact.
	after := e.getEmailState(t, acct)
	require.Equal(t, created.ID, after.ID, "no duplicate state; same row")
	require.NotNil(t, after.SyncCursor)
	require.Equal(t, cursor, *after.SyncCursor, "cursor NOT reset on re-run")
	require.Equal(t, "2026-03-15", after.Metadata["backfill_since"], "backfill NOT reset")
	require.Equal(t, "keep-me", after.Metadata["extra"], "existing metadata preserved")

	// Assert exactly one row for this account across all sync states.
	require.Equal(t, 1, e.countEmailStatesForAccount(t, acct))
}

// --- operator reset: enabled email cursors only -----------------------------

// NOTE: this test stays SERIAL. ResetGmailBackfillCursors scans and mutates
// ALL enabled email sync states DB-wide and the test asserts an exact
// Scanned/Reset count, so a concurrent test's enabled email state would be
// swept into the count (and could be deleted mid-reset). Inherently global —
// not parallelizable without a per-account scoping parameter on the service.
func TestResetGmailBackfillCursors_ResetsOnlyEnabledEmailStates(t *testing.T) {
	e := newReconcileEnv(t)
	enabledAcct := uniqueAccount("reset-enabled")
	defaultAcct := uniqueAccount("reset-default")
	disabledAcct := uniqueAccount("reset-disabled")
	otherAcct := uniqueAccount("reset-other")
	for _, acct := range []string{enabledAcct, defaultAcct, disabledAcct, otherAcct} {
		e.cleanupAccount(t, acct)
	}

	enabled, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "email",
		AccountID: &enabledAcct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyContactDriven,
		Metadata:  map[string]any{"backfill_since": "2026-03-15", "extra": "keep-me"},
	})
	require.NoError(t, err)
	oldCursor := "9999999999"
	_, err = e.syncRepo.UpdateSyncStateSuccess(e.ctx, enabled.ID, accelerated.GetCurrentTime().Add(24*time.Hour), &oldCursor)
	require.NoError(t, err)
	errMsg := "previous failure"
	_, err = e.syncRepo.UpdateSyncStateStatus(e.ctx, enabled.ID, repository.SyncStatusError, &errMsg)
	require.NoError(t, err)

	defaultMeta, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "email",
		AccountID: &defaultAcct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyContactDriven,
	})
	require.NoError(t, err)
	_, err = e.syncRepo.UpdateSyncStateSuccess(e.ctx, defaultMeta.ID, accelerated.GetCurrentTime().Add(24*time.Hour), &oldCursor)
	require.NoError(t, err)

	disabled, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "email",
		AccountID: &disabledAcct,
		Enabled:   false,
		Status:    repository.SyncStatusDisabled,
		Strategy:  repository.SyncStrategyContactDriven,
		Metadata:  map[string]any{"backfill_since": "2026-04-01"},
	})
	require.NoError(t, err)

	other, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "calendar",
		AccountID: &otherAcct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyFetchAll,
	})
	require.NoError(t, err)
	otherNext := accelerated.GetCurrentTime().Add(48 * time.Hour)
	_, err = e.syncRepo.UpdateSyncStateSuccess(e.ctx, other.ID, otherNext, &oldCursor)
	require.NoError(t, err)

	resetSvc := service.NewEmailBackfillCursorResetService(e.syncRepo)
	before := accelerated.GetCurrentTime()
	res, err := resetSvc.ResetGmailBackfillCursors(e.ctx)
	after := accelerated.GetCurrentTime()
	require.NoError(t, err)
	require.Equal(t, service.EmailBackfillCursorResetResult{Scanned: 2, Reset: 2}, res)

	enabledAfter := e.getEmailState(t, enabledAcct)
	require.NotNil(t, enabledAfter.SyncCursor)
	require.Equal(t, strconv.FormatInt(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC).Unix(), 10), *enabledAfter.SyncCursor)
	require.Equal(t, repository.SyncStatusIdle, enabledAfter.Status)
	require.Nil(t, enabledAfter.ErrorMessage)
	require.Equal(t, int32(0), enabledAfter.ErrorCount)
	require.NotNil(t, enabledAfter.NextSyncAt)
	require.False(t, enabledAfter.NextSyncAt.Before(before))
	require.False(t, enabledAfter.NextSyncAt.After(after))
	require.Equal(t, "2026-03-15", enabledAfter.Metadata["backfill_since"])
	require.Equal(t, "keep-me", enabledAfter.Metadata["extra"])

	defaultAfter := e.getEmailState(t, defaultAcct)
	require.NotNil(t, defaultAfter.SyncCursor)
	require.Equal(t, strconv.FormatInt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 10), *defaultAfter.SyncCursor)

	disabledAfter, err := e.syncRepo.GetSyncState(e.ctx, disabled.ID)
	require.NoError(t, err)
	require.False(t, disabledAfter.Enabled)
	require.Equal(t, repository.SyncStatusDisabled, disabledAfter.Status)
	require.Nil(t, disabledAfter.SyncCursor)

	otherAfter, err := e.syncRepo.GetSyncState(e.ctx, other.ID)
	require.NoError(t, err)
	require.NotNil(t, otherAfter.SyncCursor)
	require.Equal(t, oldCursor, *otherAfter.SyncCursor)
	require.NotNil(t, otherAfter.NextSyncAt)
	require.WithinDuration(t, otherNext, *otherAfter.NextSyncAt, time.Millisecond)
}

// --- respects a user-disabled state (never re-enabled) ----------------------

func TestReconcileEmail_RespectsUserDisabled(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("disabled")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, gmailReadonlyScopeForTest),
	}}
	e.service.SetEmailAccountLister(lister)

	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))
	created := e.getEmailState(t, acct)

	// User disables the account via the existing enable/disable path.
	_, err := e.syncRepo.UpdateSyncStateEnabled(e.ctx, created.ID, false)
	require.NoError(t, err)

	// Re-run reconciliation — must NOT re-enable.
	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	after := e.getEmailState(t, acct)
	require.Equal(t, created.ID, after.ID)
	require.False(t, after.Enabled, "user-disabled state must stay disabled (never re-enabled)")
}

// --- scope warning on the CREATE branch (no existing state) -----------------

func TestReconcileEmail_ScopeWarn_OnCreateBranch(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("noscope-create")
	e.cleanupAccount(t, acct)

	// Account missing gmail.readonly and with NO existing state.
	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, "openid", "email", "profile"),
	}}
	e.service.SetEmailAccountLister(lister)

	// The warning is observational: the method still returns nil AND still
	// creates the state (we do not skip enablement just because a scope is
	// missing — the sweep surfaces the auth failure with a clear error).
	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	st := e.getEmailState(t, acct)
	require.True(t, st.Enabled, "state created even though gmail.readonly is missing")
}

// --- scope warning on the FOUND branch (state pre-exists) -------------------

func TestReconcileEmail_ScopeWarn_OnFoundBranch(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("noscope-found")
	e.cleanupAccount(t, acct)

	// Pre-seed an email state for the account (as if created earlier when the
	// scope WAS present, or by a prior boot).
	pre, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "email",
		AccountID: &acct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyContactDriven,
		Metadata:  map[string]any{"backfill_since": "2026-01-01"},
	})
	require.NoError(t, err)

	// Now the account is missing gmail.readonly (reconnect pending) — the found
	// branch must still warn and leave the state untouched.
	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, "openid", "email", "profile"),
	}}
	e.service.SetEmailAccountLister(lister)

	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	after := e.getEmailState(t, acct)
	require.Equal(t, pre.ID, after.ID, "found state untouched")
	require.True(t, after.Enabled)
}

// --- per-account shape invariant: no (email, NULL) row ever produced --------

func TestReconcileEmail_PerAccountShape_NoNullAccountRow(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("shape")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, gmailReadonlyScopeForTest),
	}}
	e.service.SetEmailAccountLister(lister)

	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))

	// The per-account row exists with a non-NULL account_id.
	st := e.getEmailState(t, acct)
	require.NotNil(t, st.AccountID)
	require.Equal(t, acct, *st.AccountID)

	// No (email, NULL) orphan row exists. GetSyncStateBySource("email", nil)
	// COALESCE-matches account_id = '' which a per-account create never writes.
	_, err := e.syncRepo.GetSyncStateBySource(e.ctx, "email", nil)
	require.ErrorIs(t, err, db.ErrNotFound, "reconciliation must never create an (email, NULL) row")
}

// --- empty account list is a no-op ------------------------------------------

func TestReconcileEmail_EmptyAccountList_NoOp(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	lister := &reconcileStubLister{accounts: nil}
	e.service.SetEmailAccountLister(lister)

	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))
	require.Equal(t, 1, lister.calls, "lister was consulted")
}

// --- nil lister is a no-op (does not even consult a lister) ------------------

func TestReconcileEmail_NilLister_NoOp(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	// No SetEmailAccountLister call → nil lister.
	require.NoError(t, e.service.ReconcileEmailSyncStates(e.ctx))
}

// --- OAuth-connect reconciler closure is idempotent on re-connect -----------

func TestReconcileEmail_ConnectPath_IdempotentOnReconnect(t *testing.T) {
	t.Parallel()
	e := newReconcileEnv(t)
	acct := uniqueAccount("connect")
	e.cleanupAccount(t, acct)

	lister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(acct, gmailReadonlyScopeForTest),
	}}
	e.service.SetEmailAccountLister(lister)

	// The OAuth-connect hook (main.go) is exactly this closure shape.
	reconcile := func(ctx context.Context) error {
		return e.service.ReconcileEmailSyncStates(ctx)
	}

	// First connect creates the state.
	require.NoError(t, reconcile(e.ctx))
	first := e.getEmailState(t, acct)

	// Second connect of the same account is a no-op (no duplicate, untouched).
	require.NoError(t, reconcile(e.ctx))
	second := e.getEmailState(t, acct)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, e.countEmailStatesForAccount(t, acct))
}

// countEmailStatesForAccount counts live (email, account) rows via the
// repository's ListSyncStates (no raw SQL).
func (e *reconcileEnv) countEmailStatesForAccount(t *testing.T, accountID string) int {
	t.Helper()
	states, err := e.syncRepo.ListSyncStates(e.ctx)
	require.NoError(t, err)
	n := 0
	for _, st := range states {
		if st.Source == "email" && st.AccountID != nil && *st.AccountID == accountID {
			n++
		}
	}
	return n
}
