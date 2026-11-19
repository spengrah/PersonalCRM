//go:build integration_testdb

// End-to-end coverage for crm-admin --set-email-own-domains and
// --set-email-own-addresses: each test parses a real command line, wires the
// production dependencies (buildProductionDeps) over an isolated PostgreSQL
// clone, runs the verb, and reads the email sync-state rows back. Domains and
// addresses are placeholders (repository PII rule).
package main

import (
	"bytes"
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ownListEnv is one isolated clone the verbs run against.
type ownListEnv struct {
	ctx      context.Context
	cfg      *config.Config
	database *db.Database
	syncRepo *repository.SyncRepository
}

func newOwnListEnv(t *testing.T) *ownListEnv {
	t.Helper()
	testsupport.RequireDatabase(t)
	ctx := t.Context()
	database, cfg := newJobsTestDB(t, ctx)
	return &ownListEnv{ctx: ctx, cfg: cfg, database: database, syncRepo: repository.NewSyncRepository(database.Queries)}
}

// emailState persists an enabled email sync state for account with metadata.
func (e *ownListEnv) emailState(t *testing.T, account string, metadata map[string]any) uuid.UUID {
	t.Helper()
	st, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    google.GmailSourceName,
		AccountID: &account,
		Enabled:   true,
		Metadata:  metadata,
	})
	require.NoError(t, err)
	return st.ID
}

// metadata reads the row's metadata back from PostgreSQL.
func (e *ownListEnv) metadata(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	st, err := e.syncRepo.GetSyncState(e.ctx, id)
	require.NoError(t, err)
	return st.Metadata
}

// runVerb parses args as crm-admin does, wires the production dependencies, lets
// wrap replace the verb's repository when non-nil, and runs the verb. It
// returns what the verb printed.
func (e *ownListEnv) runVerb(t *testing.T, wrap func(emailOwnListRepo) emailOwnListRepo, args ...string) (string, error) {
	t.Helper()
	opts, err := parseArgs(args)
	require.NoError(t, err)
	require.NoError(t, validateSubcommand(opts))
	deps, cleanup, err := buildProductionDeps(e.ctx, e.cfg, e.database, opts)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	out := &bytes.Buffer{}
	deps.stdout, deps.stderr = out, &bytes.Buffer{}
	if wrap != nil {
		deps.emailOwnLists = wrap(deps.emailOwnLists)
	}
	err = run(e.ctx, opts, deps)
	return out.String(), err
}

// TestSetEmailOwnAddresses_Integration_SetsNormalizesAndClears proves that
// --set-email-own-addresses stores the normalized addresses on the row, keeps
// every other metadata key, and given an empty string removes only its key.
func TestSetEmailOwnAddresses_Integration_SetsNormalizesAndClears(t *testing.T) {
	t.Parallel()
	e := newOwnListEnv(t)
	id := e.emailState(t, "acct-a@own-domain.example", map[string]any{
		"backfill_since":  "2026-01-01",
		"terminal_reason": "manual",
		"own_domains":     []string{"own-domain.example"},
	})

	out, err := e.runVerb(t, nil, "--set-email-own-addresses", " Me@Shared-Domain.Example , <other@second.example> ")
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"backfill_since":  "2026-01-01",
		"terminal_reason": "manual",
		"own_domains":     []any{"own-domain.example"},
		"own_addresses":   []any{"me@shared-domain.example", "other@second.example"},
	}, e.metadata(t, id))
	require.Contains(t, out, "addresses=me@shared-domain.example,other@second.example")

	t.Run("an entry that is not an address is rejected and nothing is written", func(t *testing.T) {
		_, err := e.runVerb(t, nil, "--set-email-own-addresses", "shared-domain.example")
		require.Error(t, err)
		require.Equal(t, []any{"me@shared-domain.example", "other@second.example"}, e.metadata(t, id)["own_addresses"])
	})

	t.Run("nonempty input that names no address is rejected and nothing is written", func(t *testing.T) {
		// An empty group parses as an address list holding no address; only an
		// empty argument clears the list.
		_, err := e.runVerb(t, nil, "--set-email-own-addresses", "undisclosed-recipients:;")
		require.Error(t, err)
		require.Equal(t, []any{"me@shared-domain.example", "other@second.example"}, e.metadata(t, id)["own_addresses"])
	})

	t.Run("an address whose quoted local part holds a comma is one address", func(t *testing.T) {
		out, err := e.runVerb(t, nil, "--set-email-own-addresses", `"user,alias"@shared-domain.example`)
		require.NoError(t, err)
		require.Equal(t, []any{"user,alias@shared-domain.example"}, e.metadata(t, id)["own_addresses"])
		require.Contains(t, out, "addresses=user,alias@shared-domain.example")
	})

	t.Run("an empty string clears only the own addresses", func(t *testing.T) {
		_, err := e.runVerb(t, nil, "--set-email-own-addresses=")
		require.NoError(t, err)
		require.Equal(t, map[string]any{
			"backfill_since":  "2026-01-01",
			"terminal_reason": "manual",
			"own_domains":     []any{"own-domain.example"},
		}, e.metadata(t, id))
	})
}

// TestSetEmailOwnDomains_Integration_WritesOwnDomains proves that
// --set-email-own-domains stores the normalized domains under own_domains,
// keeps every other metadata key, and given an empty string removes only its
// key.
func TestSetEmailOwnDomains_Integration_WritesOwnDomains(t *testing.T) {
	t.Parallel()
	e := newOwnListEnv(t)
	id := e.emailState(t, "acct-a@own-domain.example", map[string]any{
		"backfill_since": "2026-01-01",
		"own_addresses":  []string{"me@shared-domain.example"},
	})

	out, err := e.runVerb(t, nil, "--set-email-own-domains", " Own-Domain.Example , Second.Example ")
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"backfill_since": "2026-01-01",
		"own_addresses":  []any{"me@shared-domain.example"},
		"own_domains":    []any{"own-domain.example", "second.example"},
	}, e.metadata(t, id))
	require.Contains(t, out, "domains=own-domain.example,second.example")

	_, err = e.runVerb(t, nil, "--set-email-own-domains=")
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"backfill_since": "2026-01-01",
		"own_addresses":  []any{"me@shared-domain.example"},
	}, e.metadata(t, id))
}

// TestSetEmailOwnList_Integration_AccountSelection proves that both verbs
// select the target row the same way: an error with no email account, the one
// account without --account, an error naming every account and writing
// nothing with two, and the --account row, leaving the other row unchanged.
func TestSetEmailOwnList_Integration_AccountSelection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ flag, value, key string }{
		{"--set-email-own-domains", "own-domain.example", "own_domains"},
		{"--set-email-own-addresses", "me@shared-domain.example", "own_addresses"},
	} {
		t.Run(c.flag, func(t *testing.T) {
			t.Parallel()
			e := newOwnListEnv(t)
			acctA := "acct-a@own-domain.example"
			acctB := "acct-b@own-domain.example"
			untouched := map[string]any{"backfill_since": "2026-01-01"}

			_, err := e.runVerb(t, nil, c.flag, c.value)
			require.ErrorContains(t, err, "no email sync state", "zero email accounts")

			idA := e.emailState(t, acctA, untouched)
			_, err = e.runVerb(t, nil, c.flag, c.value)
			require.NoError(t, err)
			require.Equal(t, []any{c.value}, e.metadata(t, idA)[c.key], "the one account is the target")

			idB := e.emailState(t, acctB, untouched)
			_, err = e.runVerb(t, nil, c.flag, "")
			require.ErrorContains(t, err, acctA)
			require.ErrorContains(t, err, acctB)
			require.Equal(t, []any{c.value}, e.metadata(t, idA)[c.key], "two accounts without --account write nothing")
			require.Equal(t, untouched, e.metadata(t, idB))

			_, err = e.runVerb(t, nil, c.flag, c.value, "--account", acctB)
			require.NoError(t, err)
			require.Equal(t, []any{c.value}, e.metadata(t, idB)[c.key], "--account selects its row")

			_, err = e.runVerb(t, nil, c.flag, "", "--account", acctA)
			require.NoError(t, err)
			require.Equal(t, untouched, e.metadata(t, idA), "--account clears its row")
			require.Equal(t, []any{c.value}, e.metadata(t, idB)[c.key], "--account leaves the other row unchanged")
		})
	}
}

// listThenRun runs between the verb's read of the sync states and its write.
type listThenRun struct {
	emailOwnListRepo
	between func()
}

func (r listThenRun) ListSyncStates(ctx context.Context) ([]repository.SyncState, error) {
	states, err := r.emailOwnListRepo.ListSyncStates(ctx)
	r.between()
	return states, err
}

// TestSetEmailOwnList_Integration_OverlappingVerbsKeepBothKeys is the regression
// for two verbs that overlap: --set-email-own-addresses runs to completion
// between --set-email-own-domains' read of the row and its write, and the row
// keeps both lists.
func TestSetEmailOwnList_Integration_OverlappingVerbsKeepBothKeys(t *testing.T) {
	t.Parallel()
	e := newOwnListEnv(t)
	id := e.emailState(t, "acct-a@own-domain.example", map[string]any{"backfill_since": "2026-01-01"})

	_, err := e.runVerb(t, func(repo emailOwnListRepo) emailOwnListRepo {
		return listThenRun{emailOwnListRepo: repo, between: func() {
			_, err := e.runVerb(t, nil, "--set-email-own-addresses", "me@shared-domain.example")
			require.NoError(t, err)
		}}
	}, "--set-email-own-domains", "own-domain.example")
	require.NoError(t, err)

	require.Equal(t, map[string]any{
		"backfill_since": "2026-01-01",
		"own_domains":    []any{"own-domain.example"},
		"own_addresses":  []any{"me@shared-domain.example"},
	}, e.metadata(t, id))
}
