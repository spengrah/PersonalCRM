package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SampleRawMessageReceivedFixtureJSON marshals a fixed sample
// RawMessageReceivedPayload via json.Marshal and returns the encoded
// bytes (with sorted keys so the output is byte-stable).
//
// The byte output is used as the golden fixture in
// mac-daemon/Tests/CRMMacMessagesSourceTests/Fixtures/raw_message_received_golden.json;
// the Swift side encodes its equivalent struct with sortedKeys +
// withoutEscapingSlashes JSONEncoder options and compares
// byte-for-byte.
//
// Any field added to RawMessageReceivedPayload requires updating BOTH
// this helper AND the Swift-side fixturePayload() in
// PayloadShapingTests.swift, then regenerating the golden file.
//
// Field values mirror the Swift side exactly. See:
//
//	mac-daemon/Tests/CRMMacMessagesSourceTests/PayloadShapingTests.swift
func SampleRawMessageReceivedFixtureJSON() ([]byte, error) {
	hostID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	sentAt := time.Date(2026, 5, 13, 15, 42, 18, 0, time.UTC)
	text := "hello"
	replyTo := "00000000-1111-2222-3333-444444444444"
	payload := RawMessageReceivedPayload{
		Version:     1,
		HostID:      hostID,
		Source:      "messages",
		Guid:        "abcdef12-3456-7890-abcd-ef1234567890",
		ChatID:      "iMessage;-;+15551234567",
		PeerHandle:  "+15551234567",
		PeerName:    nil,
		Text:        &text,
		MessageType: "text",
		IsGroup:     false,
		SentAt:      sentAt,
		ReplyToGuid: &replyTo,
		// Attachments deliberately omitted -> nil -> omitempty -> absent.
	}
	return jsonMarshalSortedKeys(payload)
}

// jsonMarshalSortedKeys marshals via json.Marshal and then re-encodes
// through a map round-trip to produce sorted keys.  Required because
// Go's encoding/json emits fields in struct-declaration order, but
// the byte-for-byte comparison with Swift's `.sortedKeys` option
// requires lexicographic ordering.
func jsonMarshalSortedKeys(v any) ([]byte, error) {
	first, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(first, &generic); err != nil {
		return nil, err
	}
	// encoding/json sorts map keys lexicographically by default for
	// map[string]interface{} which is what the Unmarshal round-trip
	// produces. Re-marshal yields stable, sorted-key output.
	return json.Marshal(generic)
}
