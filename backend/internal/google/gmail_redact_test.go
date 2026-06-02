package google

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHashIdentifier covers the THIRD-PARTY redaction helper: empty stays
// empty, lowercasing makes the tag case-insensitive, the output is a short
// sha256: prefixed tag, and the raw input is never recoverable from the tag.
// This is the testable slice of the PII fix; the own-account-stays-raw half is
// pinned by the rematch partial-failure integration test (which asserts the raw
// account id, not its hash, appears in the returned error).
func TestHashIdentifier(t *testing.T) {
	t.Run("empty stays empty", func(t *testing.T) {
		require.Equal(t, "", hashIdentifier(""))
		require.Equal(t, "", hashIdentifier("   "), "whitespace-only trims to empty")
	})

	t.Run("case-insensitive stability", func(t *testing.T) {
		require.Equal(t, hashIdentifier("foo@example.com"), hashIdentifier("Foo@Example.com"))
		require.Equal(t, hashIdentifier("foo@example.com"), hashIdentifier("  foo@example.com  "),
			"surrounding whitespace is trimmed before hashing")
	})

	t.Run("shape and non-reversibility", func(t *testing.T) {
		raw := "alice@example.com"
		got := hashIdentifier(raw)
		require.True(t, strings.HasPrefix(got, "sha256:"), "tag carries the sha256: prefix")
		hexPart := strings.TrimPrefix(got, "sha256:")
		require.Len(t, hexPart, 12, "tag uses exactly 12 hex chars")
		require.NotContains(t, got, raw, "raw third-party address must not appear in the tag")
		require.NotContains(t, got, "alice", "no fragment of the raw input leaks")
	})

	t.Run("distinct addresses get distinct tags", func(t *testing.T) {
		require.NotEqual(t, hashIdentifier("a@example.com"), hashIdentifier("b@example.com"))
	})
}
