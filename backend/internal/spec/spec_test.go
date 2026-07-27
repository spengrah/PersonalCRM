package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findBehavior returns the behavior with the given ID across the parsed files,
// or nil if absent.
func findBehavior(files []*File, id string) *Behavior {
	for _, f := range files {
		for i := range f.Behaviors {
			if f.Behaviors[i].ID == id {
				return &f.Behaviors[i]
			}
		}
	}
	return nil
}

func TestLintValidCorpus(t *testing.T) {
	files, viol, err := Lint("testdata/valid")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid corpus should be clean, got %d violations:\n%s", len(viol), joinViolations(viol))
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}

	total := 0
	for _, f := range files {
		total += len(f.Behaviors)
	}
	if total != 8 {
		t.Fatalf("want 8 behaviors across the corpus, got %d", total)
	}

	// Scalar given normalized to a one-element list.
	if b := findBehavior(files, "CON-001"); b == nil {
		t.Fatal("CON-001 missing")
	} else {
		if len(b.Given) != 1 || b.Given[0] != "a contact with methods and notes" {
			t.Errorf("CON-001 scalar given not normalized to one-element list: %#v", b.Given)
		}
		if len(b.Then) != 2 {
			t.Errorf("CON-001 want 2 then items, got %d", len(b.Then))
		}
		if len(b.Provenance) != 2 {
			t.Errorf("CON-001 want 2 provenance entries, got %#v", b.Provenance)
		}
		if b.Notes == "" {
			t.Error("CON-001 notes should be populated")
		}
	}

	// Invariant: statement present, GWT absent.
	if b := findBehavior(files, "CON-002"); b == nil {
		t.Fatal("CON-002 missing")
	} else {
		if b.Statement == "" {
			t.Error("CON-002 statement should be populated")
		}
		if b.When != "" || b.Given != nil || b.Then != nil {
			t.Errorf("CON-002 should carry no GWT: when=%q given=%#v then=%#v", b.When, b.Given, b.Then)
		}
	}

	// List given preserved; scalar then normalized to a one-element list.
	if b := findBehavior(files, "CON-003"); b == nil {
		t.Fatal("CON-003 missing")
	} else {
		if len(b.Given) != 2 {
			t.Errorf("CON-003 want 2 given items, got %#v", b.Given)
		}
		if len(b.Then) != 1 || b.Then[0].Text != "the row is persisted" {
			t.Errorf("CON-003 scalar then not normalized: %#v", b.Then)
		}
	}

	// Omitted given stays nil; 4-digit ID accepted.
	if b := findBehavior(files, "CON-1000"); b == nil {
		t.Fatal("CON-1000 missing")
	} else if b.Given != nil {
		t.Errorf("CON-1000 omitted given should be nil, got %#v", b.Given)
	}

	// Empty given list on a GWT behavior is valid and non-nil (present-but-empty).
	if b := findBehavior(files, "CAD-003"); b == nil {
		t.Fatal("CAD-003 missing")
	} else if b.Given == nil || len(b.Given) != 0 {
		t.Errorf("CAD-003 given: [] should be a non-nil empty slice, got %#v", b.Given)
	}
}

func TestLintViolationClasses(t *testing.T) {
	cases := []struct {
		dir       string
		wantCount int
		substrs   []string // every substring must appear somewhere in the output
	}{
		{"malformed-yaml", 1, []string{"YAML parse error"}},
		{"duplicate-keys", 2, []string{`duplicate key "behaviors"`, `duplicate key "when"`}},
		{"structural-types", 5, []string{
			"document root must be a mapping",
			"behaviors must be a sequence",
			"behavior entry must be a mapping",
			"title must be a scalar",
			"domain must be a scalar",
		}},
		{"non-string-scalars", 3, []string{"when must be a string", "statement must be a string", "then items must be strings"}},
		{"missing-file-fields", 3, []string{`missing required field "domain"`, `missing required field "prefix"`, `missing required field "maturity"`}},
		{"missing-behaviors-key", 1, []string{`missing required field "behaviors"`}},
		{"bad-maturity", 1, []string{"invalid maturity"}},
		{"bad-type", 1, []string{"invalid type"}},
		{"bad-status", 1, []string{"invalid status"}},
		{"missing-behavior-fields", 4, []string{
			`missing required field "id"`,
			`missing required field "title"`,
			`missing required field "type"`,
			`missing required field "status"`,
		}},
		{"id-bad-format", 3, []string{`id "XXX-1" must match`, `id "XXX-0001" must match`, `id "xxx-002" must match`}},
		{"id-prefix-mismatch", 1, []string{"must match AAA-NNN"}},
		{"id-dup-in-file", 1, []string{`duplicate id "DUP-001" within file`}},
		{"id-dup-cross-file", 4, []string{`id "SHR-001" is not unique across files`, `prefix "SHR" is not unique across files`}},
		{"prefix-dup-cross-file", 2, []string{`prefix "PFX" is not unique across files`}},
		{"when-list", 1, []string{"when must be a single string"}},
		{"when-missing", 1, []string{"must have a non-empty when"}},
		{"when-empty", 2, []string{"must have a non-empty when"}},
		{"then-missing", 1, []string{"must have a then with at least one outcome"}},
		{"then-empty", 1, []string{"must have a then with at least one outcome"}},
		{"statement-on-gwt-type", 1, []string{"statement is only for invariant and intent"}},
		{"invariant-with-gwt", 1, []string{"must not use given/when/then"}},
		{"invariant-missing-statement", 2, []string{"must have a non-empty statement"}},
		{"intent-with-gwt", 1, []string{"intent behavior must not use given/when/then"}},
		{"intent-missing-statement", 1, []string{"intent behavior must have a non-empty statement"}},
		{"serves-on-wrong-type", 1, []string{`serves is only for ux and intent behaviors (type "api")`}},
		{"serves-unknown-target", 1, []string{`serves target "ZZZ-999" does not exist in the corpus`}},
		{"serves-non-intent-target", 1, []string{`serves target "CON-001" is not an intent behavior (type "ux")`}},
		{"serves-self", 1, []string{`serves target "CON-001" is the behavior itself`}},
		{"serves-empty-item", 1, []string{"serves list items must be non-empty"}},
		{"serves-not-list", 1, []string{"serves must be a string or a list of strings"}},
		{"empty-list-item", 2, []string{"then list items must be non-empty", "given list items must be non-empty"}},
		// A structurally broken GWT/statement field on the "wrong" type must
		// yield ONLY its structural violation — the count of 2 (one per
		// behavior) proves the type-based presence checks did not re-report
		// the same broken fields.
		{"broken-field-no-cascade", 2, []string{"when must be a single string", "statement must be a string"}},
		{"surface-bad-value", 1, []string{`invalid surface "browser" (want ui|api|none)`}},
		{"surface-missing", 1, []string{`missing required field "surface"`}},
		{"surface-on-intent", 1, []string{"surface is not for intent behaviors"}},
		{"waivers-on-non-ui", 1, []string{`waivers are only for ui- or api-surface behaviors (surface "none")`}},
		{"waivers-on-intent", 1, []string{"waivers are not for intent behaviors"}},
		// TWO behaviors, one retired-form waiver each: behaviorWaivers fails
		// closed per FIELD, so two waivers on one behavior would report once
		// and the negative case would never be reached.
		{"waiver-index-form", 2, []string{
			"waiver then 1 uses the retired positional form; waive the then item by its key",
			"waiver then -1 uses the retired positional form; waive the then item by its key",
		}},
		{"waivers-dup-key", 1, []string{`duplicate waiver for then item "outcome-z"`}},
		{"waivers-empty-reason", 1, []string{"waiver reason must be non-empty"}},
		{"waivers-bad-shape", 5, []string{
			"waivers must be a list of {then, reason} mappings",
			"waivers items must be {then, reason} mappings",
			"waiver then must be a then-item key",
			"waiver must have a reason",
			"waiver reason must be a string",
		}},
		{"settled-not-list", 1, []string{"settled must be a list of surfaces"}},
		{"settled-mapping", 1, []string{"settled must be a list of surfaces"}},
		{"settled-null", 1, []string{"settled must list at least one surface; omit the key entirely for a genuinely unsettled domain"}},
		{"settled-empty-list", 1, []string{"settled must list at least one surface; omit the key entirely for a genuinely unsettled domain"}},
		{"settled-bad-surface", 1, []string{`invalid settled surface "browser" (want ui|api)`}},
		{"settled-none", 1, []string{`settled surface "none" is not yet supported`}},
		{"settled-dup", 1, []string{`duplicate settled surface "ui"`}},
		{"settled-non-string-item", 1, []string{"settled items must be surfaces"}},
		{"legacy-e2e-settled", 1, []string{"e2e_settled is retired; use a settled: [ui] list"}},
		{"prefix-bad-format", 1, []string{`prefix "Gc" must be uppercase alphanumeric starting with a letter`}},
		// --- then-item keys ---
		{"then-key-bad-charset", 3, []string{
			`then item key "bad_key" must be lowercase alphanumeric words separated by hyphens`,
			`then item key "BadKey" must be lowercase alphanumeric words separated by hyphens`,
			`then item key "trailing-" must be lowercase alphanumeric words separated by hyphens`,
		}},
		{"then-key-empty", 3, []string{"then item key must be a non-empty string"}},
		{"then-key-reserved", 1, []string{`then item key "statement" is reserved for statement-behavior waivers`}},
		{"then-key-dup", 1, []string{`duplicate then item key "same-key"`}},
		{"then-text-empty", 1, []string{"then list items must be non-empty strings"}},
		{"then-item-bad-shape", 5, []string{
			"then items must be strings or {key, text} mappings",
			`unknown key "txt" in then item mapping (want key, text)`,
			"then item mapping must have a key and a text",
			`duplicate key "text" in then item mapping`,
			"then item text must be a string",
		}},
		{"waiver-key-unknown", 1, []string{`waiver then "no-such-key" names no then-item key of this behavior`}},
		// An empty waiver key resolves to nothing, so it waives nothing. The
		// fixture's then[1] is a PLAIN STRING on purpose: an unkeyed item
		// carries an empty key, so without waiverItemIndex's it.Key != ""
		// guard this waiver would match it and silently waive a real
		// assertion at whatever index the unkeyed item happens to sit.
		{"waiver-key-empty", 1, []string{`waiver then "" names no then-item key of this behavior`}},
		// Both directions of misusing the reserved token, each with its own
		// message — the generic unknown-key wording would misdirect either way.
		{"waiver-statement-bad-key", 2, []string{
			`waiver then "some-key" is not valid for a statement behavior (use the reserved key "statement")`,
			`waiver then "statement" is reserved for statement behaviors; waive a then item of this behavior by its key`,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			_, viol, err := Lint("testdata/invalid/" + tc.dir)
			if err != nil {
				t.Fatalf("Lint returned error: %v", err)
			}
			if len(viol) != tc.wantCount {
				t.Fatalf("want %d violations, got %d:\n%s", tc.wantCount, len(viol), joinViolations(viol))
			}
			out := joinViolations(viol)
			for _, sub := range tc.substrs {
				if !strings.Contains(out, sub) {
					t.Errorf("output missing substring %q; got:\n%s", sub, out)
				}
			}
		})
	}
}

// TestLintServesAndIntent pins the intent/serves additions on a valid corpus:
// intent behaviors carry a statement and no GWT, serves normalizes a scalar to
// a one-element list, and cross-file serves references (including an intent
// serving a broader intent) resolve clean.
func TestLintServesAndIntent(t *testing.T) {
	files, viol, err := Lint("testdata/valid-serves")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid-serves corpus should be clean, got %d violations:\n%s", len(viol), joinViolations(viol))
	}

	if b := findBehavior(files, "DSH-001"); b == nil {
		t.Fatal("DSH-001 missing")
	} else {
		if b.Type != "intent" || b.Statement == "" {
			t.Errorf("DSH-001 should be an intent with a statement, got type=%q statement=%q", b.Type, b.Statement)
		}
		if b.Serves != nil {
			t.Errorf("DSH-001 omitted serves should be nil, got %#v", b.Serves)
		}
	}

	// Scalar serves normalized to a one-element list (same-file reference).
	if b := findBehavior(files, "DSH-002"); b == nil {
		t.Fatal("DSH-002 missing")
	} else if len(b.Serves) != 1 || b.Serves[0] != "DSH-001" {
		t.Errorf("DSH-002 scalar serves not normalized: %#v", b.Serves)
	}

	// Cross-file ux→intent and intent→intent references resolve.
	if b := findBehavior(files, "CAD-001"); b == nil {
		t.Fatal("CAD-001 missing")
	} else if len(b.Serves) != 1 || b.Serves[0] != "DSH-001" {
		t.Errorf("CAD-001 cross-file serves not parsed: %#v", b.Serves)
	}
	if b := findBehavior(files, "CAD-002"); b == nil {
		t.Fatal("CAD-002 missing")
	} else if b.Type != "intent" || len(b.Serves) != 1 {
		t.Errorf("CAD-002 should be an intent serving another intent: type=%q serves=%#v", b.Type, b.Serves)
	}
}

// TestLintSurfaceAndWaivers pins the surface/waivers/settled additions on a
// valid corpus: a ui-surface behavior carries typed waivers, an api-surface
// behavior may also carry a waiver, a retired behavior needs no surface, an
// intent takes no surface, and the file-level settled list parses as [ui].
func TestLintSurfaceAndWaivers(t *testing.T) {
	files, viol, err := Lint("testdata/valid-surface")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid-surface corpus should be clean, got %d violations:\n%s", len(viol), joinViolations(viol))
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if len(files[0].Settled) != 1 || files[0].Settled[0] != "ui" {
		t.Fatalf("settled: [ui] should parse onto the file, got %#v", files[0].Settled)
	}

	if b := findBehavior(files, "CON-001"); b == nil {
		t.Fatal("CON-001 missing")
	} else {
		if b.Surface != "ui" {
			t.Errorf("CON-001 surface = %q, want ui", b.Surface)
		}
		if len(b.Waivers) != 1 || b.Waivers[0].Key != "refocus-rechecks-freshness" || b.Waivers[0].Reason == "" {
			t.Errorf("CON-001 waivers not parsed: %#v", b.Waivers)
		}
	}
	if b := findBehavior(files, "CON-002"); b == nil {
		t.Fatal("CON-002 missing")
	} else if b.Surface != "" {
		t.Errorf("CON-002 retired behavior should carry no surface, got %q", b.Surface)
	}
	if b := findBehavior(files, "CON-004"); b == nil {
		t.Fatal("CON-004 missing")
	} else if b.Surface != "" || b.Waivers != nil {
		t.Errorf("CON-004 intent should carry no surface/waivers: %#v", b)
	}
	// An api-surface behavior may carry a waiver (relaxed from ui-only).
	if b := findBehavior(files, "CON-005"); b == nil {
		t.Fatal("CON-005 missing")
	} else if b.Surface != "api" || len(b.Waivers) != 1 || b.Waivers[0].Key != "body-is-chunked" || b.Waivers[0].Reason == "" {
		t.Errorf("CON-005 api-surface waiver not parsed clean: %#v", b)
	}
}

// TestLintSettledAbsent pins that an omitted settled key is legal — a genuinely
// unsettled domain — and parses as a nil surface list with no violation. (An
// explicit-but-empty settled, null or [], is rejected instead; see the
// settled-null / settled-empty-list cases in TestLintViolationClasses.)
func TestLintSettledAbsent(t *testing.T) {
	files, viol, err := Lint("testdata/valid-settled-absent")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid-settled-absent corpus should be clean, got %d violations:\n%s", len(viol), joinViolations(viol))
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Settled != nil {
		t.Errorf("absent settled should parse as nil, got %#v", files[0].Settled)
	}
}

// TestLintThenKeysAndKeyedWaivers pins the keyed then-item form on a valid
// corpus: keyed and plain items coexist in one then list, a keyed item carries
// its Key/Text/Line, a plain one carries an empty Key, a waiver addresses an
// item by key, and the reserved "statement" token addresses a statement
// behavior's implicit item.
func TestLintThenKeysAndKeyedWaivers(t *testing.T) {
	files, viol, err := Lint("testdata/valid-then-keys")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid-then-keys corpus should be clean, got %d violations:\n%s", len(viol), joinViolations(viol))
	}

	b := findBehavior(files, "KEY-001")
	if b == nil {
		t.Fatal("KEY-001 missing")
	}
	if len(b.Then) != 3 {
		t.Fatalf("KEY-001 want 3 then items, got %#v", b.Then)
	}
	want := []ThenItem{
		{Key: "live-contact-flag", Text: "a live contact is returned with a has_pending_followup flag", Line: 13},
		{Key: "", Text: "an unkeyed outcome stays a plain string", Line: 15},
		{Key: "pending-followup-count", Text: "the pending followup count is returned", Line: 16},
	}
	for i, w := range want {
		if b.Then[i] != w {
			t.Errorf("KEY-001 then[%d] = %#v, want %#v", i, b.Then[i], w)
		}
	}
	if got := b.ThenTexts(); len(got) != 3 || got[0] != want[0].Text || got[1] != want[1].Text {
		t.Errorf("KEY-001 ThenTexts() = %#v", got)
	}
	if len(b.Waivers) != 1 || b.Waivers[0].Key != "pending-followup-count" {
		t.Errorf("KEY-001 keyed waiver not parsed: %#v", b.Waivers)
	}

	if b := findBehavior(files, "KEY-002"); b == nil {
		t.Fatal("KEY-002 missing")
	} else if len(b.Waivers) != 1 || b.Waivers[0].Key != "statement" {
		t.Errorf("KEY-002 statement waiver not parsed: %#v", b.Waivers)
	}
}

// TestParserThenMessagesAreDistinct guards the message split that keeps this
// change inert. A single widened message ("then items must be strings or
// {key, text} mappings") would still satisfy the pre-existing
// strings.Contains assertion on non-string-scalars/ as a PREFIX, so the suite
// would stay green while the stated byte-identity invariant was broken. This
// asserts the pre-existing case's message by EQUALITY, and that the two
// messages are different strings.
func TestParserThenMessagesAreDistinct(t *testing.T) {
	const scalarMsg = "then items must be strings"
	const shapeMsg = "then items must be strings or {key, text} mappings"
	if scalarMsg == shapeMsg {
		t.Fatal("the two then-item messages must be distinct")
	}
	if !strings.HasPrefix(shapeMsg, scalarMsg) {
		t.Fatal("precondition: the widened message shares the old one's prefix — that is exactly why equality is required below")
	}

	_, viol, err := Lint("testdata/invalid/non-string-scalars")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	want := Violation{
		Path: "testdata/invalid/non-string-scalars/bad.yaml",
		Ref:  "CON-003",
		Line: 26,
		Msg:  scalarMsg,
	}
	found := false
	for _, v := range viol {
		if v.Ref == "CON-003" {
			found = true
			if v != want {
				t.Errorf("CON-003 violation = %#v, want %#v", v, want)
			}
		}
	}
	if !found {
		t.Fatalf("no CON-003 violation in:\n%s", joinViolations(viol))
	}

	// The non-scalar, non-mapping item gets the OTHER message. The found guard
	// matters as much as the comparison: without it a change that dropped the
	// violation entirely would satisfy an empty loop.
	_, viol, err = Lint("testdata/invalid/then-item-bad-shape")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	found = false
	for _, v := range viol {
		if v.Ref == "CON-001" {
			found = true
			if v.Msg != shapeMsg {
				t.Errorf("CON-001 msg = %q, want %q", v.Msg, shapeMsg)
			}
		}
	}
	if !found {
		t.Fatalf("no CON-001 violation in:\n%s", joinViolations(viol))
	}
}

// TestLint_ThenKeyDuplicateIgnoresEmptyKeys pins the non-empty qualifier on
// key uniqueness. Without it every behavior with two or more plain-string then
// items would report `duplicate then item key ""` and the whole corpus would
// fail lint — testdata/valid's CON-001 has exactly that shape.
func TestLint_ThenKeyDuplicateIgnoresEmptyKeys(t *testing.T) {
	files, viol, err := Lint("testdata/valid")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	b := findBehavior(files, "CON-001")
	if b == nil {
		t.Fatal("CON-001 missing")
	}
	if len(b.Then) < 2 {
		t.Fatalf("precondition: CON-001 must have >=2 unkeyed then items, got %#v", b.Then)
	}
	for _, it := range b.Then {
		if it.Key != "" {
			t.Fatalf("precondition: CON-001's then items must all be unkeyed, got %#v", b.Then)
		}
	}
	if strings.Contains(joinViolations(viol), "duplicate then item key") {
		t.Errorf("unkeyed items must not collide:\n%s", joinViolations(viol))
	}
}

// TestLint_WaiverIndexFormRejected pins the retirement of the !!int waiver
// form at the PARSER tier, including the negative case. The count of 2 is the
// assertion that matters: the fixture is two behaviors with one retired-form
// waiver each precisely because behaviorWaivers fails closed per FIELD, so a
// single behavior carrying both would report once and `then: -1` would ship
// untested. A `>= 1` assertion would pass on that weaker fixture.
//
// The message renders the RAW scalar text, so `then: -1` says -1 rather than
// being silently reclassified into the residual shape message.
func TestLint_WaiverIndexFormRejected(t *testing.T) {
	files, viol, err := Lint("testdata/invalid/waiver-index-form")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 2 {
		t.Fatalf("want exactly 2 violations (one per behavior), got %d:\n%s", len(viol), joinViolations(viol))
	}
	all := joinViolations(viol)
	for _, want := range []string{
		"waiver then 1 uses the retired positional form; waive the then item by its key",
		"waiver then -1 uses the retired positional form; waive the then item by its key",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("violations missing %q; got:\n%s", want, all)
		}
	}
	// Fail-closed: the whole waivers field is marked broken, so nothing is
	// carried into the semantic pass.
	for _, id := range []string{"CON-001", "CON-002"} {
		if b := findBehavior(files, id); b == nil {
			t.Errorf("%s missing", id)
		} else if len(b.Waivers) != 0 {
			t.Errorf("%s: a retired-form waiver must not parse, got %#v", id, b.Waivers)
		}
	}
}

// TestLintThenItemBrokenSuppressesSemantics pins the tiered-degradation
// contract for the new walker: the FIRST structural deviation in a then list
// reports once and suppresses every then-dependent semantic check below it —
// key charset/uniqueness, the empty-text check, and waiver key resolution.
func TestLintThenItemBrokenSuppressesSemantics(t *testing.T) {
	_, viol, err := Lint("testdata/invalid/then-broken-suppresses")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if len(viol) != 1 {
		t.Fatalf("a broken then must report exactly once, got %d:\n%s", len(viol), joinViolations(viol))
	}
	if !strings.Contains(viol[0].Msg, `unknown key "txt" in then item mapping`) {
		t.Errorf("want the first structural deviation, got %q", viol[0].Msg)
	}
	out := joinViolations(viol)
	for _, suppressed := range []string{
		"duplicate then item key",
		"then list items must be non-empty strings",
		"names no then-item key of this behavior",
		"must have a then with at least one outcome",
	} {
		if strings.Contains(out, suppressed) {
			t.Errorf("a broken then must suppress %q; got:\n%s", suppressed, out)
		}
	}

	// Fail-closed is the other half of the contract and the half that survives
	// into the EXPORTED output: a broken then must parse to nil, never to a
	// partial list with the offending item silently dropped. A dropped item
	// would delete a coverage obligation while lint stayed at one violation, so
	// the violation count above cannot detect it on its own.
	files, _, err := ParseDir("testdata/invalid/then-broken-suppresses")
	if err != nil {
		t.Fatalf("ParseDir returned error: %v", err)
	}
	b := findBehavior(files, "CON-001")
	if b == nil {
		t.Fatal("CON-001 missing from the parsed output")
	}
	if b.Then != nil {
		t.Errorf("a structurally broken then must parse to nil, got %#v", b.Then)
	}
}

// TestParserThenScalarForms pins the scalar branches of the then walker, which
// the fixture corpus only exercises in its sequence form. Each broken form must
// yield nil — never a partially-populated list and never an empty non-nil slice
// — because absent-vs-present-but-empty is decidable off exactly that
// distinction downstream.
func TestParserThenScalarForms(t *testing.T) {
	const header = "domain: contacts\nprefix: CON\nmaturity: reviewed\nbehaviors:\n  - id: CON-001\n    title: t\n    type: business-logic\n    status: current\n    surface: none\n    when: w\n    "
	cases := []struct {
		name    string
		then    string
		wantLen int // -1 = nil
		wantNil bool
		wantMsg string // "" = no violation
	}{
		{"string scalar normalizes to one item", "then: a single outcome", 1, false, ""},
		{"null is absent-equivalent", "then: null", 0, true, ""},
		{"int scalar keeps the original message", "then: 42", 0, true, "then items must be strings"},
		{"bool scalar keeps the original message", "then: true", 0, true, "then items must be strings"},
		{"mapping keeps the original message", "then:\n      a: b", 0, true, "then must be a string or a list of strings"},
		{"empty sequence is present-but-empty", "then: []", 0, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "spec.yaml")
			if err := os.WriteFile(path, []byte(header+tc.then+"\n"), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			// ParseDir is parse-only, so no semantic noise (a missing then would
			// otherwise also report "must have a then with at least one outcome").
			files, viol, err := ParseDir(dir)
			if err != nil {
				t.Fatalf("ParseDir returned error: %v", err)
			}
			if tc.wantMsg == "" {
				if len(viol) != 0 {
					t.Fatalf("want no violations, got:\n%s", joinViolations(viol))
				}
			} else {
				if len(viol) != 1 {
					t.Fatalf("want 1 violation, got %d:\n%s", len(viol), joinViolations(viol))
				}
				if viol[0].Msg != tc.wantMsg {
					t.Errorf("msg = %q, want %q", viol[0].Msg, tc.wantMsg)
				}
			}
			b := findBehavior(files, "CON-001")
			if b == nil {
				t.Fatal("CON-001 missing")
			}
			if tc.wantNil && b.Then != nil {
				t.Fatalf("want a nil then, got %#v", b.Then)
			}
			if !tc.wantNil && b.Then == nil {
				t.Fatal("want a non-nil then, got nil")
			}
			if len(b.Then) != tc.wantLen {
				t.Fatalf("want %d then items, got %#v", tc.wantLen, b.Then)
			}
			if tc.wantLen == 1 && b.Then[0].Text != "a single outcome" {
				t.Errorf("scalar then not normalized: %#v", b.Then)
			}
		})
	}
}

func TestLintAggregation(t *testing.T) {
	_, viol, err := Lint("testdata/aggregate")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	// All findings reported, no fail-fast.
	if len(viol) != 4 {
		t.Fatalf("want 4 aggregate violations, got %d:\n%s", len(viol), joinViolations(viol))
	}

	// Deterministic order: sorted by file path, so a_broken precedes b_multi.
	if !strings.HasSuffix(viol[0].Path, "a_broken.yaml") {
		t.Errorf("first violation should be from a_broken.yaml, got %s", viol[0].Path)
	}
	for _, v := range viol[1:] {
		if !strings.HasSuffix(v.Path, "b_multi.yaml") {
			t.Errorf("violations after the first should be from b_multi.yaml, got %s", v.Path)
		}
	}

	// The file-tier-broken file contributes ONLY its parse violation.
	broken := 0
	for _, v := range viol {
		if strings.HasSuffix(v.Path, "a_broken.yaml") {
			broken++
			if !strings.Contains(v.Msg, "YAML parse error") {
				t.Errorf("a_broken.yaml should contribute only a parse violation, got %q", v.Msg)
			}
		}
	}
	if broken != 1 {
		t.Errorf("a_broken.yaml should contribute exactly 1 violation, got %d", broken)
	}

	// A behavior-field-tier break (AGG-001 then) does NOT suppress a sibling's
	// semantic violation (AGG-002 bad status) in the same file.
	out := joinViolations(viol)
	if !strings.Contains(out, "AGG-001") || !strings.Contains(out, "then items must be strings") {
		t.Errorf("expected AGG-001's structural then violation; got:\n%s", out)
	}
	if !strings.Contains(out, "AGG-002") || !strings.Contains(out, "invalid status") {
		t.Errorf("expected AGG-002's semantic status violation (not suppressed by sibling break); got:\n%s", out)
	}
	// The file-level maturity violation is present too.
	if !strings.Contains(out, "invalid maturity") {
		t.Errorf("expected b_multi.yaml's file-level maturity violation; got:\n%s", out)
	}
}

func TestLintEmptyAndAbsentCases(t *testing.T) {
	// A directory with no *.yaml files lints clean (README.md ignored).
	files, viol, err := Lint("testdata/empty")
	if err != nil {
		t.Fatalf("empty dir returned error: %v", err)
	}
	if len(files) != 0 || len(viol) != 0 {
		t.Fatalf("empty dir want 0 files/0 violations, got %d/%d", len(files), len(viol))
	}

	// A file with behaviors: [] parses clean.
	files, viol, err = Lint("testdata/valid-empty-behaviors")
	if err != nil {
		t.Fatalf("behaviors:[] dir returned error: %v", err)
	}
	if len(files) != 1 || len(viol) != 0 {
		t.Fatalf("behaviors:[] want 1 file/0 violations, got %d/%d:\n%s", len(files), len(viol), joinViolations(viol))
	}
	if len(files[0].Behaviors) != 0 {
		t.Errorf("behaviors:[] should yield 0 behaviors, got %d", len(files[0].Behaviors))
	}

	// A nonexistent directory is an operational error.
	if _, _, err := Lint("testdata/does-not-exist"); err == nil {
		t.Error("nonexistent dir should return an error")
	}
}

// TestParseDirIsParseOnly pins the parse/semantic split ParseDir gives Piece 3:
// it reports structural violations but NOT semantic ones.
func TestParseDirIsParseOnly(t *testing.T) {
	// bad-status is a purely semantic violation — ParseDir must not report it.
	files, viol, err := ParseDir("testdata/invalid/bad-status")
	if err != nil {
		t.Fatalf("ParseDir returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Errorf("ParseDir should not report semantic violations, got %d:\n%s", len(viol), joinViolations(viol))
	}
	if len(files) != 1 {
		t.Fatalf("want 1 parsed file, got %d", len(files))
	}

	// malformed-yaml is a parse/structural violation — ParseDir must report it.
	_, viol, err = ParseDir("testdata/invalid/malformed-yaml")
	if err != nil {
		t.Fatalf("ParseDir returned error: %v", err)
	}
	if len(viol) != 1 {
		t.Fatalf("ParseDir should report the parse violation, got %d", len(viol))
	}
}

// TestSkippedEntriesExcludedFromBehaviors pins that a structurally unusable
// behaviors[i] entry (reported as a violation) is NOT exported as a zero-value
// stub in File.Behaviors — downstream consumers must never see a phantom
// all-empty behavior.
func TestSkippedEntriesExcludedFromBehaviors(t *testing.T) {
	files, _, err := ParseDir("testdata/invalid/structural-types")
	if err != nil {
		t.Fatalf("ParseDir returned error: %v", err)
	}
	var target *File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "entry-and-field.yaml") {
			target = f
		}
	}
	if target == nil {
		t.Fatal("entry-and-field.yaml not among parsed files")
	}
	// The fixture has two entries: a bare-string entry (skipped) and KNW-001.
	if len(target.Behaviors) != 1 {
		t.Fatalf("want 1 exported behavior (skipped entry excluded), got %d: %#v", len(target.Behaviors), target.Behaviors)
	}
	if target.Behaviors[0].ID != "KNW-001" {
		t.Errorf("surviving behavior should be KNW-001, got %q", target.Behaviors[0].ID)
	}
}

func TestParseFile(t *testing.T) {
	file, viol, err := ParseFile("testdata/valid/contacts.yaml")
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid file should have no parse violations, got %d", len(viol))
	}
	if file.Domain != "contacts" || file.Prefix != "CON" || file.Maturity != "reviewed" {
		t.Errorf("unexpected file header: %+v", *file)
	}
	if file.Path != "testdata/valid/contacts.yaml" {
		t.Errorf("Path not set on parsed file: %q", file.Path)
	}
	if len(file.Behaviors) != 5 {
		t.Errorf("want 5 behaviors, got %d", len(file.Behaviors))
	}

	// A read error is an IO-level error.
	if _, _, err := ParseFile("testdata/valid/does-not-exist.yaml"); err == nil {
		t.Error("ParseFile on a missing file should error")
	}
}

// TestParseFileThenItemLines pins that keyed items survive the parse-only path
// (ParseFile, which the traceability scanner and the drift CLI both use) and
// that ThenItem.Line carries the item's own source line — the anchor a
// line-oriented rewriter needs, since a waiver's `- then: 1` entry shares the
// six-space indent of a then item and cannot be told apart by indentation.
func TestParseFileThenItemLines(t *testing.T) {
	file, viol, err := ParseFile("testdata/valid-then-keys/spec.yaml")
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if len(viol) != 0 {
		t.Fatalf("valid file should have no parse violations:\n%s", joinViolations(viol))
	}
	b := findBehavior([]*File{file}, "KEY-001")
	if b == nil {
		t.Fatal("KEY-001 missing")
	}
	// Every item's Line must name a line that actually holds it, and the lines
	// must strictly increase down the list.
	src := strings.Split(readFixture(t, "testdata/valid-then-keys/spec.yaml"), "\n")
	prev := 0
	for i, it := range b.Then {
		if it.Line <= prev {
			t.Errorf("then[%d].Line = %d is not after the previous item's %d", i, it.Line, prev)
		}
		prev = it.Line
		if it.Line < 1 || it.Line > len(src) {
			t.Fatalf("then[%d].Line = %d is outside the file", i, it.Line)
		}
		line := src[it.Line-1]
		if it.Key != "" {
			if !strings.Contains(line, "key: "+it.Key) {
				t.Errorf("then[%d].Line %d = %q, expected the `key: %s` line", i, it.Line, line, it.Key)
			}
		} else if !strings.Contains(line, it.Text) {
			t.Errorf("then[%d].Line %d = %q, expected the item's own text", i, it.Line, line)
		}
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(data)
}

func TestViolationString(t *testing.T) {
	cases := []struct {
		v    Violation
		want string
	}{
		{Violation{Path: "spec/a.yaml", Ref: "CON-001", Line: 5, Msg: "boom"}, "spec/a.yaml:5: CON-001: boom"},
		{Violation{Path: "spec/a.yaml", Line: 5, Msg: "boom"}, "spec/a.yaml:5: boom"},
		{Violation{Path: "spec/a.yaml", Ref: "CON-001", Msg: "boom"}, "spec/a.yaml: CON-001: boom"},
		{Violation{Path: "spec/a.yaml", Msg: "boom"}, "spec/a.yaml: boom"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func joinViolations(viol []Violation) string {
	var sb strings.Builder
	for _, v := range viol {
		sb.WriteString(v.String())
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestParser_UnknownThenSubKeyReportsTheSubKeyLine pins WHICH line an unknown
// then-item sub-key is reported at: the offending sub-key's own line, not the
// mapping's `- key:` line above it. The message itself is already pinned
// elsewhere; the reported LINE is what tells the author where to look, and
// nothing asserted it.
//
// The expected line is COMPUTED from the fixture rather than hard-coded: a
// fixture edit that moved the sub-key would otherwise silently invert this into
// a tautology (asserting whatever the parser happens to emit).
func TestParser_UnknownThenSubKeyReportsTheSubKeyLine(t *testing.T) {
	const path = "testdata/invalid/then-item-bad-shape/bad.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The offending sub-key ("txt:") and the mapping line that opens its item
	// ("- key:" immediately above), both located in the fixture text.
	var subKeyLine, mappingLine int
	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "txt:") {
			subKeyLine = i + 1
			mappingLine = i // the `- key: some-key` line directly above
		}
	}
	if subKeyLine == 0 {
		t.Fatalf("precondition: the fixture must carry a `txt:` sub-key")
	}
	if subKeyLine == mappingLine {
		t.Fatal("precondition: the sub-key and the mapping line must differ, or the assertion proves nothing")
	}

	_, viol, err := Lint("testdata/invalid/then-item-bad-shape")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	want := Violation{
		Path: path,
		Ref:  "CON-002",
		Line: subKeyLine,
		Msg:  `unknown key "txt" in then item mapping (want key, text)`,
	}
	found := false
	for _, v := range viol {
		if v.Ref == "CON-002" {
			found = true
			if v != want {
				t.Errorf("CON-002 violation = %#v, want %#v (line %d is the mapping's, not the sub-key's)", v, want, mappingLine)
			}
		}
	}
	if !found {
		t.Fatalf("no CON-002 violation in:\n%s", joinViolations(viol))
	}
}
