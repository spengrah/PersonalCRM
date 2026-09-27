package events

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRawMessageReceivedGoldenParity runs SampleRawMessageReceivedFixtureJSON
// (the Go-side dump-raw-message-fixture helper's body) and compares the
// output byte-for-byte against the committed golden fixture at
// mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json.
//
// The Swift side encodes the same struct with sortedKeys +
// withoutEscapingSlashes and compares against this golden in
// PayloadShapingTests.swift. Both sides MUST produce the same bytes.
//
// On drift:
//
//   - If you intended to change RawMessageReceivedPayload, regenerate
//     the golden:
//
//     go run ./backend/internal/events/cmd/dump-raw-message-fixture \
//     > mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json
//
//     and update the Swift PayloadShapingTests.fixturePayload() helper
//     AND the Pi-side AttachmentMeta or any other downstream consumers.
//
//   - If you did NOT intend to change the wire shape, your change has
//     broken backwards compatibility.
//
// This test catches Pi-side payload drift that path-filtered Swift CI
// would otherwise miss (.github/workflows/ci.yml excludes backend/**
// from the mac-daemon-tests job).
func TestRawMessageReceivedGoldenParity(t *testing.T) {
	got, err := SampleRawMessageReceivedFixtureJSON()
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}

	goldenPath := resolveGoldenPath(t)
	wantRaw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}

	got = trimTrailingWhitespace(got)
	want := trimTrailingWhitespace(wantRaw)

	if !bytes.Equal(got, want) {
		t.Fatalf(`raw_message_received golden parity drift.

Go-side helper produced:
%s

Committed golden (%s):
%s

If the Go-side struct changed deliberately, regenerate the golden:
  go run ./backend/internal/events/cmd/dump-raw-message-fixture \
      > mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json

and update the Swift PayloadShapingTests.fixturePayload() helper to
match.`,
			string(got), goldenPath, string(want))
	}
}

// resolveGoldenPath walks up from THIS test file's directory to the
// repo root and returns the path to the committed golden fixture.
//
// Layout:
//
//	backend/internal/events/fixture_parity_test.go
//	  -> backend/internal/events/
//	  -> backend/internal/
//	  -> backend/
//	  -> <repo-root>/
func resolveGoldenPath(t *testing.T) string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	thisDir := filepath.Dir(thisFile) // backend/internal/events
	root := filepath.Clean(filepath.Join(thisDir, "..", "..", ".."))
	return filepath.Join(root,
		"mac-daemon", "Tests", "CRMMacMessagesSourceTests",
		"Fixtures", "raw_message_received_golden.json")
}

func trimTrailingWhitespace(b []byte) []byte {
	return bytes.TrimRight(b, " \t\r\n")
}
