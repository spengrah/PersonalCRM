package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmailOwnDomains_NilAndMissingMetadata(t *testing.T) {
	require.Empty(t, EmailOwnDomains.Read(nil))
	require.Empty(t, EmailOwnDomains.Read(map[string]any{}))
	require.Empty(t, EmailOwnDomains.Read(map[string]any{"backfill_since": "2026-01-01"}))
	require.Empty(t, EmailOwnDomains.Read(map[string]any{EmailOwnDomains.def().key: "not-a-list"}))
	require.Empty(t, EmailOwnDomains.Read(map[string]any{EmailOwnDomains.def().key: 42}))
}

func TestEmailOwnDomains_ParsesAndNormalizes(t *testing.T) {
	got := EmailOwnDomains.Read(map[string]any{
		EmailOwnDomains.def().key: []any{"Example.COM", "example.org", "user@example.net", "", "  "},
	})
	require.Equal(t, map[string]struct{}{
		"example.com": {},
		"example.org": {},
	}, got)
}

func TestNormalizeOwnDomains_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"contains at", "user@example.com"},
		{"empty entry", "example.com,,example.org"},
		{"whitespace", "exa mple.com"},
		{"dotless", "localdomain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := EmailOwnDomains.def().parse(c.in)
			require.Error(t, err)
		})
	}

	got, err := EmailOwnDomains.def().parse("Example.COM, example.org ")
	require.NoError(t, err)
	require.Equal(t, []string{"example.com", "example.org"}, got)
}
