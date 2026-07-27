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

// hasLine reports whether want appears as a COMPLETE line of stdout (split on
// "\n"), so report-format goldens pin exact spacing/order/punctuation — the
// contract pre-push log-scrapers depend on — rather than a loose substring.
func hasLine(stdout, want string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func TestRunCleanRoot(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/coverage-clean")
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)", code, stderr)
	}
	if !hasLine(stdout, "spec-coverage: all ui- and api-surface then-items covered or waived") {
		t.Errorf("clean summary line missing from stdout:\n%s", stdout)
	}
	// gamma is a ui-only clean domain rendered with an empty settled list ("-").
	if !hasLine(stdout, "gamma              [settled: -]  surface ui/api/none: 1/0/0  ui: 1 covered, 0 waived, 0 orphaned  api: 0 covered, 0 waived, 0 orphaned") {
		t.Errorf("clean domain header golden missing from stdout:\n%s", stdout)
	}
}

func TestRunInvalidCitations(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage")
	if code != 1 {
		t.Fatalf("want exit 1 on invalid citations, got %d", code)
	}
	// Substring signals that carry a path/line prefix stay Contains checks.
	for _, sub := range []string{
		"INVALID:",
		"citation DEAD-001 names an unknown behavior ID",
		"WARNING:",
	} {
		if !strings.Contains(stdout, sub) {
			t.Errorf("stdout missing %q; got:\n%s", sub, stdout)
		}
	}
	// Report-format lines are pinned as complete-line goldens: the unsettled
	// alpha header (empty settled list → "-"), a ui orphan (non-blocking), a
	// waived line, and the new per-surface totals line.
	for _, line := range []string{
		"alpha              [settled: -]  surface ui/api/none: 7/8/1  ui: 4 covered, 2 waived, 1 orphaned  api: 5 covered, 2 waived, 3 orphaned",
		"  ORPHAN ALP-003[0]: the request carries the filter param",
		"  waived ALP-001[1]: focus-driven freshness is not deterministically provable",
		"spec-coverage: 4 orphaned then-items (ui 1, api 3), 8 invalid citations",
	} {
		if !hasLine(stdout, line) {
			t.Errorf("stdout missing golden line %q; got:\n%s", line, stdout)
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
	if !hasLine(stdout, "  ORPHAN (blocking) BET-001[0]: y") {
		t.Errorf("blocking orphan golden missing from stdout:\n%s", stdout)
	}
	if !hasLine(stdout, "beta               [settled: ui]  surface ui/api/none: 1/0/0  ui: 0 covered, 0 waived, 1 orphaned  api: 0 covered, 0 waived, 0 orphaned") {
		t.Errorf("settled header golden missing from stdout:\n%s", stdout)
	}
}

// TestRunApiSettledBlocks pins the new api-settlement path: a domain that lists
// api in settled with an uncited api behavior blocks (exit 1), the orphan
// renders "(blocking)", and the reversed file input `settled: [api, ui]`
// canonicalizes to `[settled: ui,api]` end-to-end.
func TestRunApiSettledBlocks(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage-api-blocked")
	if code != 1 {
		t.Fatalf("want exit 1 on api-settled orphan, got %d", code)
	}
	if !hasLine(stdout, "apiblocked         [settled: ui,api]  surface ui/api/none: 0/1/0  ui: 0 covered, 0 waived, 0 orphaned  api: 0 covered, 0 waived, 1 orphaned") {
		t.Errorf("api-settled header golden missing (canonicalization?):\n%s", stdout)
	}
	if !hasLine(stdout, "  ORPHAN (blocking) API-001[0]: a 200 carries the resource") {
		t.Errorf("blocking api orphan golden missing from stdout:\n%s", stdout)
	}
}

// TestRunApiWarnNotBlocked pins that the same uncited api orphan only warns
// (exit 0) when api is absent from the domain's settled list.
func TestRunApiWarnNotBlocked(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage-api-warn")
	if code != 0 {
		t.Fatalf("want exit 0 when api is unsettled, got %d", code)
	}
	if !hasLine(stdout, "  ORPHAN API-001[0]: a 200 carries the resource") {
		t.Errorf("non-blocking api orphan golden missing from stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "ORPHAN (blocking)") {
		t.Errorf("api orphan must not render as blocking when api is unsettled:\n%s", stdout)
	}
}

// TestSettledLabel exercises the canonicalization branch directly: input order
// is normalized to enum order (ui before api), and an empty list renders "-".
func TestSettledLabel(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"api", "ui"}, "ui,api"}, // reversed input canonicalized
		{[]string{"ui"}, "ui"},
		{[]string{"api"}, "api"},
		{nil, "-"},
	}
	for _, tc := range cases {
		if got := settledLabel(tc.in); got != tc.want {
			t.Errorf("settledLabel(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunKeyedCitationsClean pins the keyed form at the CLI boundary: a corpus
// whose items are cited by key (with a keyed waiver and a reserved-token
// statement waiver alongside) is fully covered and exits 0.
func TestRunKeyedCitationsClean(t *testing.T) {
	code, stdout, stderr := runCLI(t, fixtures+"/coverage-keys")
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)\n%s", code, stderr, stdout)
	}
	if !hasLine(stdout, "spec-coverage: all ui- and api-surface then-items covered or waived") {
		t.Errorf("clean summary line missing from stdout:\n%s", stdout)
	}
	// Waived lines render by key, and a statement behavior's implicit item
	// still renders as the bare ID.
	for _, line := range []string{
		"  waived ALP-001.waived-outcome: not deterministically provable in the browser",
		"  waived ALP-003: structural, enforced by the query layer rather than observable",
	} {
		if !hasLine(stdout, line) {
			t.Errorf("stdout missing golden line %q; got:\n%s", line, stdout)
		}
	}
}

// TestSpecCoverage_KeyedCorpusInsertionStable is acceptance criterion 2 at the
// CLI boundary: the same corpus with one item inserted at position 1 keeps
// every keyed citation's verdict, while the indexed citations in the same
// fixture re-point and orphan the assertion they used to name.
func TestSpecCoverage_KeyedCorpusInsertionStable(t *testing.T) {
	_, before, _ := runCLI(t, fixtures+"/coverage-keys")
	code, after, _ := runCLI(t, fixtures+"/coverage-keys-inserted")
	if code != 0 {
		t.Fatalf("want exit 0 (alpha is unsettled, so orphans warn), got %d:\n%s", code, after)
	}

	// Keyed: the waived item is still addressed by the same reference and still
	// waived, though an item was inserted above it.
	const waived = "  waived ALP-001.waived-outcome: not deterministically provable in the browser"
	if !hasLine(before, waived) || !hasLine(after, waived) {
		t.Errorf("keyed waiver line did not survive the insertion:\nBEFORE:\n%s\nAFTER:\n%s", before, after)
	}
	// No keyed item that was covered before became an orphan.
	for _, line := range []string{
		"  ORPHAN ALP-001.first-outcome: the first outcome holds",
		"  ORPHAN ALP-001.second-outcome: the second outcome holds",
	} {
		if hasLine(after, line) {
			t.Errorf("a keyed citation lost its item across the insertion: %q\n%s", line, after)
		}
	}
	// Indexed, same insertion: the assertion the citation used to name is now
	// an orphan. If this stops holding, the fixture pair has stopped
	// discriminating and the keyed-stability claim above proves nothing.
	const shifted = "  ORPHAN ALP-002[2]: the indexed second outcome holds"
	if hasLine(before, shifted) {
		t.Fatalf("precondition: the indexed item must be covered before the insertion:\n%s", before)
	}
	if !hasLine(after, shifted) {
		t.Errorf("indexed citation should have re-pointed, orphaning %q:\n%s", shifted, after)
	}
}

// TestSpecCoverage_DanglingKeyExitsOne is acceptance criterion 3 at the CLI
// boundary: a citation naming a key the corpus no longer carries is a hard
// failure that names the citing site.
func TestSpecCoverage_DanglingKeyExitsOne(t *testing.T) {
	code, stdout, _ := runCLI(t, fixtures+"/coverage-dangling-key")
	if code != 1 {
		t.Fatalf("want exit 1 on a dangling keyed citation, got %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "citation ALP-001.renamed-key names an unknown then-item key (behavior has: alpha, beta)") {
		t.Errorf("stdout missing the dangling-key problem:\n%s", stdout)
	}
	if !strings.Contains(stdout, "alpha_test.go:3") {
		t.Errorf("stdout must name the citing site:\n%s", stdout)
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
