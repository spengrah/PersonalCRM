package spec

import (
	"fmt"
	"strings"
	"testing"
)

// thenItems builds an unkeyed then list from plain texts — the shape the
// parser produces for plain-string YAML items.
func thenItems(texts ...string) []ThenItem {
	out := make([]ThenItem, len(texts))
	for i, t := range texts {
		out[i] = ThenItem{Text: t}
	}
	return out
}

// baseGWT is a fully-populated GWT behavior; per-case mutations clone it and
// change exactly one field so each row isolates one axis of the comparison.
func baseGWT() Behavior {
	return Behavior{
		ID:         "X-001",
		Title:      "title",
		Type:       "business-logic",
		Status:     "current",
		Surface:    "api",
		Given:      []string{"g1"},
		When:       "w1",
		Then:       thenItems("t1", "t2"),
		Provenance: []string{"p1"},
		Notes:      "notes",
		Line:       7,
	}
}

// baseStatement is a statement (invariant) behavior for the statement-drift case.
func baseStatement() Behavior {
	return Behavior{
		ID:        "X-001",
		Title:     "title",
		Type:      "invariant",
		Status:    "current",
		Surface:   "api",
		Statement: "s1",
		Line:      7,
	}
}

func with(b Behavior, mut func(*Behavior)) Behavior {
	mut(&b)
	return b
}

func cite(path string, line int, id string) Citation {
	return Citation{Path: path, Line: line, ID: id, Then: -1}
}

const citeFile = "backend/x_test.go"

func TestSpecDrift(t *testing.T) {
	// The default citation set + empty changed set makes every case
	// discriminating: a warn fires iff the assertable content changed, so an
	// excluded-field row that stayed silent proves the field is not compared.
	defaultCites := []Citation{cite(citeFile, 10, "X-001")}

	tests := []struct {
		name          string
		base          []Behavior
		head          []Behavior
		cites         []Citation
		changed       []string
		wantIDs       []string          // expected warned IDs, in output order
		wantFileCount map[string]int    // optional: distinct citing-file count in the message
		wantCiteList  map[string]string // optional: exact sorted/deduped path:line list inside "(...)"
	}{
		// --- assertable-field changes → warn when the citing file is untouched ---
		{
			name:    "then changed",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.Then = thenItems("t1", "t2-changed") })},
			cites:   defaultCites,
			wantIDs: []string{"X-001"}, wantFileCount: map[string]int{"X-001": 1},
		},
		{
			name:    "then reorder",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.Then = thenItems("t2", "t1") })},
			cites:   defaultCites,
			wantIDs: []string{"X-001"},
		},
		{
			name:    "when changed",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites:   defaultCites,
			wantIDs: []string{"X-001"},
		},
		{
			name:    "given changed",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.Given = []string{"g2"} })},
			cites:   defaultCites,
			wantIDs: []string{"X-001"},
		},
		{
			name:    "statement changed",
			base:    []Behavior{baseStatement()},
			head:    []Behavior{with(baseStatement(), func(b *Behavior) { b.Statement = "s2" })},
			cites:   defaultCites,
			wantIDs: []string{"X-001"},
		},

		// --- excluded-field changes → silent (proves the exclusion list) ---
		{
			name: "title changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Title = "other" })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "notes changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Notes = "other" })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "provenance changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Provenance = []string{"p2"} })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "type changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Type = "api" })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "status changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Status = "proposed" })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "surface changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Surface = "ui" })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "serves changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Serves = []string{"Y-001"} })},
			cites: defaultCites, wantIDs: nil,
		},
		{
			name: "waivers changed", base: []Behavior{baseGWT()},
			head:  []Behavior{with(baseGWT(), func(b *Behavior) { b.Waivers = []Waiver{{Index: 0, Reason: "r"}} })},
			cites: defaultCites, wantIDs: nil,
		},

		// --- touch semantics ---
		{
			name:    "then changed + citing file touched → silent",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.Then = thenItems("t1", "t2x") })},
			cites:   defaultCites,
			changed: []string{citeFile},
			wantIDs: nil,
		},
		{
			name:    "then changed + no citing file touched → warn",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.Then = thenItems("t1", "t2x") })},
			cites:   defaultCites,
			changed: []string{"backend/unrelated_test.go"},
			wantIDs: []string{"X-001"},
		},

		// --- N-citation branches ---
		{
			name: "duplicate citations in one file count as one file → warn once",
			base: []Behavior{baseGWT()},
			head: []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites: []Citation{
				cite(citeFile, 10, "X-001"),
				cite(citeFile, 42, "X-001"),
			},
			wantIDs: []string{"X-001"}, wantFileCount: map[string]int{"X-001": 1},
			// Two distinct (path,line) pairs in one file: both listed, sorted.
			wantCiteList: map[string]string{"X-001": "backend/x_test.go:10, backend/x_test.go:42"},
		},
		{
			name: "exact-duplicate (path,line) citation deduped",
			base: []Behavior{baseGWT()},
			head: []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites: []Citation{
				cite(citeFile, 10, "X-001"),
				cite(citeFile, 10, "X-001"),
			},
			wantIDs: []string{"X-001"}, wantFileCount: map[string]int{"X-001": 1},
			wantCiteList: map[string]string{"X-001": "backend/x_test.go:10"},
		},
		{
			name: "multiple citing files all untouched → warn",
			base: []Behavior{baseGWT()},
			head: []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites: []Citation{
				cite("backend/b_test.go", 20, "X-001"),
				cite("backend/a_test.go", 10, "X-001"),
			},
			wantIDs: []string{"X-001"}, wantFileCount: map[string]int{"X-001": 2},
			// Assert the exact sorted list — proves ordering, not just the count.
			wantCiteList: map[string]string{"X-001": "backend/a_test.go:10, backend/b_test.go:20"},
		},
		{
			name: "multiple citing files one touched → silent",
			base: []Behavior{baseGWT()},
			head: []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites: []Citation{
				cite("backend/a_test.go", 10, "X-001"),
				cite("backend/b_test.go", 20, "X-001"),
			},
			changed: []string{"backend/b_test.go"},
			wantIDs: nil,
		},

		// --- partition / edge cases ---
		{
			name:    "zero-citation changed behavior → silent",
			base:    []Behavior{baseGWT()},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites:   nil,
			wantIDs: nil,
		},
		{
			name:    "newly-added ID → silent",
			base:    nil,
			head:    []Behavior{baseGWT()},
			cites:   defaultCites,
			wantIDs: nil,
		},
		{
			name:    "removed ID → silent",
			base:    []Behavior{baseGWT()},
			head:    nil,
			cites:   defaultCites,
			wantIDs: nil,
		},
		{
			name:    "empty base → silent",
			base:    []Behavior{},
			head:    []Behavior{with(baseGWT(), func(b *Behavior) { b.When = "w2" })},
			cites:   defaultCites,
			wantIDs: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := []*File{{Path: "spec/x.yaml", Behaviors: tc.base}}
			head := []*File{{Path: "spec/x.yaml", Behaviors: tc.head}}
			changed := map[string]bool{}
			for _, f := range tc.changed {
				changed[f] = true
			}

			got := SpecDrift(base, head, tc.cites, changed)

			var gotIDs []string
			for _, v := range got {
				gotIDs = append(gotIDs, v.Ref)
			}
			if !equalStrs(gotIDs, tc.wantIDs) {
				t.Fatalf("warned IDs = %v, want %v", gotIDs, tc.wantIDs)
			}
			for id, n := range tc.wantFileCount {
				want := fmt.Sprintf("its %d citing test file(s)", n)
				found := false
				for _, v := range got {
					if v.Ref == id && strings.Contains(v.Msg, want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("no warning for %s carrying %q; got %+v", id, want, got)
				}
			}
			for id, list := range tc.wantCiteList {
				want := "(" + list + ")"
				found := false
				for _, v := range got {
					if v.Ref == id && strings.Contains(v.Msg, want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("warning for %s missing exact citing list %q; got %+v", id, want, got)
				}
			}
		})
	}
}

// TestSpecDrift_KeyingOnlyEditIsSilent pins that minting then-item keys is not
// an assertion change. The keying pass gives every item a Key and shifts every
// Line (a keyed item occupies two YAML lines where a plain one occupied
// one) while changing no text — and drift must stay silent, or the migration
// would fire a warning storm across every cited behavior. Comparing ThenItem
// structs instead of ThenTexts() is exactly the mistake this catches.
func TestSpecDrift_KeyingOnlyEditIsSilent(t *testing.T) {
	base := []*File{{Path: "spec/x.yaml", Behaviors: []Behavior{baseGWT()}}}
	keyed := with(baseGWT(), func(b *Behavior) {
		b.Then = []ThenItem{
			{Key: "first-outcome", Text: "t1", Line: 40},
			{Key: "second-outcome", Text: "t2", Line: 42},
		}
	})
	head := []*File{{Path: "spec/x.yaml", Behaviors: []Behavior{keyed}}}

	if got := SpecDrift(base, head, []Citation{cite(citeFile, 10, "X-001")}, map[string]bool{}); len(got) != 0 {
		t.Fatalf("keying-only edit must not drift, got %+v", got)
	}

	// The same fixture with ONE text changed still warns, so the silence above
	// is the exclusion working rather than the comparison being dead.
	textChanged := with(keyed, func(b *Behavior) { b.Then[1].Text = "t2-changed" })
	head = []*File{{Path: "spec/x.yaml", Behaviors: []Behavior{textChanged}}}
	if got := SpecDrift(base, head, []Citation{cite(citeFile, 10, "X-001")}, map[string]bool{}); len(got) != 1 {
		t.Fatalf("a text change alongside keys must still warn, got %+v", got)
	}
}

// TestSpecDriftThenTexts pins the projection drift compares on.
func TestSpecDriftThenTexts(t *testing.T) {
	b := baseGWT()
	if got := b.ThenTexts(); !equalStrs(got, []string{"t1", "t2"}) {
		t.Errorf("ThenTexts() = %#v, want [t1 t2]", got)
	}
	b.Then = nil
	if got := b.ThenTexts(); got != nil {
		t.Errorf("an absent then must project to nil, got %#v", got)
	}
	b.Then = []ThenItem{}
	if got := b.ThenTexts(); got == nil || len(got) != 0 {
		t.Errorf("a present-but-empty then must project to an empty non-nil slice, got %#v", got)
	}
}

// TestSpecDriftOrdering_PathAndRef exercises the Path and Ref tie-breakers of
// the output sort (the line-only ordering test reaches neither). Two behaviors
// share a Path AND Line (Ref decides), plus one in a later-sorting Path at the
// same Line (Path decides).
func TestSpecDriftOrdering_PathAndRef(t *testing.T) {
	mk := func(id string, line int) Behavior {
		b := baseGWT()
		b.ID = id
		b.Line = line
		return b
	}
	drift := func(b Behavior) Behavior { return with(b, func(x *Behavior) { x.When = "w2" }) }

	base := []*File{
		{Path: "spec/a.yaml", Behaviors: []Behavior{mk("X-002", 5), mk("X-001", 5)}},
		{Path: "spec/b.yaml", Behaviors: []Behavior{mk("X-003", 5)}},
	}
	head := []*File{
		{Path: "spec/a.yaml", Behaviors: []Behavior{drift(mk("X-002", 5)), drift(mk("X-001", 5))}},
		{Path: "spec/b.yaml", Behaviors: []Behavior{drift(mk("X-003", 5))}},
	}
	cites := []Citation{
		cite("backend/a_test.go", 1, "X-001"),
		cite("backend/b_test.go", 1, "X-002"),
		cite("backend/c_test.go", 1, "X-003"),
	}

	got := SpecDrift(base, head, cites, map[string]bool{})
	want := []string{"X-001", "X-002", "X-003"} // a.yaml (Ref 001<002), then b.yaml
	if len(got) != 3 {
		t.Fatalf("want 3 warnings, got %d: %v", len(got), refs(got))
	}
	for i, w := range want {
		if got[i].Ref != w {
			t.Fatalf("order = %v, want %v", refs(got), want)
		}
	}
	// Path tie-break landed a.yaml before b.yaml.
	if got[0].Path != "spec/a.yaml" || got[2].Path != "spec/b.yaml" {
		t.Fatalf("path ordering = [%s ... %s], want spec/a.yaml ... spec/b.yaml", got[0].Path, got[2].Path)
	}
}

func refs(vs []Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Ref
	}
	return out
}

// TestSpecDriftOrdering pins the (Path, Line, Ref) output ordering so multiple
// warnings render deterministically.
func TestSpecDriftOrdering(t *testing.T) {
	mk := func(id string, line int) Behavior {
		b := baseGWT()
		b.ID = id
		b.Line = line
		return b
	}
	base := []*File{{Path: "spec/x.yaml", Behaviors: []Behavior{mk("X-001", 20), mk("X-002", 10)}}}
	head := []*File{{Path: "spec/x.yaml", Behaviors: []Behavior{
		with(mk("X-001", 20), func(b *Behavior) { b.When = "w2" }),
		with(mk("X-002", 10), func(b *Behavior) { b.When = "w2" }),
	}}}
	cites := []Citation{cite("backend/a_test.go", 1, "X-001"), cite("backend/b_test.go", 1, "X-002")}

	got := SpecDrift(base, head, cites, map[string]bool{})
	if len(got) != 2 {
		t.Fatalf("want 2 warnings, got %d: %+v", len(got), got)
	}
	// Same path, so Line orders: X-002 (line 10) before X-001 (line 20).
	if got[0].Ref != "X-002" || got[1].Ref != "X-001" {
		t.Fatalf("ordering = [%s, %s], want [X-002, X-001]", got[0].Ref, got[1].Ref)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
