package main

import (
	"errors"
	"strings"
	"testing"
)

// The CLI tests point at the internal/spec package's fixtures via relative
// path (the test binary runs in this package's directory).
const fixtures = "../../internal/spec/testdata"

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf strings.Builder
	code = run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestRunValidDir(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/valid")
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "2 files, 8 behaviors — OK") {
		t.Errorf("summary line missing from stdout: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
}

func TestRunViolatingDir(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/invalid/bad-maturity")
	if code != 1 {
		t.Fatalf("want exit 1, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "invalid maturity") {
		t.Errorf("violation missing from stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "1 violations") {
		t.Errorf("summary line missing violation count: %q", stdout)
	}
}

func TestRunEmptyDir(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/empty")
	if code != 0 {
		t.Fatalf("want exit 0 on empty corpus, got %d", code)
	}
	if !strings.Contains(stdout, "no spec files found") {
		t.Errorf("empty-corpus note missing from stdout: %q", stdout)
	}
}

func TestRunNonexistentDir(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/does-not-exist")
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "spec-lint:") {
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
	if code := run([]string{fixtures + "/valid"}, failWriter{}, &errBuf); code != 2 {
		t.Fatalf("want exit 2 on stdout write failure, got %d", code)
	}
}

func TestRunBadArgCount(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}} {
		code, _, stderr := runCLI(t, args...)
		if code != 2 {
			t.Errorf("args %v: want exit 2, got %d", args, code)
		}
		if !strings.Contains(stderr, "usage: spec-lint") {
			t.Errorf("args %v: usage missing from stderr: %q", args, stderr)
		}
	}
}
