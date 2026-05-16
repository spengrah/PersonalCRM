package matching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parityEntry is a single row from the shared normalization parity fixture.
//
// The fixture lives at backend/internal/matching/testdata/normalization_parity.json
// and is consumed by both this Go test AND
// mac-daemon/Tests/CRMMacCoreTests/NormalizationParityTests.swift. Both sides
// must produce the same normalized output for every entry; any drift between
// the Go normalizers (used by the Pi-side identity match) and the Swift
// normalizers (used by the Mac daemon for sender filtering) silently loses
// data, so the parity fixture is the contract.
type parityEntry struct {
	Raw      string `json:"raw"`
	Type     string `json:"type"`
	Expected string `json:"expected"`
}

func TestNormalizationParityFixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "normalization_parity.json")
	data, err := os.ReadFile(fixturePath)
	require.NoError(t, err, "parity fixture must exist at %s", fixturePath)

	var entries []parityEntry
	require.NoError(t, json.Unmarshal(data, &entries))
	require.NotEmpty(t, entries, "parity fixture must contain entries")

	for _, entry := range entries {
		entry := entry
		t.Run(entry.Type+"/"+entry.Raw, func(t *testing.T) {
			var got string
			switch entry.Type {
			case "email":
				got = NormalizeEmail(entry.Raw)
			case "phone":
				got = NormalizePhoneE164(entry.Raw)
			default:
				t.Fatalf("unknown parity entry type %q", entry.Type)
			}
			assert.Equal(t, entry.Expected, got,
				"parity drift: type=%s raw=%q", entry.Type, entry.Raw)
		})
	}
}
