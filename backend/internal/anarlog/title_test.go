package anarlog

import (
	"reflect"
	"strings"
	"testing"
)

// TestExtractNameTokens covers the D2 fixture table from the PR plan
// plus the word-boundary cases added per Codex round-1 review.
func TestExtractNameTokens(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		// Meta-token stripping.
		{"strip_1to1", "Alice / Bob 1:1", []string{"Alice", "Bob"}},
		{"strip_sync", "Alice sync", []string{"Alice"}},
		{"strip_catchup", "Catchup with Alice and Bob", []string{"Alice", "Bob"}},
		{"strip_intro", "intro Alice", []string{"Alice"}},
		{"strip_standup", "Standup with Alice", []string{"Alice"}},
		{"strip_huddle", "Huddle with Alice", []string{"Alice"}},
		{"strip_check_in_hyphen", "Check-in with Alice", []string{"Alice"}},
		{"strip_checkin", "Checkin with Alice", []string{"Alice"}},
		{"strip_review_word", "Alice review", []string{"Alice"}},
		{"strip_one_dash_one", "Alice 1-1", []string{"Alice"}},
		{"strip_one_on_one", "Alice one-on-one", []string{"Alice"}},

		// Date stripping.
		{"strip_date_iso", "Alice / Bob 2026-05-12", []string{"Alice", "Bob"}},
		{"strip_date_us", "Alice + Bob 5/12", []string{"Alice", "Bob"}},
		{"strip_date_us_full", "Alice + Bob 5/12/2026", []string{"Alice", "Bob"}},
		{"strip_date_us_two_digit_year", "Alice + Bob 5/12/26", []string{"Alice", "Bob"}},
		{"strip_numeric_month_day", "Alice + Bob 5-12", []string{"Alice", "Bob"}},

		// Month + weekday stripping.
		{"strip_month", "Alice & Bob May review", []string{"Alice", "Bob"}},
		{"strip_weekday", "Monday standup w/ Alice & Carol", []string{"Alice", "Carol"}},
		{"strip_weekday_abbrev", "Mon w/ Alice", []string{"Alice"}},

		// Separator splitting.
		{"split_slash", "Alice/Bob", []string{"Alice", "Bob"}},
		{"split_ampersand", "Alice & Bob", []string{"Alice", "Bob"}},
		{"split_and_word", "Alice and Bob", []string{"Alice", "Bob"}},
		{"split_with_word", "Catchup with Alice", []string{"Alice"}},
		{"split_w_slash", "Sync w/ Alice", []string{"Alice"}},
		{"split_plus", "Alice+Bob", []string{"Alice", "Bob"}},
		{"split_hyphen", "Alice-Bob 1:1", []string{"Alice", "Bob"}},
		{"split_colon", "Alice:Bob", []string{"Alice", "Bob"}},

		// Keep-regex filtering.
		{"keep_capitalized_only", "alice / bob", []string{}},
		{"keep_alpha_only", "Alice / Bob123", []string{"Alice"}},
		{"keep_length_min", "A / Bo", []string{"Bo"}},
		{"keep_length_max", "Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa / Bob", []string{"Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Bob"}}, // 30 chars passes
		{"keep_length_reject_31", "Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa / Bob", []string{"Bob"}},                            // 31 chars rejected

		// Stopwords.
		{"stopword_re", "Re Alice", []string{"Alice"}},
		{"stopword_fwd", "Fwd Alice", []string{"Alice"}},
		{"stopword_test", "Test Alice", []string{"Alice"}},
		{"stopword_demo", "Demo Alice", []string{"Alice"}},

		// Dedup.
		{"dedup_case_insensitive", "Alice & alice", []string{"Alice"}},
		{"dedup_exact", "Alice and Alice", []string{"Alice"}},

		// Empty / whitespace.
		{"empty", "", []string{}},
		{"whitespace_only", "   ", []string{}},

		// Composite.
		{"composite_meta_and_split", "1:1 catchup w/ Alice & Bob 2026-05-12", []string{"Alice", "Bob"}},

		// Unicode (out of scope per spec).
		{"unicode_diacritic_dropped", "José / Bob", []string{"Bob"}},

		// Word-boundary protection (Codex round-1 P2#1).
		{"word_boundary_monica", "Monica and Bob", []string{"Monica", "Bob"}},
		{"word_boundary_callie", "Callie sync", []string{"Callie"}},
		{"word_boundary_pranav", "Pranav sync", []string{"Pranav"}},
		{"word_boundary_may_june_dropped", "May & June 1:1", []string{}},
		{"word_boundary_april_dropped", "April review", []string{}},
		{"word_boundary_january_february_dropped", "January / February", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractNameTokens(tc.input)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractNameTokens(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestExtractNameTokens_PreservesEncounterOrder verifies the spec
// invariant that tokens appear in the order they were encountered.
func TestExtractNameTokens_PreservesEncounterOrder(t *testing.T) {
	got := ExtractNameTokens("Charlie and Alice and Bob")
	want := []string{"Charlie", "Alice", "Bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("encounter order broken: got %v, want %v", got, want)
	}
}

// TestExtractNameTokens_DedupPreservesFirstCasing verifies that
// case-insensitive dedup keeps the first-seen casing.
func TestExtractNameTokens_DedupPreservesFirstCasing(t *testing.T) {
	got := ExtractNameTokens("Alice and alice and ALICE")
	if len(got) != 1 || got[0] != "Alice" {
		t.Errorf("dedup should keep first-seen casing: got %v", got)
	}
}

// TestExtractNameTokens_Determinism — same input always yields same
// output (no map iteration leaking).
func TestExtractNameTokens_Determinism(t *testing.T) {
	input := "Alice / Bob / Carol / Dave / Erika"
	first := ExtractNameTokens(input)
	for i := 0; i < 50; i++ {
		got := ExtractNameTokens(input)
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("non-deterministic output: run 0 = %v, run %d = %v", first, i, got)
		}
	}
}

// TestExtractNameTokens_NoSideEffects — repeated calls don't pollute
// package state. (Sanity check; the function is pure.)
func TestExtractNameTokens_NoSideEffects(t *testing.T) {
	_ = ExtractNameTokens("Alice")
	_ = ExtractNameTokens("Bob")
	got := ExtractNameTokens("Carol")
	if !reflect.DeepEqual(got, []string{"Carol"}) {
		t.Errorf("side-effect leak: got %v", got)
	}
}

// Sanity: keepTokenRegex matches the documented invariants.
func TestKeepTokenRegex(t *testing.T) {
	cases := []struct {
		in   string
		keep bool
	}{
		{"Alice", true},
		{"A", false},                     // too short
		{"Ab", true},                     // length 2
		{strings.Repeat("A", 30), true},  // length 30
		{strings.Repeat("A", 31), false}, // length 31
		{"alice", false},                 // lowercase first letter
		{"Bob123", false},                // digit
		{"Bob-Smith", false},             // hyphen
	}
	for _, tc := range cases {
		got := keepTokenRegex.MatchString(tc.in)
		if got != tc.keep {
			t.Errorf("keepTokenRegex.MatchString(%q) = %v, want %v", tc.in, got, tc.keep)
		}
	}
}
