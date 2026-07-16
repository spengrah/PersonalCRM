package main

import (
	"errors"
	"strings"
	"testing"
)

// The CLI tests point at the internal/spec package's repo-root-shaped
// fixtures via relative path (the test binary runs in this package's
// directory).
const fixtures = "../../internal/spec/testdata"

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf strings.Builder
	code = run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestRunCleanRoot(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/coverage-clean")
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "all ui-surface then-items covered or waived") {
		t.Errorf("summary line missing from stdout: %q", stdout)
	}
}

func TestRunInvalidCitations(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage")
	if code != 1 {
		t.Fatalf("want exit 1 on invalid citations, got %d", code)
	}
	for _, sub := range []string{
		"INVALID:",
		"citation DEAD-001 names an unknown behavior ID",
		"WARNING:",
		"ORPHAN ALP-003[0]",
		"waived ALP-001[1]",
		"1 orphaned ui then-items, 8 invalid citations",
	} {
		if !strings.Contains(stdout, sub) {
			t.Errorf("stdout missing %q; got:\n%s", sub, stdout)
		}
	}
	// An unsettled domain's orphan warns; it must not render as blocking.
	if strings.Contains(stdout, "ORPHAN (blocking)") {
		t.Errorf("unsettled orphan rendered as blocking:\n%s", stdout)
	}
}

func TestRunSettledOrphanBlocks(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage-blocked")
	if code != 1 {
		t.Fatalf("want exit 1 on settled-domain orphan, got %d", code)
	}
	if !strings.Contains(stdout, "ORPHAN (blocking) BET-001[0]") {
		t.Errorf("blocking orphan missing from stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "[settled]") {
		t.Errorf("settled marker missing from stdout: %q", stdout)
	}
}

func TestRunLintDirtyCorpus(t *testing.T) {
	// A repo root whose spec/ does not lint clean is an operational error —
	// point spec/ at a fixture with violations by faking the layout.
	code, _, stderr := runCLI(t, fixtures+"/coverage-lint-dirty")
	if code != 2 {
		t.Fatalf("want exit 2 on lint-dirty corpus, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "lint violations") {
		t.Errorf("stderr should name the lint failure, got %q", stderr)
	}
}

func TestRunNonexistentRoot(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/does-not-exist")
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "spec-coverage:") {
		t.Errorf("error missing from stderr: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on operational error, got %q", stdout)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("sink failed") }

// A stdout write failure is an operational error: the run cannot prove it
// reported everything, so it must not exit 0 or 1.
func TestRunStdoutWriteFailure(t *testing.T) {
	var errBuf strings.Builder
	if code := run([]string{fixtures + "/coverage-clean"}, failWriter{}, &errBuf); code != 2 {
		t.Fatalf("want exit 2 on stdout write failure, got %d", code)
	}
}

func TestRunBadArgCount(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}} {
		code, _, stderr := runCLI(t, args...)
		if code != 2 {
			t.Errorf("args %v: want exit 2, got %d", args, code)
		}
		if !strings.Contains(stderr, "usage: spec-coverage") {
			t.Errorf("args %v: usage missing from stderr: %q", args, stderr)
		}
	}
}
