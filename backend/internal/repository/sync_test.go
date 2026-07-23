package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Pure unit coverage of extractBackfillComplete: the flag is opaque to
// the backend, so absent keys and malformed values must default to
// false rather than erroring or defaulting to true.
// spec: MAC-015[2]
func TestExtractBackfillComplete(t *testing.T) {
	cases := []struct {
		name     string
		metadata []byte
		want     bool
	}{
		{name: "nil metadata", metadata: nil, want: false},
		{name: "empty metadata", metadata: []byte(``), want: false},
		{name: "absent key", metadata: []byte(`{"other_key":"x"}`), want: false},
		{name: "null value", metadata: []byte(`{"backfill_complete":null}`), want: false},
		{name: "non-bool string value", metadata: []byte(`{"backfill_complete":"true"}`), want: false},
		{name: "non-bool numeric value", metadata: []byte(`{"backfill_complete":1}`), want: false},
		{name: "malformed JSON", metadata: []byte(`{not-json`), want: false},
		{name: "true value", metadata: []byte(`{"backfill_complete":true}`), want: true},
		{name: "false value", metadata: []byte(`{"backfill_complete":false}`), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBackfillComplete(tc.metadata)
			require.Equal(t, tc.want, got)
		})
	}
}
