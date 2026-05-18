package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gowebpki/jcs"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// parityEntry mirrors the schema written by
// backend/cmd/dump-external-contact-hash-fixture/main.go.
type parityEntry struct {
	Name      string          `json:"name"`
	Note      string          `json:"note,omitempty"`
	Input     json.RawMessage `json:"input"`
	StripHost bool            `json:"strip_host"`
	Canonical string          `json:"canonical"`
	SHA256    string          `json:"sha256"`
}

type parityFile struct {
	Meta    json.RawMessage `json:"_meta"`
	Entries []parityEntry   `json:"entries"`
}

// TestExternalContactHash_ParityFixture is the cross-language parity
// anchor. The same fixture file (external_contact_hash_parity.json) is
// read by the Swift JCSParityFixtureTests; if gowebpki/jcs starts
// producing different canonical bytes (e.g. a library upgrade that
// changes float handling) this test fails AND the Swift side breaks
// independently. Either failure forces a coordinated regeneration via
// `go run ./backend/cmd/dump-external-contact-hash-fixture`.
func TestExternalContactHash_ParityFixture(t *testing.T) {
	path := filepath.Join("testdata", "external_contact_hash_parity.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read parity fixture")

	var file parityFile
	require.NoError(t, json.Unmarshal(raw, &file), "decode parity fixture")
	require.NotEmpty(t, file.Entries, "parity fixture must contain entries")

	for _, entry := range file.Entries {
		t.Run(entry.Name, func(t *testing.T) {
			input := []byte(entry.Input)
			if entry.StripHost {
				stripped, err := sjson.DeleteBytes(input, "host_id")
				require.NoError(t, err, "strip host_id")
				input = stripped
			}
			canonical, err := jcs.Transform(input)
			require.NoError(t, err, "jcs.Transform")
			require.Equal(t, entry.Canonical, string(canonical),
				"canonical drift: entry=%s — regenerate via "+
					"`go run ./backend/cmd/dump-external-contact-hash-fixture`",
				entry.Name)
			sum := sha256.Sum256(canonical)
			require.Equal(t, entry.SHA256, hex.EncodeToString(sum[:]),
				"sha256 drift: entry=%s", entry.Name)
		})
	}
}

// TestExternalContactHash_WeirdFixtureParity locks the upstream
// `gowebpki/jcs` `weird.json` test vector against our Go canonicalizer.
// The Swift JCSWeirdFixtureParityTests reads the same input + output
// file pair, so any drift between the two implementations on these
// discriminator cases (control escapes, supplementary-plane keys,
// </script> escapes, etc.) fails on both sides.
func TestExternalContactHash_WeirdFixtureParity(t *testing.T) {
	inputPath := filepath.Join("testdata", "jcs_weird_input.json")
	outputPath := filepath.Join("testdata", "jcs_weird_output.json")
	input, err := os.ReadFile(inputPath)
	require.NoError(t, err, "read weird input")
	expected, err := os.ReadFile(outputPath)
	require.NoError(t, err, "read weird output")

	canonical, err := jcs.Transform(input)
	require.NoError(t, err, "jcs.Transform weird input")
	// The upstream `weird.json` output ships without a trailing
	// newline; if a future copy operation adds one, trim before
	// comparing.
	require.Equal(t, string(expected), string(canonical),
		"weird-fixture canonical drift; re-copy from "+
			"github.com/gowebpki/jcs@v1.0.1/testdata/output/weird.json "+
			"if upstream changed, otherwise investigate.")
}

// TestExternalContactHash_ComputeContentHashAgainstParityFixture
// drives the user-facing helper (ComputeContentHash) through every
// fixture entry that has strip_host=true. This is the same test the
// Swift end-to-end ContentHasherTests will mirror — ensures the
// wrapped helper agrees with the raw JCS path used by the fixture
// generator.
func TestExternalContactHash_ComputeContentHashAgainstParityFixture(t *testing.T) {
	path := filepath.Join("testdata", "external_contact_hash_parity.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read parity fixture")

	var file parityFile
	require.NoError(t, json.Unmarshal(raw, &file), "decode parity fixture")

	for _, entry := range file.Entries {
		if !entry.StripHost {
			continue
		}
		t.Run(entry.Name, func(t *testing.T) {
			got, err := ComputeContentHash([]byte(entry.Input))
			require.NoError(t, err)
			require.Equal(t, entry.SHA256, got,
				"ComputeContentHash drift for entry=%s", entry.Name)
		})
	}
}
