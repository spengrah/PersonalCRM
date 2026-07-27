package main

import (
	"strings"
	"testing"
)

// TestMintSlug pins every parameter of the slug algorithm (D2). The keys it
// mints are permanent, so each rule gets its own row: inverting any one of them
// changes exactly one expectation here.
func TestMintSlug(t *testing.T) {
	// A 40-char base whose 38-char truncation lands ON a separator, which is
	// what makes the trailing-hyphen trim in the numeric-suffix path
	// observable: without it the result would be "...ccccccccccc--2".
	const base40 = "aaaaaaaaaaaa-bbbbbbbbbbbb-ccccccccccc-dd"
	// A 37-char base built from 5 tokens, where appending the 5th overflows
	// the cap and is therefore dropped again — so the append cannot resolve
	// the collision and the numeric fallback must.
	const base37 = "aaaaaaaaa-bbbbbbbbb-ccccccccc-ddddddd"

	cases := []struct {
		name  string
		text  string
		taken []string
		want  string
	}{
		{
			name: "stopwords dropped",
			text: "the row is removed from the overdue list",
			want: "row-removed-overdue-list",
		},
		{
			name: "negation token not is retained",
			text: "the banner is not shown when nothing is stale",
			want: "banner-not-shown-when",
		},
		{
			name: "negation token never is retained",
			text: "a revoked host is never listed",
			want: "revoked-host-never-listed",
		},
		{
			name: "negation token without is retained",
			text: "the import completes without a duplicate",
			want: "import-completes-without-duplicate",
		},
		{
			name: "all-stopword text keeps the unfiltered tokens",
			text: "the and of",
			want: "the-and-of",
		},
		{
			name: "only the first four surviving tokens are taken",
			text: "alpha beta gamma delta epsilon zeta",
			want: "alpha-beta-gamma-delta",
		},
		{
			name: "non-alphanumeric runs collapse to a single separator",
			text: "a contact's methods are displayed",
			want: "contact-s-methods-displayed",
		},
		{
			name: "cap drops a trailing token rather than truncating it",
			text: "aaaaaaaaaaaa bbbbbbbbbbbb cccccccccccc dddd",
			want: "aaaaaaaaaaaa-bbbbbbbbbbbb-cccccccccccc",
		},
		{
			name: "a single over-long token is hard-truncated to the cap",
			text: strings.Repeat("a", 45),
			want: strings.Repeat("a", 40),
		},
		{
			name:  "a collision appends the next unused token",
			text:  "alpha beta gamma delta epsilon zeta",
			taken: []string{"alpha-beta-gamma-delta"},
			want:  "alpha-beta-gamma-delta-epsilon",
		},
		{
			name:  "the cap is re-applied after a collision append, so an overflowing token cannot resolve it",
			text:  "aaaaaaaaa bbbbbbbbb ccccccccc ddddddd eeeeeeee",
			taken: []string{base37},
			want:  base37 + "-2",
		},
		{
			name:  "the numeric fallback trims a separator left by truncating the base",
			text:  "aaaaaaaaaaaa bbbbbbbbbbbb ccccccccccc dd",
			taken: []string{base40},
			want:  "aaaaaaaaaaaa-bbbbbbbbbbbb-ccccccccccc-2",
		},
		{
			name:  "the numeric fallback keeps counting past an already-taken suffix",
			text:  "alpha beta gamma delta",
			taken: []string{"alpha-beta-gamma-delta", "alpha-beta-gamma-delta-2"},
			want:  "alpha-beta-gamma-delta-3",
		},
		{
			name: "the reserved statement token is never minted",
			text: "statement",
			want: "statement-2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taken := map[string]bool{}
			for _, k := range tc.taken {
				taken[k] = true
			}
			got, err := mintSlug(tc.text, taken)
			if err != nil {
				t.Fatalf("mintSlug(%q) returned error: %v", tc.text, err)
			}
			if got != tc.want {
				t.Errorf("mintSlug(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if !slugCharset.MatchString(got) {
				t.Errorf("mintSlug(%q) = %q, which violates the key charset", tc.text, got)
			}
			if len(got) > slugMaxLen {
				t.Errorf("mintSlug(%q) = %q (%d chars), over the %d-char cap", tc.text, got, len(got), slugMaxLen)
			}
			if got == slugReserved {
				t.Errorf("mintSlug(%q) minted the reserved token %q", tc.text, slugReserved)
			}
		})
	}
}

// TestMintSlugNoAlphanumericAborts pins the abort in D2 step 2: a text with no
// alphanumeric content cannot yield a key, and the tool must say so rather than
// mint an empty one.
func TestMintSlugNoAlphanumericAborts(t *testing.T) {
	if got, err := mintSlug("--- !!! ---", map[string]bool{}); err == nil {
		t.Fatalf("mintSlug on an alphanumeric-free text returned %q, want an error", got)
	}
}

// TestSlugTokensDropsStopwordsButNotNegations guards the stopword set itself:
// every negation-bearing token must survive the filter, because a permanent key
// that loses one reads as the inverse of the item it names.
func TestSlugTokensDropsStopwordsButNotNegations(t *testing.T) {
	for _, tok := range []string{"no", "not", "never", "without", "unless", "nothing", "none"} {
		if slugStopwords[tok] {
			t.Errorf("%q is a stopword, but negation-bearing tokens must be retained", tok)
		}
		got := slugTokens("the " + tok + " case")
		if len(got) == 0 || got[0] != tok {
			t.Errorf("slugTokens(%q) = %v, want %q retained as the first token", "the "+tok+" case", got, tok)
		}
	}
	for _, tok := range []string{"a", "the", "of", "with", "that"} {
		if !slugStopwords[tok] {
			t.Errorf("%q must be a stopword", tok)
		}
	}
}
