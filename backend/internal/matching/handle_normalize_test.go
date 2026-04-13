package matching

import "testing"

func TestNormalizeHandleForNameMatch(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantTerm   string
		wantUsable bool
	}{
		{name: "baseline", input: "alicesmith", wantTerm: "alicesmith", wantUsable: true},
		{name: "lowercased", input: "AliceSmith", wantTerm: "alicesmith", wantUsable: true},
		{name: "strip at-sign", input: "@alice", wantTerm: "alice", wantUsable: true},
		{name: "dot separator", input: "@alice.smith", wantTerm: "alice smith", wantUsable: true},
		{name: "underscore separator", input: "alice_smith", wantTerm: "alice smith", wantUsable: true},
		{name: "hyphen separator", input: "alice-smith", wantTerm: "alice smith", wantUsable: true},
		{name: "trailing digits stripped", input: "alicesmith23", wantTerm: "alicesmith", wantUsable: true},
		{name: "interior digit kept", input: "alice2smith", wantTerm: "alice2smith", wantUsable: true},
		{name: "diacritic fold", input: "josé_smith", wantTerm: "jose smith", wantUsable: true},
		{name: "trim whitespace", input: "  @alice  ", wantTerm: "alice", wantUsable: true},
		{name: "at min length alex kept", input: "alex", wantTerm: "alex", wantUsable: true},
		{name: "below min length bob", input: "bob", wantTerm: "", wantUsable: false},
		{name: "empty", input: "", wantTerm: "", wantUsable: false},
		{name: "punctuation only", input: "@@@", wantTerm: "", wantUsable: false},
		{name: "collapses to 3 chars", input: "a_b", wantTerm: "", wantUsable: false},
		{name: "surrounding punctuation trimmed", input: "_alice_", wantTerm: "alice", wantUsable: true},
		{name: "all digits stripped to empty", input: "12345", wantTerm: "", wantUsable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTerm, gotUsable := NormalizeHandleForNameMatch(tc.input)
			if gotTerm != tc.wantTerm {
				t.Errorf("term: got %q, want %q", gotTerm, tc.wantTerm)
			}
			if gotUsable != tc.wantUsable {
				t.Errorf("usable: got %v, want %v", gotUsable, tc.wantUsable)
			}
		})
	}
}

func TestNormalizeForExactHandleMatch(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "basic name", input: "Alice Smith", want: "alicesmith"},
		{name: "diacritic", input: "José Smith", want: "josesmith"},
		{name: "handle with underscore", input: "@jose_smith", want: "josesmith"},
		{name: "uppercase with dot", input: "ALICE.SMITH", want: "alicesmith"},
		{name: "hyphen with digits", input: "Alice-Smith-23", want: "alicesmith23"},
		{name: "empty", input: "", want: ""},
		{name: "whitespace only", input: "  ", want: ""},
		{name: "single word diacritic", input: "José", want: "jose"},
		{name: "apostrophe", input: "O'Brien", want: "obrien"},
		{name: "hyphen in name", input: "Jean-Paul", want: "jeanpaul"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeForExactHandleMatch(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
