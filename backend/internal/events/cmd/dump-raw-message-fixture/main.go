// dump-raw-message-fixture marshals a fixed sample
// RawMessageReceivedPayload via json.Marshal and writes the result to
// stdout.
//
// Used to regenerate the golden fixture at
// mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json
// against which the Swift PayloadShaping encoder is byte-for-byte
// compared. Drift between the two sides (Go json.Marshal vs Swift
// JSONEncoder) silently breaks the ingest pipeline; this helper +
// the parity test in backend/internal/events/fixture_parity_test.go
// catches drift at PR-review time.
//
// Regenerate:
//
//	go run ./backend/internal/events/cmd/dump-raw-message-fixture \
//	    > mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json
//
// MUST stay aligned with the Swift-side fixturePayload() helper in
// PayloadShapingTests.swift.
package main

import (
	"fmt"
	"os"

	"personal-crm/backend/internal/events"
)

func main() {
	data, err := events.SampleRawMessageReceivedFixtureJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}
