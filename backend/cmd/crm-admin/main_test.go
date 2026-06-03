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

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
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

func newTestDeps() (adminDeps, *bytes.Buffer, *fakeTokenMinter, *fakeHostLister, *fakeHostRevoker, *fakeRematchRunner) {
	stdout := &bytes.Buffer{}
	tokens := &fakeTokenMinter{
		token:     "test-token-base64",
		expiresAt: time.Date(2026, 5, 13, 15, 42, 18, 0, time.UTC),
	}
	hosts := &fakeHostLister{}
	revoker := &fakeHostRevoker{}
	rematch := &fakeRematchRunner{result: &messages.RematchStrandedResult{}}
	return adminDeps{
		tokens:  tokens,
		hosts:   hosts,
		revoker: revoker,
		rematch: rematch,
		stdout:  stdout,
		stderr:  &bytes.Buffer{},
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

type fakeCorrespondenceScanner struct {
	upserted int
	err      error
	since    time.Time
	log      *[]string
}

func (f *fakeCorrespondenceScanner) Run(_ context.Context, since time.Time) (int, error) {
	f.since = since
	if f.log != nil {
		*f.log = append(*f.log, "producer")
	}
	if f.err != nil {
		return 0, f.err
	}
	return f.upserted, nil
}

func TestRunRederiveCorrespondenceNamesHappyAndOrdering(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	log := &[]string{}
	rederive := &fakeRederiveRunner{
		result: google.CorrespondenceRederiveResult{
			Scanned: 12, Rederived: 9, SkippedNoGmailID: 2, SkippedUnavailable: 1, Failed: 0,
		},
		log: log,
	}
	scanner := &fakeCorrespondenceScanner{upserted: 4, log: log}
	deps.rederive = rederive
	deps.correspondence = scanner

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Ordering: re-derive FIRST, then the full-range producer pass.
	if len(*log) != 2 || (*log)[0] != "rederive" || (*log)[1] != "producer" {
		t.Fatalf("expected [rederive producer], got %v", *log)
	}
	// Both phases use the same full-range floor (2026-01-01).
	if !rederive.since.Equal(scanner.since) {
		t.Fatalf("phases used different since: %v vs %v", rederive.since, scanner.since)
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
		"candidates_upserted:  4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
}

func TestRunRederiveCorrespondenceNamesFailedExitsNonZero(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	log := &[]string{}
	// Partial failure: some rows failed, but successfully re-derived rows must
	// still get the full-range producer pass (P2).
	deps.rederive = &fakeRederiveRunner{
		result: google.CorrespondenceRederiveResult{Scanned: 5, Rederived: 3, Failed: 2},
		log:    log,
	}
	scanner := &fakeCorrespondenceScanner{upserted: 1, log: log}
	deps.correspondence = scanner

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err == nil {
		t.Fatal("expected non-nil error when some rows failed")
	}
	// The producer pass still ran despite the failures.
	if len(*log) != 2 || (*log)[1] != "producer" {
		t.Fatalf("expected producer to run after partial failure, log=%v", *log)
	}
	if !strings.Contains(stdout.String(), "failed:               2") {
		t.Fatalf("expected summary printed with failed count, got %q", stdout.String())
	}
}

func TestRunRederiveCorrespondenceNamesRederiveErrorSkipsProducer(t *testing.T) {
	deps, _, _, _, _, _ := newTestDeps()
	log := &[]string{}
	deps.rederive = &fakeRederiveRunner{err: errors.New("list failed"), log: log}
	deps.correspondence = &fakeCorrespondenceScanner{log: log}

	err := run(context.Background(), runOptions{rederiveCorrespondence: true}, deps)
	if err == nil {
		t.Fatal("expected error when the re-derive phase hard-errors")
	}
	// A hard error in phase 1 aborts before the producer pass.
	if len(*log) != 1 || (*log)[0] != "rederive" {
		t.Fatalf("expected producer NOT to run after a hard re-derive error, log=%v", *log)
	}
}
