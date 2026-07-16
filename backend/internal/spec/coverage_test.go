package spec

import (
	"strings"
	"testing"
)

// loadCoverage runs the full pipeline (lint + collect + compute) over one of
// the repo-root-shaped fixtures under testdata/.
func loadCoverage(t *testing.T, root string) *Coverage {
	t.Helper()
	files, viol, err := Lint(root + "/spec")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("fixture corpus must lint clean, got:\n%s", joinViolations(viol))
	}
	cites, probs, err := CollectCitations(root+"/backend", root+"/frontend/tests/e2e")
	if err != nil {
		t.Fatalf("CollectCitations returned error: %v", err)
	}
	return ComputeCoverage(files, cites, probs)
}

func findItem(items []ItemCoverage, ref string) *ItemCoverage {
	for i := range items {
		if items[i].Ref() == ref {
			return &items[i]
		}
	}
	return nil
}

func TestCollectCitations(t *testing.T) {
	cites, probs, err := CollectCitations("testdata/coverage/backend", "testdata/coverage/frontend/tests/e2e")
	if err != nil {
		t.Fatalf("CollectCitations returned error: %v", err)
	}

	// The malformed lowercase ref and the trailing marker are problems, not
	// citations.
	all := joinViolations(probs)
	if len(probs) != 2 ||
		!strings.Contains(all, `malformed spec citation "alp-001"`) ||
		!strings.Contains(all, "spec citation marker must be the only content on its line") {
		t.Fatalf("want malformed + trailing-marker problems, got %#v", probs)
	}

	// 6 clean Go refs + 6 clean E2E refs; the helper.ts citation and the
	// trailing marker must NOT be collected as citations.
	var goCites, e2eCites int
	for _, c := range cites {
		if c.E2E {
			e2eCites++
			if !strings.HasSuffix(c.Path, ".spec.ts") {
				t.Errorf("E2E citation from a non-spec file: %s", c.Path)
			}
		} else {
			goCites++
		}
		if c.ID == "ALP-003" {
			t.Errorf("citation collected from helper.ts or a trailing marker: %+v", c)
		}
	}
	if goCites != 6 || e2eCites != 6 {
		t.Errorf("want 6 Go + 6 E2E citations, got %d + %d: %#v", goCites, e2eCites, cites)
	}

	// Indexed and bare parse shapes.
	var sawIndexed, sawBare bool
	for _, c := range cites {
		if c.ID == "ALP-001" && c.Then == 0 && c.E2E {
			sawIndexed = true
		}
		if c.ID == "ALP-002" && c.Then == -1 && c.E2E {
			sawBare = true
		}
	}
	if !sawIndexed || !sawBare {
		t.Errorf("indexed/bare citation shapes not parsed: %#v", cites)
	}
}

func TestComputeCoverageVerdicts(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage")

	if len(cov.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(cov.Domains))
	}
	d := cov.Domains[0]
	if d.Domain != "alpha" || d.Settled {
		t.Fatalf("unexpected domain header: %+v", d)
	}
	if d.UI != 7 || d.API != 1 || d.None != 0 || d.Intents != 1 || d.Retired != 1 {
		t.Errorf("surface counts ui/api/none/intents/retired = %d/%d/%d/%d/%d, want 7/1/0/1/1",
			d.UI, d.API, d.None, d.Intents, d.Retired)
	}

	covered, waived, orphans := d.Counts()
	if covered != 4 || waived != 2 || orphans != 1 {
		t.Fatalf("counts covered/waived/orphans = %d/%d/%d, want 4/2/1\nitems: %#v", covered, waived, orphans, d.Items)
	}

	cases := []struct{ ref, state string }{
		{"ALP-001[0]", ItemCovered}, // indexed citation
		{"ALP-001[1]", ItemWaived},  // waiver
		{"ALP-002[0]", ItemCovered}, // bare citation covers all items
		{"ALP-003[0]", ItemOrphan},  // uncited
		{"ALP-004", ItemCovered},    // statement behavior, single implicit item
		{"ALP-009[0]", ItemCovered}, // cited wins over the (stale) waiver
		{"ALP-010", ItemWaived},     // statement behavior waived via index 0
	}
	for _, tc := range cases {
		it := findItem(d.Items, tc.ref)
		if it == nil {
			t.Errorf("item %s missing from report: %#v", tc.ref, d.Items)
			continue
		}
		if it.State != tc.state {
			t.Errorf("%s state = %s, want %s", tc.ref, it.State, tc.state)
		}
	}
	if it := findItem(d.Items, "ALP-001[1]"); it != nil && it.Reason == "" {
		t.Error("waived item should carry the waiver reason")
	}
	// Proposed behaviors contribute no items.
	if it := findItem(d.Items, "ALP-008[0]"); it != nil {
		t.Errorf("proposed behavior must not appear in coverage items: %#v", it)
	}
}

func TestComputeCoverageProblemsAndWarnings(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage")

	wantProblems := []string{
		`malformed spec citation "alp-001"`,
		"spec citation marker must be the only content on its line",
		"citation DEAD-001 names an unknown behavior ID",
		"citation ALP-006 names an intent behavior",
		"citation ALP-007 names a retired behavior",
		"citation ALP-008 names a proposed behavior",
		"citation ALP-004[0] indexes a statement behavior",
		"citation ALP-002[5] is out of range (1 then items)",
	}
	if len(cov.Problems) != len(wantProblems) {
		t.Fatalf("want %d problems, got %d:\n%s", len(wantProblems), len(cov.Problems), joinViolations(cov.Problems))
	}
	all := joinViolations(cov.Problems)
	for _, sub := range wantProblems {
		if !strings.Contains(all, sub) {
			t.Errorf("problems missing %q; got:\n%s", sub, all)
		}
	}

	wantWarnings := []string{
		"E2E citation ALP-005 names a api-surface behavior",
		"alpha.yaml: ALP-009[0]: stale waiver: the item is waived but cited",
	}
	if len(cov.Warnings) != len(wantWarnings) {
		t.Fatalf("want %d warnings, got %d:\n%s", len(wantWarnings), len(cov.Warnings), joinViolations(cov.Warnings))
	}
	allW := joinViolations(cov.Warnings)
	for _, sub := range wantWarnings {
		if !strings.Contains(allW, sub) {
			t.Errorf("warnings missing %q; got:\n%s", sub, allW)
		}
	}
}

func TestComputeCoverageSettledDomain(t *testing.T) {
	// The fixture spells the flag `e2e_settled: True` — a legal !!bool
	// spelling yaml.v3 does not normalize — pinning that enforcement cannot
	// be silently disabled by capitalization.
	cov := loadCoverage(t, "testdata/coverage-blocked")
	if len(cov.Domains) != 1 || !cov.Domains[0].Settled {
		t.Fatalf("beta should parse as settled: %#v", cov.Domains)
	}
	if _, _, orphans := cov.Domains[0].Counts(); orphans != 1 {
		t.Errorf("want 1 orphan in settled domain, got %d", orphans)
	}
	if len(cov.Problems) != 0 {
		t.Errorf("no citation problems expected, got:\n%s", joinViolations(cov.Problems))
	}
}

func TestComputeCoverageClean(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage-clean")
	if len(cov.Problems) != 0 || len(cov.Warnings) != 0 {
		t.Fatalf("clean fixture should have no problems/warnings: %#v / %#v", cov.Problems, cov.Warnings)
	}
	covered, waived, orphans := cov.Domains[0].Counts()
	if covered != 1 || waived != 0 || orphans != 0 {
		t.Errorf("counts = %d/%d/%d, want 1/0/0", covered, waived, orphans)
	}
}

func TestCollectCitationsMissingRoot(t *testing.T) {
	if _, _, err := CollectCitations("testdata/does-not-exist", "testdata/coverage/frontend/tests/e2e"); err == nil {
		t.Error("missing backend root should return an error")
	}
}
