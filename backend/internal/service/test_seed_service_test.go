package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEscapeSQLLikeWildcards guards the /cleanup prefix's LIKE-wildcard escaping
// (moved here with escapeSQLLikeWildcards as part of the /cleanup layer fix). A
// caller-supplied prefix must have %, _, and \ escaped so it is matched
// literally — preventing LIKE-wildcard injection in the prefix deletes.
func TestEscapeSQLLikeWildcards(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "no wildcards", input: "simple-prefix", expected: "simple-prefix"},
		{name: "percentage wildcard", input: "test%", expected: `test\%`},
		{name: "underscore wildcard", input: "test_prefix", expected: `test\_prefix`},
		{name: "both wildcards", input: "test%_prefix", expected: `test\%\_prefix`},
		{name: "multiple percentages", input: "a%b%c", expected: `a\%b\%c`},
		{name: "backslash in input", input: `test\prefix`, expected: `test\\prefix`},
		{name: "backslash and wildcards", input: `test\%_`, expected: `test\\\%\_`},
		{name: "empty string", input: "", expected: ""},
		{name: "typical test prefix", input: "w0-1234567890", expected: "w0-1234567890"},
		{name: "malicious injection attempt", input: "prefix%; DROP TABLE contacts; --", expected: `prefix\%; DROP TABLE contacts; --`},
		{name: "wildcard at start", input: "%admin", expected: `\%admin`},
		{name: "only wildcards", input: "%%__", expected: `\%\%\_\_`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, escapeSQLLikeWildcards(tt.input))
		})
	}
}
