package spec

import (
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
		if len(b.Then) != 1 || b.Then[0] != "the row is persisted" {
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
		{"statement-on-gwt-type", 1, []string{"statement is only for invariant"}},
		{"invariant-with-gwt", 1, []string{"must not use given/when/then"}},
		{"invariant-missing-statement", 2, []string{"must have a non-empty statement"}},
		{"empty-list-item", 2, []string{"then list items must be non-empty", "given list items must be non-empty"}},
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

func TestLintAggregation(t *testing.T) {
	_, viol, err := Lint("testdata/aggregate")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	// All findings reported, no fail-fast.
	if len(viol) != 4 {
		t.Fatalf("want 4 aggregate violations, got %d:\n%s", len(viol), joinViolations(viol))
	}

	// Deterministic order (D15): sorted by file path, so a_broken precedes b_multi.
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
	// semantic violation (AGG-002 bad status) in the same file (D18).
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
