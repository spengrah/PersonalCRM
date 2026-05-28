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

	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"

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
			"rotate-host-key",
			[]string{"--rotate-host-key", "11111111-2222-3333-4444-555555555555"},
			func(t *testing.T, o runOptions) {
				if o.rotateHostID != "11111111-2222-3333-4444-555555555555" {
					t.Fatalf("expected rotate id, got %q", o.rotateHostID)
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
