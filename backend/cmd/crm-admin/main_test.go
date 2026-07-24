// Tests for crm-admin flag dispatch + mutual exclusion.
//
// We avoid taking a database dependency by driving run() directly
// with hand-crafted runOptions + fake interface impls. parseArgs is
// covered separately so flag-shape regressions are caught.
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type fakeTokenMinter struct {
	token     string
	expiresAt time.Time
	err       error
	calls     int
}

func (f *fakeTokenMinter) CreatePairingToken(_ context.Context) (string, time.Time, error) {
	f.calls++
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return f.token, f.expiresAt, nil
}

type fakeHostLister struct {
	hosts []*repository.MacHost
	err   error
	calls int
}

func (f *fakeHostLister) ListActiveHosts(_ context.Context) ([]*repository.MacHost, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.hosts, nil
}

type fakeHostRevoker struct {
	revoked uuid.UUID
	err     error
	calls   int
}

func (f *fakeHostRevoker) RevokeHost(_ context.Context, id uuid.UUID) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.revoked = id
	return nil
}

type fakeRematchRunner struct {
	result *messages.RematchStrandedResult
	err    error
	calls  int
}

func (f *fakeRematchRunner) RematchStranded(_ context.Context) (*messages.RematchStrandedResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeGmailBackfillResetter struct {
	result service.EmailBackfillCursorResetResult
	err    error
	calls  int
}

func (f *fakeGmailBackfillResetter) ResetGmailBackfillCursors(_ context.Context) (service.EmailBackfillCursorResetResult, error) {
	f.calls++
	if f.err != nil {
		return service.EmailBackfillCursorResetResult{}, f.err
	}
	return f.result, nil
}

// fakeSeedRunner records which seed path ran (additive vs reset) and the params
// it received, so dispatch + flag-mapping can be asserted without a DB/harness.
// It returns `result` on BOTH paths, mirroring the real seedAdapter: a failed
// run carries its PARTIAL ProfileResult out alongside the error, and the
// entrypoints print that as a degraded summary.
type fakeSeedRunner struct {
	result     synthetic.ProfileResult
	err        error
	seedCalls  int
	resetCalls int
	lastParams synthetic.SeedParams
}

func (f *fakeSeedRunner) stamp(params synthetic.SeedParams) synthetic.ProfileResult {
	res := f.result
	res.Profile = params.Profile
	res.Namespace = params.Namespace
	res.Seed = params.Seed
	return res
}

func (f *fakeSeedRunner) Seed(_ context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error) {
	f.seedCalls++
	f.lastParams = params
	return f.stamp(params), f.err
}

func (f *fakeSeedRunner) ResetAndSeed(_ context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error) {
	f.resetCalls++
	f.lastParams = params
	return f.stamp(params), f.err
}

func newTestDeps() (adminDeps, *bytes.Buffer, *fakeTokenMinter, *fakeHostLister, *fakeHostRevoker, *fakeRematchRunner) {
	stdout := &bytes.Buffer{}
	tokens := &fakeTokenMinter{
		token:     "test-token-base64",
		expiresAt: time.Date(2026, 5, 13, 15, 42, 18, 0, time.UTC),
	}
	hosts := &fakeHostLister{}
	revoker := &fakeHostRevoker{}
	rematch := &fakeRematchRunner{result: &messages.RematchStrandedResult{}}
	gmailReset := &fakeGmailBackfillResetter{}
	return adminDeps{
		tokens:     tokens,
		hosts:      hosts,
		revoker:    revoker,
		rematch:    rematch,
		gmailReset: gmailReset,
		seed:       &fakeSeedRunner{},
		stdout:     stdout,
		stderr:     &bytes.Buffer{},
	}, stdout, tokens, hosts, revoker, rematch
}

func TestRunNoFlagsErrors(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{}, deps)
	if err == nil {
		t.Fatal("expected error on missing subcommand")
	}
	if !strings.Contains(err.Error(), "no subcommand specified") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMutualExclusionErrors(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{
		mintPairingToken: true,
		listHosts:        true,
	}, deps)
	if err == nil {
		t.Fatal("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMintPairingTokenHappy(t *testing.T) {
	deps, stdout, tokens, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{mintPairingToken: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens.calls != 1 {
		t.Fatalf("expected 1 token mint, got %d", tokens.calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "token=test-token-base64") {
		t.Fatalf("output missing token: %q", out)
	}
	if !strings.Contains(out, "expires_at=2026-05-13T15:42:18Z") {
		t.Fatalf("output missing expires_at: %q", out)
	}
}

func TestRunMintPairingTokenWithLabelEchoes(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{
		mintPairingToken: true,
		hostnameLabel:    "mac-1",
	}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "hostname_label=mac-1") {
		t.Fatalf("missing hostname label: %q", stdout.String())
	}
}

func TestRunMintPairingTokenError(t *testing.T) {
	deps, _, tokens, _, _, _ := newTestDeps()
	tokens.err = errors.New("kaboom")
	err := run(context.Background(), runOptions{mintPairingToken: true}, deps)
	if err == nil {
		t.Fatal("expected mint error")
	}
}

func TestRunListHostsEmpty(t *testing.T) {
	deps, stdout, _, hosts, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{listHosts: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hosts.calls != 1 {
		t.Fatalf("expected 1 list call, got %d", hosts.calls)
	}
	if !strings.Contains(stdout.String(), "no active paired hosts") {
		t.Fatalf("expected empty-list message, got %q", stdout.String())
	}
}

func TestRunListHostsPopulated(t *testing.T) {
	deps, stdout, _, hosts, _, _ := newTestDeps()
	id := uuid.New()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	heartbeat := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	hosts.hosts = []*repository.MacHost{
		{
			ID:              id,
			Hostname:        "mac-1",
			CreatedAt:       when,
			LastHeartbeatAt: &heartbeat,
		},
	}
	err := run(context.Background(), runOptions{listHosts: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "id="+id.String()) {
		t.Fatalf("missing id: %q", out)
	}
	if !strings.Contains(out, "hostname=mac-1") {
		t.Fatalf("missing hostname: %q", out)
	}
	if !strings.Contains(out, "last_heartbeat_at=2026-01-02T04:00:00Z") {
		t.Fatalf("missing heartbeat: %q", out)
	}
}

func TestRunListHostsNeverHeartbeat(t *testing.T) {
	deps, stdout, _, hosts, _, _ := newTestDeps()
	hosts.hosts = []*repository.MacHost{
		{ID: uuid.New(), Hostname: "mac-1", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	err := run(context.Background(), runOptions{listHosts: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "last_heartbeat_at=never") {
		t.Fatalf("expected 'never' heartbeat marker: %q", stdout.String())
	}
}

func TestRunListHostsError(t *testing.T) {
	deps, _, _, hosts, _, _ := newTestDeps()
	hosts.err = errors.New("db unreachable")
	err := run(context.Background(), runOptions{listHosts: true}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "list active hosts") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestRunRevokeHostMalformedUUID(t *testing.T) {
	deps, _, _, _, revoker, _ := newTestDeps()
	err := run(context.Background(), runOptions{revokeHostID: "not-a-uuid"}, deps)
	if err == nil {
		t.Fatal("expected uuid parse error")
	}
	if revoker.calls != 0 {
		t.Fatalf("revoke must not be called on malformed UUID; got %d calls", revoker.calls)
	}
}

func TestRunRevokeHostHappy(t *testing.T) {
	deps, stdout, _, _, revoker, _ := newTestDeps()
	id := uuid.New()
	err := run(context.Background(), runOptions{revokeHostID: id.String()}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revoker.revoked != id {
		t.Fatalf("expected revoked=%s, got %s", id, revoker.revoked)
	}
	if !strings.Contains(stdout.String(), "revoked host_id="+id.String()) {
		t.Fatalf("missing revoke confirmation: %q", stdout.String())
	}
}

func TestRunRevokeHostError(t *testing.T) {
	deps, _, _, _, revoker, _ := newTestDeps()
	revoker.err = errors.New("not found")
	err := run(context.Background(), runOptions{revokeHostID: uuid.New().String()}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunRotateHostKeyMalformedUUID(t *testing.T) {
	deps, _, tokens, hosts, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{rotateHostID: "not-a-uuid"}, deps)
	if err == nil {
		t.Fatal("expected uuid parse error")
	}
	if tokens.calls != 0 {
		t.Fatalf("token must not be minted on malformed UUID; got %d calls", tokens.calls)
	}
	if hosts.calls != 0 {
		t.Fatalf("host list must not be queried on malformed UUID; got %d calls", hosts.calls)
	}
}

func TestRunRotateHostKeyHostNotFound(t *testing.T) {
	deps, _, tokens, hosts, _, _ := newTestDeps()
	hosts.hosts = []*repository.MacHost{
		{ID: uuid.New(), Hostname: "other"},
	}
	err := run(context.Background(), runOptions{rotateHostID: uuid.New().String()}, deps)
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "no active host") {
		t.Fatalf("expected not-found message, got %v", err)
	}
	if tokens.calls != 0 {
		t.Fatalf("token must not be minted when host is missing; got %d calls", tokens.calls)
	}
}

func TestRunRotateHostKeyListError(t *testing.T) {
	deps, _, tokens, hosts, _, _ := newTestDeps()
	hosts.err = errors.New("db down")
	err := run(context.Background(), runOptions{rotateHostID: uuid.New().String()}, deps)
	if err == nil {
		t.Fatal("expected list error")
	}
	if tokens.calls != 0 {
		t.Fatalf("token must not be minted when list fails; got %d calls", tokens.calls)
	}
}

func TestRunRotateHostKeyHappy(t *testing.T) {
	deps, stdout, tokens, hosts, _, _ := newTestDeps()
	id := uuid.New()
	hosts.hosts = []*repository.MacHost{
		{ID: id, Hostname: "mac-1"},
	}
	err := run(context.Background(), runOptions{rotateHostID: id.String()}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens.calls != 1 {
		t.Fatalf("expected 1 mint, got %d", tokens.calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "token=test-token-base64") {
		t.Fatalf("missing token: %q", out)
	}
	if !strings.Contains(out, "crm-mac install --re-pair --pair test-token-base64") {
		t.Fatalf("missing templated re-pair command: %q", out)
	}
}

func TestRunRotateHostKeyTokenMintError(t *testing.T) {
	deps, _, tokens, hosts, _, _ := newTestDeps()
	id := uuid.New()
	hosts.hosts = []*repository.MacHost{{ID: id, Hostname: "mac-1"}}
	tokens.err = errors.New("rng failed")
	err := run(context.Background(), runOptions{rotateHostID: id.String()}, deps)
	if err == nil {
		t.Fatal("expected mint error")
	}
	if !strings.Contains(err.Error(), "mint pairing token") {
		t.Fatalf("expected wrapped mint error, got %v", err)
	}
}

func TestRunRematchStrandedDelegates(t *testing.T) {
	deps, stdout, _, _, _, rematch := newTestDeps()
	rematch.result = &messages.RematchStrandedResult{
		Scanned: 10, Matched: 3, StillStranded: 7, Enqueued: 3, Errors: 0,
	}
	err := run(context.Background(), runOptions{rematchStranded: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rematch.calls != 1 {
		t.Fatalf("expected 1 rematch call, got %d", rematch.calls)
	}
	out := stdout.String()
	for _, want := range []string{"scanned:        10", "matched:        3", "enqueued:       3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
}

func TestRunResetGmailBackfillCursorsCountsOnly(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	reset := &fakeGmailBackfillResetter{
		result: service.EmailBackfillCursorResetResult{Scanned: 3, Reset: 2},
	}
	deps.gmailReset = reset

	err := run(context.Background(), runOptions{resetGmailBackfill: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reset.calls != 1 {
		t.Fatalf("expected 1 reset call, got %d", reset.calls)
	}
	out := stdout.String()
	for _, want := range []string{"reset-gmail-backfill-cursors summary:", "scanned: 3", "reset:   2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
	for _, forbidden := range []string{"account_id", "sync_cursor", "example.com"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("output must be counts-only; found %q in %s", forbidden, out)
		}
	}
}

func TestRunResetGmailBackfillCursorsError(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.gmailReset = &fakeGmailBackfillResetter{err: errors.New("db down")}

	err := run(context.Background(), runOptions{resetGmailBackfill: true}, deps)
	if err == nil {
		t.Fatal("expected reset error")
	}
	if !strings.Contains(err.Error(), "reset gmail backfill cursors") {
		t.Fatalf("expected wrapped reset error, got %v", err)
	}
}

type fakeReconcileRunner struct {
	result service.ReconcileAllResult
	err    error
	calls  int
}

func (f *fakeReconcileRunner) ReconcileAllAddressBookMethods(_ context.Context) (service.ReconcileAllResult, error) {
	f.calls++
	if f.err != nil {
		return service.ReconcileAllResult{}, f.err
	}
	return f.result, nil
}

func TestRunReconcileAddressBookMethodsHappy(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	reconcile := &fakeReconcileRunner{result: service.ReconcileAllResult{
		Scanned: 20, MethodsAutoApplied: 13, SuggestionsRecorded: 7, Failed: 0,
	}}
	deps.reconcile = reconcile

	err := run(context.Background(), runOptions{reconcileAddressBook: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reconcile.calls != 1 {
		t.Fatalf("expected 1 reconcile call, got %d", reconcile.calls)
	}
	out := stdout.String()
	for _, want := range []string{"scanned:               20", "methods_auto_applied:  13", "suggestions_recorded:  7", "failed:                0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
}

func TestRunReconcileAddressBookMethodsFailedExitsNonZero(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	reconcile := &fakeReconcileRunner{result: service.ReconcileAllResult{
		Scanned: 5, MethodsAutoApplied: 2, SuggestionsRecorded: 1, Failed: 2,
	}}
	deps.reconcile = reconcile

	err := run(context.Background(), runOptions{reconcileAddressBook: true}, deps)
	if err == nil {
		t.Fatal("expected non-nil error when some rows failed (non-zero exit)")
	}
	// Summary still printed even on failure.
	if !strings.Contains(stdout.String(), "failed:                2") {
		t.Fatalf("expected summary printed with failed count, got %q", stdout.String())
	}
}

func TestRunReconcileAddressBookMethodsError(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.reconcile = &fakeReconcileRunner{err: errors.New("list failed")}

	err := run(context.Background(), runOptions{reconcileAddressBook: true}, deps)
	if err == nil {
		t.Fatal("expected error when reconcile runner errors")
	}
}

func TestRunReconcileMutualExclusion(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.reconcile = &fakeReconcileRunner{}
	err := run(context.Background(), runOptions{
		reconcileAddressBook: true,
		listHosts:            true,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

type fakeTagMigrator struct {
	result service.TagMigrationResult
	err    error
	calls  int
}

func (f *fakeTagMigrator) MigrateTags(_ context.Context) (service.TagMigrationResult, error) {
	f.calls++
	if f.err != nil {
		return service.TagMigrationResult{}, f.err
	}
	return f.result, nil
}

func TestRunMigrateTagsHappy(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	migrator := &fakeTagMigrator{result: service.TagMigrationResult{
		Tags: 4, TagNodesCreated: 3, TagNodesExisting: 1, ContactTags: 9, SkippedDeletedContacts: 2, AssertionsAsserted: 9,
	}}
	deps.migrateTags = migrator

	err := run(context.Background(), runOptions{migrateTags: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrator.calls != 1 {
		t.Fatalf("expected 1 migrate call, got %d", migrator.calls)
	}
	out := stdout.String()
	for _, want := range []string{
		"tags:", "tag_nodes_created:", "tag_nodes_existing:",
		"contact_tags_migrated:", "contact_tags_skipped_deleted:", "assertions_asserted:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing label %q: %s", want, out)
		}
	}
	// The skipped-soft-deleted count is surfaced explicitly (not silently dropped).
	if !strings.Contains(out, "contact_tags_skipped_deleted:  2") {
		t.Fatalf("output missing skipped-deleted count: %s", out)
	}
}

func TestRunMigrateTagsError(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.migrateTags = &fakeTagMigrator{err: service.ErrTagCaseCollision}

	err := run(context.Background(), runOptions{migrateTags: true}, deps)
	if err == nil {
		t.Fatal("expected error when the tag migrator errors (e.g. case collision)")
	}
	if !errors.Is(err, service.ErrTagCaseCollision) {
		t.Fatalf("expected wrapped ErrTagCaseCollision, got %v", err)
	}
}

func TestRunMigrateTagsMutualExclusion(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.migrateTags = &fakeTagMigrator{}
	err := run(context.Background(), runOptions{
		migrateTags: true,
		listHosts:   true,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

type fakeContactKnowledgeMigrator struct {
	result service.ContactKnowledgeMigrationResult
	err    error
	calls  int
}

func (f *fakeContactKnowledgeMigrator) MigrateContactKnowledgeColumns(_ context.Context) (service.ContactKnowledgeMigrationResult, error) {
	f.calls++
	if f.err != nil {
		return service.ContactKnowledgeMigrationResult{}, f.err
	}
	return f.result, nil
}

func TestRunMigrateContactKnowledgeHappy(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	migrator := &fakeContactKnowledgeMigrator{result: service.ContactKnowledgeMigrationResult{
		Contacts: 5, LocationsMigrated: 3, BirthdaysMigrated: 4, HowMetMigrated: 2,
	}}
	deps.migrateContactKnowledge = migrator

	err := run(context.Background(), runOptions{migrateContactKnowledge: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrator.calls != 1 {
		t.Fatalf("expected 1 migrate call, got %d", migrator.calls)
	}
	out := stdout.String()
	for _, want := range []string{
		"contacts_scanned:", "locations_migrated:", "birthdays_migrated:", "how_met_migrated:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing label %q: %s", want, out)
		}
	}
}

func TestRunMigrateContactKnowledgeError(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.migrateContactKnowledge = &fakeContactKnowledgeMigrator{err: errors.New("boom")}

	err := run(context.Background(), runOptions{migrateContactKnowledge: true}, deps)
	if err == nil {
		t.Fatal("expected error when the contact-knowledge migrator errors")
	}
}

func TestRunMigrateContactKnowledgeMutualExclusion(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.migrateContactKnowledge = &fakeContactKnowledgeMigrator{}
	err := run(context.Background(), runOptions{
		migrateContactKnowledge: true,
		migrateTags:             true,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestRunResetGmailBackfillMutualExclusion(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{
		resetGmailBackfill: true,
		rematchStranded:    true,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestParseArgsAllFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		validate func(t *testing.T, o runOptions)
	}{
		{
			"mint with label",
			[]string{"--mint-pairing-token", "--hostname-label", "mac-1"},
			func(t *testing.T, o runOptions) {
				if !o.mintPairingToken {
					t.Fatal("mint flag not set")
				}
				if o.hostnameLabel != "mac-1" {
					t.Fatalf("expected label mac-1, got %q", o.hostnameLabel)
				}
			},
		},
		{
			"list-hosts",
			[]string{"--list-hosts"},
			func(t *testing.T, o runOptions) {
				if !o.listHosts {
					t.Fatal("list-hosts flag not set")
				}
			},
		},
		{
			"revoke-host",
			[]string{"--revoke-host", "11111111-2222-3333-4444-555555555555"},
			func(t *testing.T, o runOptions) {
				if o.revokeHostID != "11111111-2222-3333-4444-555555555555" {
					t.Fatalf("expected revoke id, got %q", o.revokeHostID)
				}
			},
		},
		{
			"rematch",
			[]string{"--messages-rematch-stranded"},
			func(t *testing.T, o runOptions) {
				if !o.rematchStranded {
					t.Fatal("rematch flag not set")
				}
			},
		},
		{
			"reconcile-address-book-methods",
			[]string{"--reconcile-address-book-methods"},
			func(t *testing.T, o runOptions) {
				if !o.reconcileAddressBook {
					t.Fatal("reconcile-address-book-methods flag not set")
				}
			},
		},
		{
			"rotate-host-key",
			[]string{"--rotate-host-key", "11111111-2222-3333-4444-555555555555"},
			func(t *testing.T, o runOptions) {
				if o.rotateHostID != "11111111-2222-3333-4444-555555555555" {
					t.Fatalf("expected rotate id, got %q", o.rotateHostID)
				}
			},
		},
		{
			"rederive-correspondence-names",
			[]string{"--rederive-correspondence-names"},
			func(t *testing.T, o runOptions) {
				if !o.rederiveCorrespondence {
					t.Fatal("rederive-correspondence-names flag not set")
				}
			},
		},
		{
			"reset-gmail-backfill-cursors",
			[]string{"--reset-gmail-backfill-cursors"},
			func(t *testing.T, o runOptions) {
				if !o.resetGmailBackfill {
					t.Fatal("reset-gmail-backfill-cursors flag not set")
				}
			},
		},
		{
			// --seed (bool subcommand) and --prng-seed (uint64) are DISTINCT
			// flags — Go's flag cannot bind one name to both.
			"seed with distinct prng-seed + profile + yes",
			[]string{"--seed", "--profile", "dev", "--prng-seed", "42", "--namespace", "ns1", "--yes"},
			func(t *testing.T, o runOptions) {
				if !o.doSeed {
					t.Fatal("--seed bool not set")
				}
				if o.prngSeed != 42 {
					t.Fatalf("expected prng-seed 42, got %d", o.prngSeed)
				}
				if o.seedProfile != "dev" {
					t.Fatalf("expected profile dev, got %q", o.seedProfile)
				}
				if o.seedNamespace != "ns1" {
					t.Fatalf("expected namespace ns1, got %q", o.seedNamespace)
				}
				if !o.seedYes {
					t.Fatal("--yes not set")
				}
			},
		},
		{
			"reset-and-seed",
			[]string{"--reset-and-seed", "--yes"},
			func(t *testing.T, o runOptions) {
				if !o.resetAndSeed {
					t.Fatal("--reset-and-seed not set")
				}
				if !o.seedYes {
					t.Fatal("--yes not set")
				}
			},
		},
		{
			"migrate",
			[]string{"--migrate"},
			func(t *testing.T, o runOptions) {
				if !o.migrate {
					t.Fatal("--migrate not set")
				}
				if o.migrateCheck {
					t.Fatal("--migrate-check should not be set")
				}
			},
		},
		{
			"migrate-check",
			[]string{"--migrate-check"},
			func(t *testing.T, o runOptions) {
				if !o.migrateCheck {
					t.Fatal("--migrate-check not set")
				}
				if o.migrate {
					t.Fatal("--migrate should not be set")
				}
			},
		},
		{
			"migrate-tags",
			[]string{"--migrate-tags"},
			func(t *testing.T, o runOptions) {
				if !o.migrateTags {
					t.Fatal("--migrate-tags not set")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, err := parseArgs(c.args)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			c.validate(t, opts)
		})
	}
}

// --- --rederive-correspondence-names dispatch tests ---

type fakeRederiveRunner struct {
	result google.CorrespondenceRederiveResult
	err    error
	since  time.Time
	log    *[]string
}

func (f *fakeRederiveRunner) RederiveNames(_ context.Context, since time.Time) (google.CorrespondenceRederiveResult, error) {
	f.since = since
	if f.log != nil {
		*f.log = append(*f.log, "rederive")
	}
	if f.err != nil {
		return google.CorrespondenceRederiveResult{}, f.err
	}
	return f.result, nil
}

func TestRunRederiveCorrespondenceNamesHappy(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	log := &[]string{}
	rederive := &fakeRederiveRunner{
		result: google.CorrespondenceRederiveResult{
			Scanned: 12, Rederived: 9, SkippedNoGmailID: 2, SkippedUnavailable: 1, Failed: 0,
		},
		log: log,
	}
	deps.rederive = rederive

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the re-derive phase runs now; candidate discovery moved in-sync.
	if len(*log) != 1 || (*log)[0] != "rederive" {
		t.Fatalf("expected [rederive], got %v", *log)
	}
	if rederive.since.Format("2006-01-02") != correspondenceBackfillFloor {
		t.Fatalf("expected since=%s, got %v", correspondenceBackfillFloor, rederive.since)
	}
	out := stdout.String()
	for _, want := range []string{
		"scanned:              12",
		"rederived:            9",
		"skipped_no_gmail_id:  2",
		"skipped_unavailable:  1",
		"failed:               0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
	// The retired producer pass must no longer be summarized.
	if strings.Contains(out, "candidates_upserted") {
		t.Fatalf("candidates_upserted must be gone from the summary: %s", out)
	}
}

func TestRunRederiveCorrespondenceNamesFailedExitsNonZero(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	log := &[]string{}
	deps.rederive = &fakeRederiveRunner{
		result: google.CorrespondenceRederiveResult{Scanned: 5, Rederived: 3, Failed: 2},
		log:    log,
	}

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err == nil {
		t.Fatal("expected non-nil error when some rows failed")
	}
	if !strings.Contains(stdout.String(), "failed:               2") {
		t.Fatalf("expected summary printed with failed count, got %q", stdout.String())
	}
}

func TestRunRederiveCorrespondenceNamesRederiveErrorFails(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	log := &[]string{}
	deps.rederive = &fakeRederiveRunner{err: errors.New("list failed"), log: log}

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err == nil {
		t.Fatal("expected error when the re-derive phase hard-errors")
	}
	if len(*log) != 1 || (*log)[0] != "rederive" {
		t.Fatalf("expected only the re-derive phase to run, log=%v", *log)
	}
}

// --- --seed / --reset-and-seed dispatch tests ---

func TestRunSeedDispatchesAdditiveWithDefaults(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	// --yes confirms; default profile for --seed is `dev`.
	err := run(context.Background(), runOptions{doSeed: true, seedYes: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.seedCalls != 1 || seed.resetCalls != 0 {
		t.Fatalf("expected exactly one additive Seed call, got seed=%d reset=%d", seed.seedCalls, seed.resetCalls)
	}
	if seed.lastParams.Profile != synthetic.ProfileDev {
		t.Fatalf("expected default --seed profile dev, got %q", seed.lastParams.Profile)
	}
	out := stdout.String()
	if !strings.Contains(out, "seed summary (profile=dev") {
		t.Fatalf("expected counts-only summary, got %q", out)
	}
}

func TestRunResetAndSeedDispatchesWithDefaults(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	err := run(context.Background(), runOptions{resetAndSeed: true, seedYes: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.resetCalls != 1 || seed.seedCalls != 0 {
		t.Fatalf("expected exactly one ResetAndSeed call, got seed=%d reset=%d", seed.seedCalls, seed.resetCalls)
	}
	if seed.lastParams.Profile != synthetic.ProfileProdShaped {
		t.Fatalf("expected default --reset-and-seed profile prod-shaped, got %q", seed.lastParams.Profile)
	}
	if !strings.Contains(stdout.String(), "seed summary (profile=prod-shaped") {
		t.Fatalf("expected counts-only summary, got %q", stdout.String())
	}
}

func TestRunSeedRequiresConfirm(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	// No --yes and no CRM_SEED_RESET_CONFIRM → refuse, do NOT run the seed.
	t.Setenv(seedConfirmEnv, "")
	err := run(context.Background(), runOptions{doSeed: true}, deps)
	if err == nil {
		t.Fatal("expected --seed to refuse without confirmation")
	}
	if seed.seedCalls != 0 {
		t.Fatalf("seed must not run without confirmation; got %d calls", seed.seedCalls)
	}
}

func TestRunResetAndSeedRequiresConfirm(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	t.Setenv(seedConfirmEnv, "")
	err := run(context.Background(), runOptions{resetAndSeed: true}, deps)
	if err == nil {
		t.Fatal("expected --reset-and-seed to refuse without confirmation")
	}
	if seed.resetCalls != 0 {
		t.Fatalf("reset-and-seed must not run without confirmation; got %d calls", seed.resetCalls)
	}
}

func TestRunSeedConfirmViaEnv(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	// The env-var alternative to --yes.
	t.Setenv(seedConfirmEnv, "1")
	err := run(context.Background(), runOptions{doSeed: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.seedCalls != 1 {
		t.Fatalf("expected seed to run with CRM_SEED_RESET_CONFIRM=1; got %d calls", seed.seedCalls)
	}
}

func TestRunSeedProfileOverride(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true, seedProfile: "minimal-scoped"}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.lastParams.Profile != synthetic.ProfileMinimalScoped {
		t.Fatalf("expected overridden profile minimal-scoped, got %q", seed.lastParams.Profile)
	}
}

func TestRunSeedUnknownProfileErrors(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true, seedProfile: "nope"}, deps)
	if err == nil {
		t.Fatal("expected unknown-profile error")
	}
	if seed.seedCalls != 0 {
		t.Fatalf("seed must not run on an unknown profile; got %d calls", seed.seedCalls)
	}
}

// TestRunResetAndSeedInvalidNamespaceRefusesBeforeDispatch guards the
// destructive-ordering contract: an invalid --namespace must be rejected BEFORE
// the seed runner is dispatched. ResetSyntheticData (the hard TRUNCATE of every
// live data table) lives inside seedAdapter.ResetAndSeed, so asserting
// resetCalls == 0 proves the truncate never ran. This test FAILS if validation
// is moved back into runProfile (after the truncate).
func TestRunResetAndSeedInvalidNamespaceRefusesBeforeDispatch(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	// `bad_ns` contains an underscore — a SQL LIKE metacharacter outside the
	// safe [a-z0-9-] charset.
	err := run(context.Background(), runOptions{resetAndSeed: true, seedYes: true, seedNamespace: "bad_ns"}, deps)
	if err == nil {
		t.Fatal("expected invalid-namespace error")
	}
	if seed.resetCalls != 0 {
		t.Fatalf("reset-and-seed must not run on an invalid namespace; got %d calls (the DB would be wiped before the late rejection)", seed.resetCalls)
	}
}

// TestRunSeedInvalidNamespaceRefusesBeforeDispatch is the additive-path
// counterpart: --seed must also fail fast on an invalid namespace rather than
// defer the rejection to harness construction.
func TestRunSeedInvalidNamespaceRefusesBeforeDispatch(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true, seedNamespace: "bad_ns"}, deps)
	if err == nil {
		t.Fatal("expected invalid-namespace error")
	}
	if seed.seedCalls != 0 {
		t.Fatalf("seed must not run on an invalid namespace; got %d calls", seed.seedCalls)
	}
}

func TestRunSeedNamespaceAndPRNGOverride(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	seed := &fakeSeedRunner{}
	deps.seed = seed

	err := run(context.Background(), runOptions{
		doSeed: true, seedYes: true, seedNamespace: "myworld", prngSeed: 99,
	}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.lastParams.Namespace != "myworld" {
		t.Fatalf("expected namespace override myworld, got %q", seed.lastParams.Namespace)
	}
	if seed.lastParams.Seed != 99 {
		t.Fatalf("expected prng-seed override 99, got %d", seed.lastParams.Seed)
	}
}

func TestRunSeedPropagatesError(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.seed = &fakeSeedRunner{err: errors.New("queue not drained")}

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true}, deps)
	if err == nil {
		t.Fatal("expected the seed error to propagate")
	}
	if !strings.Contains(err.Error(), "seed") {
		t.Fatalf("expected wrapped seed error, got %v", err)
	}
}

func TestRunSeedMutualExclusion(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	err := run(context.Background(), runOptions{doSeed: true, resetAndSeed: true, seedYes: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

// --- --migrate / --migrate-check tests ---

// TestValidateSubcommandMigrateSole confirms each migration flag is a valid sole
// subcommand.
func TestValidateSubcommandMigrateSole(t *testing.T) {
	if err := validateSubcommand(runOptions{migrate: true}); err != nil {
		t.Fatalf("--migrate alone should validate, got %v", err)
	}
	if err := validateSubcommand(runOptions{migrateCheck: true}); err != nil {
		t.Fatalf("--migrate-check alone should validate, got %v", err)
	}
}

// TestValidateSubcommandMigrateMutualExclusion confirms the migration flags are
// mutually exclusive with each other and with other subcommands.
func TestValidateSubcommandMigrateMutualExclusion(t *testing.T) {
	cases := []struct {
		name string
		opts runOptions
	}{
		{"migrate+migrate-check", runOptions{migrate: true, migrateCheck: true}},
		{"migrate+list-hosts", runOptions{migrate: true, listHosts: true}},
		{"migrate-check+seed", runOptions{migrateCheck: true, doSeed: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSubcommand(c.opts)
			if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("expected mutual-exclusion error, got %v", err)
			}
		})
	}
}

// TestRunMigrateThroughDispatcherGuards confirms run() refuses the migration
// subcommands (they are dispatched pre-DB in runMain, never through run()).
func TestRunMigrateThroughDispatcherGuards(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	for _, opts := range []runOptions{{migrate: true}, {migrateCheck: true}} {
		err := run(context.Background(), opts, deps)
		if err == nil || !strings.Contains(err.Error(), "dispatched pre-DB") {
			t.Fatalf("expected pre-DB dispatch guard error, got %v", err)
		}
	}
}

// TestRunMigrateCheckUpToDate: no migrations pending → nil (exit 0) + summary.
func TestRunMigrateCheckUpToDate(t *testing.T) {
	stdout := &bytes.Buffer{}
	status := func(_ context.Context, _, _ string) (bool, bool, error) {
		return false, false, nil
	}
	err := runMigrateCheckWith(context.Background(), "url", "path", stdout, status)
	if err != nil {
		t.Fatalf("expected nil (up-to-date), got %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "app_pending=0 river_pending=0") {
		t.Fatalf("unexpected summary: %q", got)
	}
}

// TestRunMigrateCheckPendingExit2: app and/or River pending → exitErr{code:2}.
func TestRunMigrateCheckPendingExit2(t *testing.T) {
	cases := []struct {
		name           string
		app, river     bool
		wantAppSummary string
	}{
		{"app only", true, false, "app_pending=1 river_pending=0"},
		{"river only", false, true, "app_pending=0 river_pending=1"},
		{"both", true, true, "app_pending=1 river_pending=1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			status := func(_ context.Context, _, _ string) (bool, bool, error) {
				return c.app, c.river, nil
			}
			err := runMigrateCheckWith(context.Background(), "url", "path", stdout, status)
			var ee exitErr
			if !errors.As(err, &ee) {
				t.Fatalf("expected exitErr, got %v", err)
			}
			if ee.code != migrateExitPending {
				t.Fatalf("expected exit code %d, got %d", migrateExitPending, ee.code)
			}
			if got := stdout.String(); !strings.Contains(got, c.wantAppSummary) {
				t.Fatalf("unexpected summary: %q", got)
			}
		})
	}
}

// TestRunMigrateCheckErrorExit1: an operational error from the status reporter
// surfaces as a plain error (→ exit 1), NOT an exitErr{code:2}.
func TestRunMigrateCheckErrorExit1(t *testing.T) {
	stdout := &bytes.Buffer{}
	status := func(_ context.Context, _, _ string) (bool, bool, error) {
		return false, false, errors.New("dirty migration state")
	}
	err := runMigrateCheckWith(context.Background(), "url", "path", stdout, status)
	if err == nil {
		t.Fatal("expected an operational error")
	}
	var ee exitErr
	if errors.As(err, &ee) {
		t.Fatalf("operational error must NOT be an exitErr (would map to exit 2), got code %d", ee.code)
	}
	if !strings.Contains(err.Error(), "dirty migration state") {
		t.Fatalf("expected wrapped status error, got %v", err)
	}
}

// TestExitErrMapping confirms main()'s errors.As mapping contract: an
// exitErr propagates its code, while a plain error does not match. (main() itself
// calls os.Exit, so we assert the classification run() relies on rather than
// spawning a subprocess.)
func TestExitErrMapping(t *testing.T) {
	var ee exitErr
	if !errors.As(exitErr{code: migrateExitPending, msg: "migrations pending"}, &ee) {
		t.Fatal("exitErr should match errors.As(exitErr)")
	}
	if ee.code != migrateExitPending {
		t.Fatalf("expected propagated code %d, got %d", migrateExitPending, ee.code)
	}
	if errors.As(errors.New("plain"), &ee) {
		t.Fatal("a plain error must NOT match errors.As(exitErr) — it keeps exit-1 behavior")
	}
}

// --- --list-jobs / --retry-job dispatch tests ---

// fakeRiverJobAdmin records the JobList params it received and returns canned
// jobs/errors, so list/retry dispatch + output formatting can be asserted
// without a real River client. The opaque JobListParams can't be introspected,
// so --job-state / --job-limit propagation is exercised by the integration
// test; here we assert behavior and output.
type fakeRiverJobAdmin struct {
	listResult *river.JobListResult
	listErr    error
	listCalls  int

	retryResult *rivertype.JobRow
	retryErr    error
	retryID     int64
	retryCalls  int
}

func (f *fakeRiverJobAdmin) JobList(_ context.Context, _ *river.JobListParams) (*river.JobListResult, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeRiverJobAdmin) JobRetry(_ context.Context, id int64) (*rivertype.JobRow, error) {
	f.retryCalls++
	f.retryID = id
	if f.retryErr != nil {
		return nil, f.retryErr
	}
	return f.retryResult, nil
}

func depsWithJobs(jobs riverJobAdmin) (adminDeps, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	return adminDeps{jobs: jobs, stdout: stdout, stderr: &bytes.Buffer{}}, stdout
}

func TestParseArgsListAndRetryFlags(t *testing.T) {
	t.Run("list-jobs with state + limit", func(t *testing.T) {
		opts, err := parseArgs([]string{"--list-jobs", "--job-state", "discarded", "--job-limit", "5"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !opts.listJobs {
			t.Fatal("--list-jobs not set")
		}
		if opts.jobState != "discarded" {
			t.Fatalf("expected job-state discarded, got %q", opts.jobState)
		}
		if opts.jobLimit != 5 {
			t.Fatalf("expected job-limit 5, got %d", opts.jobLimit)
		}
	})

	t.Run("list-jobs defaults", func(t *testing.T) {
		opts, err := parseArgs([]string{"--list-jobs"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if opts.jobState != "discarded,retryable" {
			t.Fatalf("expected default state filter, got %q", opts.jobState)
		}
		if opts.jobLimit != 100 {
			t.Fatalf("expected default limit 100, got %d", opts.jobLimit)
		}
	})

	t.Run("retry-job id", func(t *testing.T) {
		opts, err := parseArgs([]string{"--retry-job", "412"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if opts.retryJobID != 412 {
			t.Fatalf("expected retry id 412, got %d", opts.retryJobID)
		}
	})
}

func TestListAndRetryMutualExclusion(t *testing.T) {
	// Both selected → mutual-exclusion error.
	if err := validateSubcommand(runOptions{listJobs: true, retryJobID: 7}); err == nil {
		t.Fatal("expected mutual-exclusion error for --list-jobs + --retry-job")
	}
	// --retry-job 0 = unset, so only --list-jobs is active.
	if err := validateSubcommand(runOptions{listJobs: true, retryJobID: 0}); err != nil {
		t.Fatalf("--list-jobs alone should be valid, got %v", err)
	}
	// --retry-job alone is valid.
	if err := validateSubcommand(runOptions{retryJobID: 7}); err != nil {
		t.Fatalf("--retry-job alone should be valid, got %v", err)
	}
	// Neither → no-subcommand error.
	if err := validateSubcommand(runOptions{}); err == nil {
		t.Fatal("expected no-subcommand error")
	}
}

func TestRunListJobsHappy(t *testing.T) {
	finalized := time.Date(2026, 6, 12, 3, 4, 5, 0, time.UTC)
	created := time.Date(2026, 6, 12, 1, 2, 3, 0, time.UTC)
	jobs := &fakeRiverJobAdmin{
		listResult: &river.JobListResult{
			Jobs: []*rivertype.JobRow{
				{
					ID:          412,
					Kind:        "todoist_followup_create",
					State:       rivertype.JobStateDiscarded,
					Attempt:     10,
					MaxAttempts: 10,
					Queue:       "default",
					CreatedAt:   created,
					FinalizedAt: &finalized,
					EncodedArgs: []byte(`{"contact_task_id":"00000000-0000-0000-0000-000000000001"}`),
					Errors: []rivertype.AttemptError{
						{Attempt: 9, Error: "first failure"},
						{Attempt: 10, Error: "POST /tasks: 503\nsecond line"},
					},
				},
				{
					ID:          413,
					Kind:        "interaction_recorder",
					State:       rivertype.JobStateRetryable,
					Attempt:     1,
					MaxAttempts: 5,
					Queue:       "default",
					CreatedAt:   created,
					FinalizedAt: nil,
					EncodedArgs: []byte(`{"event_id":"00000000-0000-0000-0000-000000000002"}`),
				},
			},
		},
	}
	deps, stdout := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{listJobs: true, jobState: "discarded,retryable", jobLimit: 100}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobs.listCalls != 1 {
		t.Fatalf("expected 1 JobList call, got %d", jobs.listCalls)
	}
	out := stdout.String()
	// Row 1: discarded, exhausted, finalized, multi-line last error quoted.
	if !strings.Contains(out, "id=412 kind=todoist_followup_create state=discarded attempt=10/10") {
		t.Fatalf("missing discarded row prefix: %q", out)
	}
	if !strings.Contains(out, "finalized_at=2026-06-12T03:04:05Z") {
		t.Fatalf("missing finalized_at: %q", out)
	}
	// %q keeps the multi-line error on one line as an escaped string.
	if !strings.Contains(out, `last_error="POST /tasks: 503\nsecond line"`) {
		t.Fatalf("expected quoted multi-line last_error, got %q", out)
	}
	if !strings.Contains(out, `args={"contact_task_id":"00000000-0000-0000-0000-000000000001"}`) {
		t.Fatalf("missing args JSON: %q", out)
	}
	// Row 2: retryable, no finalized_at.
	if !strings.Contains(out, "id=413 kind=interaction_recorder state=retryable attempt=1/5") {
		t.Fatalf("missing retryable row prefix: %q", out)
	}
	if !strings.Contains(out, "finalized_at=none") {
		t.Fatalf("expected finalized_at=none for retryable row: %q", out)
	}
	// last_error empty for the row with no Errors.
	if !strings.Contains(out, `last_error=""`) {
		t.Fatalf("expected empty last_error for no-error row: %q", out)
	}
}

func TestRunListJobsEmpty(t *testing.T) {
	jobs := &fakeRiverJobAdmin{listResult: &river.JobListResult{Jobs: nil}}
	deps, stdout := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{listJobs: true, jobState: "discarded,retryable", jobLimit: 100}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "no jobs found (states=discarded,retryable)") {
		t.Fatalf("expected empty-result message, got %q", stdout.String())
	}
}

func TestRunListJobsLimitReachedNote(t *testing.T) {
	// Fake returns exactly limit rows → limit-reached note fires.
	rows := make([]*rivertype.JobRow, 2)
	for i := range rows {
		rows[i] = &rivertype.JobRow{
			ID: int64(500 + i), Kind: "interaction_recorder", State: rivertype.JobStateDiscarded,
			Attempt: 3, MaxAttempts: 3, Queue: "default",
			CreatedAt: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		}
	}
	jobs := &fakeRiverJobAdmin{listResult: &river.JobListResult{Jobs: rows}}
	deps, stdout := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{listJobs: true, jobState: "discarded", jobLimit: 2}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "note: limit reached (2 rows shown)") {
		t.Fatalf("expected limit-reached note, got %q", stdout.String())
	}
}

func TestRunListJobsInvalidState(t *testing.T) {
	jobs := &fakeRiverJobAdmin{}
	deps, _ := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{listJobs: true, jobState: "bogus", jobLimit: 100}, deps)
	if err == nil {
		t.Fatal("expected unknown-state error")
	}
	if !strings.Contains(err.Error(), "unknown state") || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("expected error naming valid states, got %v", err)
	}
	if jobs.listCalls != 0 {
		t.Fatalf("JobList must not be called on invalid state; got %d", jobs.listCalls)
	}
}

func TestRunListJobsInvalidLimit(t *testing.T) {
	for _, lim := range []int{0, 10001} {
		jobs := &fakeRiverJobAdmin{}
		deps, _ := depsWithJobs(jobs)
		err := run(context.Background(), runOptions{listJobs: true, jobState: "discarded", jobLimit: lim}, deps)
		if err == nil {
			t.Fatalf("expected limit error for --job-limit %d", lim)
		}
		if !strings.Contains(err.Error(), "--job-limit must be between 1 and 10000") {
			t.Fatalf("expected limit-range error, got %v", err)
		}
		if jobs.listCalls != 0 {
			t.Fatalf("JobList must not be called on invalid limit; got %d", jobs.listCalls)
		}
	}
}

func TestRunListJobsError(t *testing.T) {
	jobs := &fakeRiverJobAdmin{listErr: errors.New("db unreachable")}
	deps, _ := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{listJobs: true, jobState: "discarded", jobLimit: 100}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "list jobs") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestRunRetryJobHappy(t *testing.T) {
	scheduled := time.Date(2026, 6, 12, 5, 0, 0, 0, time.UTC)
	jobs := &fakeRiverJobAdmin{
		retryResult: &rivertype.JobRow{
			ID: 412, Kind: "todoist_followup_create", State: rivertype.JobStateAvailable,
			ScheduledAt: scheduled,
		},
	}
	deps, stdout := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{retryJobID: 412}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobs.retryCalls != 1 || jobs.retryID != 412 {
		t.Fatalf("expected 1 JobRetry(412) call, got calls=%d id=%d", jobs.retryCalls, jobs.retryID)
	}
	out := stdout.String()
	if !strings.Contains(out, "retried job id=412 kind=todoist_followup_create state=available") {
		t.Fatalf("missing retry confirmation: %q", out)
	}
	if !strings.Contains(out, "scheduled_at=2026-06-12T05:00:00Z") {
		t.Fatalf("missing scheduled_at: %q", out)
	}
}

func TestRunRetryJobNotFound(t *testing.T) {
	jobs := &fakeRiverJobAdmin{retryErr: rivertype.ErrNotFound}
	deps, _ := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{retryJobID: 999999999}, deps)
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "no job with id 999999999") {
		t.Fatalf("expected friendly not-found error naming the id, got %v", err)
	}
}

func TestRunRetryJobInvalidID(t *testing.T) {
	jobs := &fakeRiverJobAdmin{}
	deps, _ := depsWithJobs(jobs)
	// A negative id counts as active in validateSubcommand but is rejected by
	// the runner before calling JobRetry.
	err := run(context.Background(), runOptions{retryJobID: -1}, deps)
	if err == nil {
		t.Fatal("expected invalid-id error")
	}
	if !strings.Contains(err.Error(), "--retry-job must be a positive job id") {
		t.Fatalf("expected positive-id error, got %v", err)
	}
	if jobs.retryCalls != 0 {
		t.Fatalf("JobRetry must not be called on invalid id; got %d", jobs.retryCalls)
	}
}

func TestRunRetryJobError(t *testing.T) {
	jobs := &fakeRiverJobAdmin{retryErr: errors.New("connection reset")}
	deps, _ := depsWithJobs(jobs)
	err := run(context.Background(), runOptions{retryJobID: 412}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "retry job 412") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

// fakeTier0Reader records the cutoff it was called with and returns canned rows.
type fakeTier0Reader struct {
	rows       []repository.Tier0Row
	err        error
	calls      int
	lastCutoff time.Time
}

func (f *fakeTier0Reader) Tier0StatsByKind(_ context.Context, cutoff time.Time) ([]repository.Tier0Row, error) {
	f.calls++
	f.lastCutoff = cutoff
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestParseArgsRiverTier0Flags(t *testing.T) {
	t.Run("default window-hours", func(t *testing.T) {
		opts, err := parseArgs([]string{"--river-tier0"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !opts.riverTier0 {
			t.Fatal("--river-tier0 not set")
		}
		if opts.windowHours != 24 {
			t.Fatalf("expected default window-hours 24, got %d", opts.windowHours)
		}
	})

	t.Run("window-hours override", func(t *testing.T) {
		opts, err := parseArgs([]string{"--river-tier0", "--window-hours", "72"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if opts.windowHours != 72 {
			t.Fatalf("expected window-hours 72, got %d", opts.windowHours)
		}
	})
}

func TestRunRiverTier0DispatchesAndFormats(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	tier0 := &fakeTier0Reader{rows: []repository.Tier0Row{
		{Kind: "gcal_sync", N: 5, P50WaitS: 0.250, P50RunS: 1.500},
		{Kind: "cadence_updater", N: 12, P50WaitS: 0.010, P50RunS: 0.020},
	}}
	deps.tier0 = tier0

	before := accelerated.GetCurrentTime()
	err := run(context.Background(), runOptions{riverTier0: true, windowHours: 24}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tier0.calls != 1 {
		t.Fatalf("expected 1 tier0 call, got %d", tier0.calls)
	}
	// Cutoff is ~24h before now (accelerated time == wall time in tests).
	wantCutoff := before.Add(-24 * time.Hour)
	if diff := tier0.lastCutoff.Sub(wantCutoff); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("cutoff not ~24h before now: got %s, want ~%s", tier0.lastCutoff, wantCutoff)
	}
	out := stdout.String()
	for _, want := range []string{
		"river-tier0 (last 24h",
		"APPROXIMATE",
		"kind=gcal_sync n=5 p50_wait_s=0.250 p50_run_s=1.500",
		"kind=cadence_updater n=12 p50_wait_s=0.010 p50_run_s=0.020",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
}

func TestRunRiverTier0Empty(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	deps.tier0 = &fakeTier0Reader{rows: nil}

	if err := run(context.Background(), runOptions{riverTier0: true, windowHours: 24}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "no finished jobs in window") {
		t.Fatalf("expected empty-window message, got %s", stdout.String())
	}
}

func TestRunRiverTier0RejectsBadWindow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		windowHours int
	}{
		{"zero", 0},
		{"negative", -5},
		{"above_max", maxTier0WindowHours + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _, _, _, _ := newTestDeps()
			tier0 := &fakeTier0Reader{}
			deps.tier0 = tier0

			err := run(context.Background(), runOptions{riverTier0: true, windowHours: tc.windowHours}, deps)
			if err == nil {
				t.Fatalf("expected error for window-hours=%d", tc.windowHours)
			}
			if !strings.Contains(err.Error(), "--window-hours must be between") {
				t.Fatalf("expected window-hours validation error, got %v", err)
			}
			if tier0.calls != 0 {
				t.Fatalf("Tier0StatsByKind must not be called on a bad window; got %d", tier0.calls)
			}
		})
	}
}

func TestRunRiverTier0Error(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	deps.tier0 = &fakeTier0Reader{err: errors.New("db down")}

	err := run(context.Background(), runOptions{riverTier0: true, windowHours: 24}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "river tier0 stats") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}
