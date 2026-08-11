package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmailOwnDomains_NilAndMissingMetadata(t *testing.T) {
	require.Empty(t, EmailOwnDomains(nil))
	require.Empty(t, EmailOwnDomains(map[string]any{}))
	require.Empty(t, EmailOwnDomains(map[string]any{"backfill_since": "2026-01-01"}))
	require.Empty(t, EmailOwnDomains(map[string]any{MetadataKeyEmailOwnDomains: "not-a-list"}))
	require.Empty(t, EmailOwnDomains(map[string]any{MetadataKeyEmailOwnDomains: 42}))
}

func TestEmailOwnDomains_ParsesAndNormalizes(t *testing.T) {
	got := EmailOwnDomains(map[string]any{
		MetadataKeyEmailOwnDomains: []any{"Example.COM", "example.org", "user@example.net", "", "  "},
	})
	require.Equal(t, map[string]struct{}{
		"example.com": {},
		"example.org": {},
	}, got)
}

func TestNormalizeOwnDomains_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"contains at", []string{"user@example.com"}},
		{"empty entry", []string{""}},
		{"whitespace", []string{"exa mple.com"}},
		{"dotless", []string{"localdomain"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NormalizeOwnDomains(c.in)
			require.Error(t, err)
		})
	}

	got, err := NormalizeOwnDomains([]string{"Example.COM", " example.org "})
	require.NoError(t, err)
	require.Equal(t, []string{"example.com", "example.org"}, got)

	empty, err := NormalizeOwnDomains(nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestWithEmailOwnDomains_PreservesOtherKeys(t *testing.T) {
	original := map[string]any{
		"backfill_since":  "2026-01-01",
		"terminal_reason": "manual",
	}

	set := WithEmailOwnDomains(original, []string{"own-domain.example"})
	require.Equal(t, "2026-01-01", set["backfill_since"])
	require.Equal(t, "manual", set["terminal_reason"])
	require.Equal(t, []any{"own-domain.example"}, set[MetadataKeyEmailOwnDomains])

	cleared := WithEmailOwnDomains(set, nil)
	require.Equal(t, "2026-01-01", cleared["backfill_since"])
	require.Equal(t, "manual", cleared["terminal_reason"])
	_, present := cleared[MetadataKeyEmailOwnDomains]
	require.False(t, present)

	// Neither call may mutate its input map.
	require.NotContains(t, original, MetadataKeyEmailOwnDomains)
	require.Len(t, original, 2)
	require.Contains(t, set, MetadataKeyEmailOwnDomains)
}
