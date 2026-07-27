package spec

import (
	"fmt"
	"os"
	"path/filepath"
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

	// 11 clean Go refs + 6 clean E2E refs; the helper.ts citation and the
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
		// The leak canary is scoped to E2E citations: both leak sources —
		// helper.ts (a non-.spec.ts file under the e2e root) and the trailing
		// marker on the last line of alpha.spec.ts — live under the e2e root, so
		// any leak would surface as an E2E == true citation. The bare Go cite of
		// ALP-003 in foo_test.go is the intentional Go-cite-of-ui fixture (it
		// grants no ui coverage), not a leak.
		if c.ID == "ALP-003" && c.E2E {
			t.Errorf("citation collected from helper.ts or a trailing marker: %+v", c)
		}
	}
	if goCites != 11 || e2eCites != 6 {
		t.Errorf("want 11 Go + 6 E2E citations, got %d + %d: %#v", goCites, e2eCites, cites)
	}

	// Keyed and bare parse shapes.
	var sawKeyed, sawBare bool
	for _, c := range cites {
		if c.ID == "ALP-001" && c.Key == "list-refreshes" && c.E2E {
			sawKeyed = true
		}
		if c.ID == "ALP-002" && c.Key == "" && c.E2E {
			sawBare = true
		}
	}
	if !sawKeyed || !sawBare {
		t.Errorf("keyed/bare citation shapes not parsed: %#v", cites)
	}
}

func TestComputeCoverageVerdicts(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage")

	if len(cov.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(cov.Domains))
	}
	d := cov.Domains[0]
	if d.Domain != "alpha" || len(d.Settled) != 0 {
		t.Fatalf("unexpected domain header: %+v", d)
	}
	if d.UI != 7 || d.API != 8 || d.None != 1 || d.Intents != 1 || d.Retired != 1 {
		t.Errorf("surface counts ui/api/none/intents/retired = %d/%d/%d/%d/%d, want 7/8/1/1/1",
			d.UI, d.API, d.None, d.Intents, d.Retired)
	}

	// ui and api orphan populations are counted independently.
	uiC, uiW, uiO := d.SurfaceCounts("ui")
	if uiC != 4 || uiW != 2 || uiO != 1 {
		t.Fatalf("ui counts covered/waived/orphans = %d/%d/%d, want 4/2/1\nitems: %#v", uiC, uiW, uiO, d.Items)
	}
	apiC, apiW, apiO := d.SurfaceCounts("api")
	if apiC != 5 || apiW != 2 || apiO != 3 {
		t.Fatalf("api counts covered/waived/orphans = %d/%d/%d, want 5/2/3\nitems: %#v", apiC, apiW, apiO, d.Items)
	}
	// Counts() aggregates across surfaces: ui(4/2/1) + api(5/2/3) = 9/4/4.
	aggC, aggW, aggO := d.Counts()
	if aggC != 9 || aggW != 4 || aggO != 4 {
		t.Fatalf("aggregate counts covered/waived/orphans = %d/%d/%d, want 9/4/4\nitems: %#v", aggC, aggW, aggO, d.Items)
	}

	cases := []struct {
		ref, state, surface string
	}{
		// Keyed items render by key; items that have never been cited or
		// waived carry no key and keep the positional rendering, which is a
		// location rather than a citable handle.
		{"ALP-001.list-refreshes", ItemCovered, "ui"},            // keyed E2E citation
		{"ALP-001.refocus-rechecks-freshness", ItemWaived, "ui"}, // keyed waiver
		{"ALP-002[0]", ItemCovered, "ui"},                        // bare E2E citation covers all items
		{"ALP-003[0]", ItemOrphan, "ui"},                         // uncited (a Go cite grants no ui coverage)
		{"ALP-004", ItemCovered, "ui"},                           // ui statement behavior, single implicit item
		{"ALP-009.stale-waiver-item", ItemCovered, "ui"},         // cited wins over the (stale) waiver
		{"ALP-010", ItemWaived, "ui"},                            // ui statement waived by the reserved token
		{"ALP-011[0]", ItemCovered, "api"},                       // bare Go citation covers all items
		{"ALP-011[1]", ItemCovered, "api"},                       // bare Go citation covers all items
		{"ALP-012[0]", ItemOrphan, "api"},                        // uncited api behavior
		{"ALP-013.body-is-chunked", ItemWaived, "api"},           // api waiver
		{"ALP-014", ItemCovered, "api"},                          // api statement covered by a bare Go cite
		{"ALP-015[0]", ItemOrphan, "api"},                        // uncited, so unkeyed
		{"ALP-015.error-body-names-field", ItemCovered, "api"},   // keyed Go citation covers only its item
		{"ALP-016", ItemWaived, "api"},                           // api statement waived by the reserved token
		{"ALP-017.conflict-409-named", ItemCovered, "api"},       // Go-cited (also waived → stale)
		{"ALP-005[0]", ItemOrphan, "api"},                        // E2E-cited only: grants no api coverage
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
		if it.Surface != tc.surface {
			t.Errorf("%s surface = %s, want %s", tc.ref, it.Surface, tc.surface)
		}
	}
	if it := findItem(d.Items, "ALP-001.refocus-rechecks-freshness"); it != nil && it.Reason == "" {
		t.Error("waived item should carry the waiver reason")
	}
	// Proposed behaviors contribute no items.
	if it := findItem(d.Items, "ALP-008[0]"); it != nil {
		t.Errorf("proposed behavior must not appear in coverage items: %#v", it)
	}
	// A none-surface behavior mints no coverage item even when Go-cited (its
	// tally is reflected only in d.None), pinning the reserved-none boundary.
	if it := findItem(d.Items, "ALP-018[0]"); it != nil {
		t.Errorf("none-surface behavior must not appear in coverage items: %#v", it)
	}
	if it := findItem(d.Items, "ALP-018"); it != nil {
		t.Errorf("none-surface behavior must not appear in coverage items: %#v", it)
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
		"citation ALP-004.some-key names a then-item key on a statement behavior (no then items)",
		"citation ALP-001.no-such-key names an unknown then-item key (behavior has: list-refreshes, refocus-rechecks-freshness)",
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
		"alpha.yaml: ALP-009.stale-waiver-item: stale waiver: the item is waived but cited by an E2E test",
		"alpha.yaml: ALP-017.conflict-409-named: stale waiver: the item is waived but cited by a Go test",
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
	// The fixture declares `settled: [ui]` — a surface list parsed onto the
	// file — pinning that a listed ui surface still tracks its orphans.
	cov := loadCoverage(t, "testdata/coverage-blocked")
	if len(cov.Domains) != 1 || len(cov.Domains[0].Settled) == 0 {
		t.Fatalf("beta should parse as settled: %#v", cov.Domains)
	}
	if _, _, orphans := cov.Domains[0].SurfaceCounts("ui"); orphans != 1 {
		t.Errorf("want 1 ui orphan in settled domain, got %d", orphans)
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

// markerPrefix is the citation-marker prefix, assembled from two literals so
// that this file never contains one. coverage_test.go is a *_test.go OUTSIDE
// testdata/, so the repo's own `make spec-coverage` run walks it — a literal
// marker here (even inside a Go string) would surface as a spurious INVALID in
// the real report.
const markerPrefix = "// spec" + ": "

// marker renders one whole-line citation marker for the temp-tree fixtures.
func marker(refs string) string { return markerPrefix + refs + "\n" }

// scanMarkers writes one Go test file carrying the given marker lines into a
// throwaway repo-shaped tree and returns what CollectCitations extracts. The
// grammar cases need no corpus, so a temp tree keeps them out of the shared
// coverage fixtures (whose per-item verdicts other tests pin exactly).
func scanMarkers(t *testing.T, body string) ([]Citation, []Violation) {
	t.Helper()
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	e2e := filepath.Join(root, "frontend", "tests", "e2e")
	for _, d := range []string{backend, e2e} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(backend, "x_test.go"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cites, probs, err := CollectCitations(backend, e2e)
	if err != nil {
		t.Fatalf("CollectCitations returned error: %v", err)
	}
	return cites, probs
}

// TestCollectCitationsKeyedForm pins the two remaining reference forms: a keyed
// reference parses into Key, a bare one leaves it empty, and Ref() round-trips
// both.
func TestCollectCitationsKeyedForm(t *testing.T) {
	cites, probs := scanMarkers(t, "package x\n"+
		marker("ALP-001.live-contact-flag")+
		marker("ALP-003")+
		marker("ALP-004.a1-b2, ALP-005.k"))
	if len(probs) != 0 {
		t.Fatalf("no problems expected, got:\n%s", joinViolations(probs))
	}
	want := []struct{ id, key, ref string }{
		{"ALP-001", "live-contact-flag", "ALP-001.live-contact-flag"},
		{"ALP-003", "", "ALP-003"},
		{"ALP-004", "a1-b2", "ALP-004.a1-b2"},
		{"ALP-005", "k", "ALP-005.k"},
	}
	if len(cites) != len(want) {
		t.Fatalf("want %d citations, got %d: %#v", len(want), len(cites), cites)
	}
	for i, w := range want {
		c := cites[i]
		if c.ID != w.id || c.Key != w.key {
			t.Errorf("citation %d = {ID:%q Key:%q}, want {ID:%q Key:%q}", i, c.ID, c.Key, w.id, w.key)
		}
		if got := c.Ref(); got != w.ref {
			t.Errorf("citation %d Ref() = %q, want %q", i, got, w.ref)
		}
	}
}

// TestCollectCitations_IndexedFormRejected pins the retirement of the positional
// form: it yields no citation and a TARGETED message that tells the author to
// cite by key, rather than degrading into the generic malformed one (which
// would send them looking for a typo in a reference that is not malformed at
// all — it names a real behavior and a real item, the one way that can silently
// re-point).
func TestCollectCitations_IndexedFormRejected(t *testing.T) {
	cites, probs := scanMarkers(t, "package x\n"+marker("ALP-002[3]"))
	if len(cites) != 0 {
		t.Errorf("a retired-form reference must not be collected: %#v", cites)
	}
	if len(probs) != 1 {
		t.Fatalf("want 1 problem, got %d:\n%s", len(probs), joinViolations(probs))
	}
	want := `spec citation "ALP-002[3]" uses the retired positional form; cite the then-item by key (<ID>.<then-item-key>)`
	if probs[0].Msg != want {
		t.Errorf("problem = %q, want %q", probs[0].Msg, want)
	}
	if strings.Contains(probs[0].Msg, "malformed") {
		t.Error("the retired-form message must be distinct from the generic malformed one")
	}
}

// TestThenKeyCharsetIsShared pins that the then-item key charset has ONE
// definition. thenKeyRegex (the lint pattern) and citationRef's key alternative
// are both built from thenKeyCharset, so they cannot drift — this test's job is
// to fail the moment someone re-introduces a literal on either side, which is
// how the two silently diverged into a key that lints clean but can never be
// cited (or the reverse).
func TestThenKeyCharsetIsShared(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"live-contact-flag", true},
		{"a1-b2", true},
		{"k", true},
		{"abc123", true},
		{"bad_key", false}, // underscore: the row the divergence demonstration turns on
		{"BadKey", false},
		{"-lead", false},
		{"trail-", false},
		{"a--b", false},
		{"", false},
	}
	for _, tc := range cases {
		lint := thenKeyRegex.MatchString(tc.key)
		m := citationRef.FindStringSubmatch("ALP-001." + tc.key)
		cite := m != nil && m[2] == tc.key
		if lint != tc.want || cite != tc.want {
			t.Errorf("key %q: thenKeyRegex=%v citationRef=%v, want both %v — the two definitions have diverged",
				tc.key, lint, cite, tc.want)
		}
	}
}

// TestCollectCitations_HashSuffixReserved pins the @ reservation: a reference
// carrying one gets its OWN forward-looking message, not the generic malformed
// one, so the character cannot be squatted before the suffix is designed.
func TestCollectCitations_HashSuffixReserved(t *testing.T) {
	cites, probs := scanMarkers(t, "package x\n"+marker("ALP-001.live-contact-flag@a3f2"))
	if len(cites) != 0 {
		t.Errorf("a reserved-suffix reference must not be collected: %#v", cites)
	}
	if len(probs) != 1 {
		t.Fatalf("want 1 problem, got %d:\n%s", len(probs), joinViolations(probs))
	}
	want := `spec citation "ALP-001.live-contact-flag@a3f2" uses the reserved @hash suffix, which is not yet supported`
	if probs[0].Msg != want {
		t.Errorf("problem = %q, want %q", probs[0].Msg, want)
	}
	if strings.Contains(probs[0].Msg, "malformed") {
		t.Error("the reserved-suffix message must be distinct from the generic malformed one")
	}
}

// TestCollectCitations_MalformedKey pins that the key charset is enforced at
// the citation site too — a key outside it could never match a linted then-item
// key, so accepting it would only defer the failure.
func TestCollectCitations_MalformedKey(t *testing.T) {
	for _, ref := range []string{"ALP-001.Bad_Key", "ALP-001.", "ALP-001.-lead", "ALP-001.trail-", "ALP-001.a--b"} {
		cites, probs := scanMarkers(t, "package x\n"+marker(ref))
		if len(cites) != 0 {
			t.Errorf("%s: must not be collected, got %#v", ref, cites)
		}
		if len(probs) != 1 {
			t.Fatalf("%s: want 1 malformed-citation problem, got %#v", ref, probs)
		}
		// The HINT is pinned by equality, not by a "malformed" prefix match:
		// it enumerates the accepted forms, so it is the one line an author
		// reads to learn the grammar, and a prefix assertion would let it rot.
		want := fmt.Sprintf("malformed spec citation %q (want <ID> or <ID>.<then-item-key>)", ref)
		if probs[0].Msg != want {
			t.Errorf("%s: msg = %q, want %q", ref, probs[0].Msg, want)
		}
	}
}

// TestComputeCoverageKeyedVerdicts pins keyed resolution end to end: a keyed
// citation covers exactly its item, a keyed waiver waives exactly its item, and
// the reserved "statement" waiver waives a statement behavior's implicit item.
func TestComputeCoverageKeyedVerdicts(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage-keys")
	if len(cov.Problems) != 0 {
		t.Fatalf("no problems expected, got:\n%s", joinViolations(cov.Problems))
	}
	if len(cov.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(cov.Domains))
	}
	d := cov.Domains[0]

	cases := []struct{ ref, state string }{
		{"ALP-001.first-outcome", ItemCovered},  // keyed E2E citation
		{"ALP-001.second-outcome", ItemCovered}, // keyed E2E citation
		{"ALP-001.waived-outcome", ItemWaived},  // keyed waiver
		{"ALP-003", ItemWaived},                 // statement waived by the reserved token
		{"ALP-004.rejects-bad-input", ItemCovered},
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
	// A keyed citation covers ONLY its own item: nothing else went covered by
	// accident, and no item stayed orphaned.
	if _, _, orphans := d.Counts(); orphans != 0 {
		t.Errorf("want 0 orphans, got %d: %#v", orphans, d.Items)
	}
}

// TestComputeCoverage_KeyedCitationSurvivesThenInsertion is acceptance
// criterion 2: two corpora differing only by an item inserted at position 1,
// and every keyed citation keeps the identical verdict for the identical
// assertion text. (The contrast leg — indexed citations re-pointing across the
// same insertion — retired with the form itself; its subject is now the
// retirement tests, which prove the form cannot be written at all.)
//
// Two assertions keep this from going vacuous: the precondition that each keyed
// text exists in the base corpus (if keying silently stopped resolving, it
// fails), and the fresh-orphan check on the inserted item (if the insertion
// silently did not land, it fails).
func TestComputeCoverage_KeyedCitationSurvivesThenInsertion(t *testing.T) {
	before := loadCoverage(t, "testdata/coverage-keys")
	after := loadCoverage(t, "testdata/coverage-keys-inserted")

	stateByText := func(cov *Coverage) map[string]string {
		out := map[string]string{}
		for _, d := range cov.Domains {
			for _, it := range d.Items {
				out[it.Text] = it.State
			}
		}
		return out
	}
	b, a := stateByText(before), stateByText(after)

	// Keyed: the assertion's verdict is unchanged by the insertion above it.
	for _, text := range []string{
		"the first outcome holds",
		"the second outcome holds",
		"the waived outcome is not browser-observable",
	} {
		if b[text] == "" {
			t.Fatalf("precondition: %q missing from the base corpus", text)
		}
		if a[text] != b[text] {
			t.Errorf("keyed item %q: state %s → %s across the insertion (must be stable)", text, b[text], a[text])
		}
	}

	// The inserted item is a fresh orphan — correct (nothing cites it yet), and
	// it is the anti-vacuity control: it proves the insertion actually landed
	// and coverage was recomputed over the changed corpus.
	if a["an outcome inserted at position 1"] != ItemOrphan {
		t.Errorf("a newly inserted keyed item should be orphaned, got %q", a["an outcome inserted at position 1"])
	}
	if b["an outcome inserted at position 1"] != "" {
		t.Errorf("precondition: the inserted item must NOT exist in the base corpus, got %q", b["an outcome inserted at position 1"])
	}
}

// TestComputeCoverage_DanglingKeyCitation is acceptance criterion 3: renaming
// (or deleting) a cited item's key fails loudly, naming the citing path:line,
// the reference, and the behavior's available keys.
func TestComputeCoverage_DanglingKeyCitation(t *testing.T) {
	cov := loadCoverage(t, "testdata/coverage-dangling-key")
	if len(cov.Problems) != 1 {
		t.Fatalf("want 1 problem, got %d:\n%s", len(cov.Problems), joinViolations(cov.Problems))
	}
	p := cov.Problems[0]
	const want = "citation ALP-001.renamed-key names an unknown then-item key (behavior has: alpha, beta)"
	if p.Msg != want {
		t.Errorf("problem = %q, want %q", p.Msg, want)
	}
	if !strings.HasSuffix(p.Path, "alpha_test.go") || p.Line == 0 {
		t.Errorf("problem must name the citing site, got %s:%d", p.Path, p.Line)
	}
}

// TestComputeCoverage_DanglingKeyOnUnkeyedBehavior pins the empty-enumeration
// branch of the same message: a behavior with no keyed items says so rather
// than rendering "behavior has: ".
func TestComputeCoverage_DanglingKeyOnUnkeyedBehavior(t *testing.T) {
	files := []*File{{Domain: "alpha", Prefix: "ALP", Path: "spec/alpha.yaml", Behaviors: []Behavior{{
		ID: "ALP-001", Type: "api", Status: "current", Surface: "api", When: "w",
		Then: []ThenItem{{Text: "an unkeyed outcome"}},
	}}}}
	// This mirrors what scanFile produces: a citation carries a then-item key
	// or it is bare — there is no third form.
	cites := []Citation{{Path: "backend/x_test.go", Line: 3, ID: "ALP-001", Key: "nope"}}
	cov := ComputeCoverage(files, cites, nil)
	if len(cov.Problems) != 1 {
		t.Fatalf("want 1 problem, got %#v", cov.Problems)
	}
	const want = "citation ALP-001.nope names an unknown then-item key (behavior has no keyed then items)"
	if cov.Problems[0].Msg != want {
		t.Errorf("problem = %q, want %q", cov.Problems[0].Msg, want)
	}
}

// TestComputeCoverage_KeyedCitationOfStatementBehavior pins that a keyed
// citation of a statement behavior is invalid: a statement behavior has no then
// list at all, so no key can address it.
func TestComputeCoverage_KeyedCitationOfStatementBehavior(t *testing.T) {
	files := []*File{{Domain: "alpha", Prefix: "ALP", Path: "spec/alpha.yaml", Behaviors: []Behavior{{
		ID: "ALP-001", Type: "invariant", Status: "current", Surface: "api",
		Statement: "every read filters soft-deleted rows",
	}}}}
	keyed := ComputeCoverage(files, []Citation{{Path: "backend/x_test.go", Line: 3, ID: "ALP-001", Key: "some-key"}}, nil)
	if len(keyed.Problems) != 1 {
		t.Fatalf("want 1 problem, got %#v", keyed.Problems)
	}
	const want = "citation ALP-001.some-key names a then-item key on a statement behavior (no then items)"
	if keyed.Problems[0].Msg != want {
		t.Errorf("keyed problem = %q, want %q", keyed.Problems[0].Msg, want)
	}
}

// TestItemCoverageRef pins the three rendering forms. It is load-bearing rather
// than cosmetic: coverage_test's findItem helper looks items up BY Ref().
func TestItemCoverageRef(t *testing.T) {
	cases := []struct {
		ic   ItemCoverage
		want string
	}{
		{ItemCoverage{ID: "ALP-001", Key: "live-contact-flag", Then: 0}, "ALP-001.live-contact-flag"},
		{ItemCoverage{ID: "ALP-001", Then: 2}, "ALP-001[2]"},
		{ItemCoverage{ID: "ALP-001", Then: -1}, "ALP-001"},
	}
	for _, tc := range cases {
		if got := tc.ic.Ref(); got != tc.want {
			t.Errorf("Ref(%#v) = %q, want %q", tc.ic, got, tc.want)
		}
	}
}

func TestCollectCitationsMissingRoot(t *testing.T) {
	if _, _, err := CollectCitations("testdata/does-not-exist", "testdata/coverage/frontend/tests/e2e"); err == nil {
		t.Error("missing backend root should return an error")
	}
}

// TestComputeCoverage_UnresolvableKeyedWaiverWaivesNothing pins the `ok` guard
// at coverage.go's waiver loop. It cannot be a CLI test: an unresolvable keyed
// waiver is a lint violation, and spec-coverage exits before it ever reaches
// ComputeCoverage — so the only place this behavior is observable is here.
//
// Dropping the ok check (`n, _ := waiverItemIndex(...)`) makes every
// unresolvable waiver waive item 0, because that is what the resolver returns
// alongside ok=false. The behavior below is built so that would be visible:
// item 0 is orphaned and nothing else is, so a spurious waiver flips exactly
// one verdict.
func TestComputeCoverage_UnresolvableKeyedWaiverWaivesNothing(t *testing.T) {
	files := []*File{{
		Domain:  "alpha",
		Prefix:  "ALP",
		Path:    "spec/alpha.yaml",
		Settled: []string{"ui"},
		Behaviors: []Behavior{{
			ID:      "ALP-001",
			Title:   "a behavior whose waiver names a key no item carries",
			Type:    "ux",
			Status:  "current",
			Surface: "ui",
			When:    "x",
			Then: []ThenItem{
				{Key: "first", Text: "the first outcome"},
				{Key: "second", Text: "the second outcome"},
			},
			Waivers: []Waiver{{Key: "no-such-key", Reason: "r"}},
		}},
	}}

	cov := ComputeCoverage(files, nil, nil)
	if len(cov.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(cov.Domains))
	}
	items := cov.Domains[0].Items
	for _, want := range []struct {
		ref   string
		state string
	}{
		{"ALP-001.first", ItemOrphan},
		{"ALP-001.second", ItemOrphan},
	} {
		it := findItem(items, want.ref)
		if it == nil {
			t.Fatalf("%s missing from the coverage report", want.ref)
		}
		if it.State != want.state {
			t.Errorf("%s is %q, want %q — an unresolvable keyed waiver must waive nothing", want.ref, it.State, want.state)
		}
		if it.Reason != "" {
			t.Errorf("%s carries waiver reason %q, want none", want.ref, it.Reason)
		}
	}
}

// TestComputeCoverage_StatementTokenWaiverOnGWTBehaviorWaivesNothing pins the
// ok=false half of waiverItemIndex's statement branch: the reserved "statement"
// token on a behavior that HAS no statement resolves to nothing, so it waives
// nothing.
//
// It cannot be a CLI test — spec-lint blocks that waiver before spec-coverage
// ever reaches ComputeCoverage — so a unit test is the only place the behavior
// is observable. (The ok=true half is already exercised end to end by
// testdata/coverage-keys' ALP-003 and its CLI golden.)
//
// Dropping the b.Statement != "" condition makes the token resolve to index 0
// on any behavior, silently waiving a real assertion. The fixture is built so
// that would be visible: both items are orphans, so a spurious waiver flips
// exactly one verdict.
func TestComputeCoverage_StatementTokenWaiverOnGWTBehaviorWaivesNothing(t *testing.T) {
	files := []*File{{
		Domain: "alpha",
		Prefix: "ALP",
		Path:   "spec/alpha.yaml",
		Behaviors: []Behavior{{
			ID:      "ALP-001",
			Title:   "a GWT behavior whose waiver uses the statement token",
			Type:    "ux",
			Status:  "current",
			Surface: "ui",
			When:    "x",
			Then: []ThenItem{
				{Key: "first", Text: "the first outcome"},
				{Key: "second", Text: "the second outcome"},
			},
			Waivers: []Waiver{{Key: waiverStatementKey, Reason: "r"}},
		}},
	}}

	cov := ComputeCoverage(files, nil, nil)
	if len(cov.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(cov.Domains))
	}
	items := cov.Domains[0].Items
	for _, ref := range []string{"ALP-001.first", "ALP-001.second"} {
		it := findItem(items, ref)
		if it == nil {
			t.Fatalf("%s missing from the coverage report", ref)
		}
		if it.State != ItemOrphan {
			t.Errorf("%s is %q, want %q — the statement token must waive nothing on a GWT behavior", ref, it.State, ItemOrphan)
		}
		if it.Reason != "" {
			t.Errorf("%s carries waiver reason %q, want none", ref, it.Reason)
		}
	}
}
