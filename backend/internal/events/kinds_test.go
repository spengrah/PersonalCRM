package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMarshalUnmarshal_MessageReceived round-trips a MessageReceivedPayload
// through the envelope, asserting every field survives including the
// optional pointer fields.
func TestMarshalUnmarshal_MessageReceived(t *testing.T) {
	cid := uuid.New()
	desc := "hello"
	original := MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:123:456",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Description:       &desc,
		ExternalMessageID: "msg-abc",
	}

	raw, err := Marshal(KindMessageReceived, original)
	require.NoError(t, err)

	env := &Envelope{Kind: KindMessageReceived, Payload: raw}
	var decoded MessageReceivedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

// TestMarshalUnmarshal_MessageReceived_NilOptionals exercises the nil
// ContactID / nil Description path — these are valid for unmatched-peer
// telegram messages.
func TestMarshalUnmarshal_MessageReceived_NilOptionals(t *testing.T) {
	original := MessageReceivedPayload{
		Version:   1,
		PeerRef:   "tg:0:999",
		MessageAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}

	raw, err := Marshal(KindMessageReceived, original)
	require.NoError(t, err)

	env := &Envelope{Kind: KindMessageReceived, Payload: raw}
	var decoded MessageReceivedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
	require.Nil(t, decoded.ContactID)
	require.Nil(t, decoded.Description)
}

func TestMarshalUnmarshal_MessageSent(t *testing.T) {
	cid := uuid.New()
	desc := "hi"
	original := MessageSentPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:123:456",
		MessageAt:         time.Date(2026, 4, 10, 13, 0, 0, 0, time.UTC),
		Description:       &desc,
		ExternalMessageID: "msg-xyz",
	}
	raw, err := Marshal(KindMessageSent, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMessageSent, Payload: raw}
	var decoded MessageSentPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_CalendarAttended(t *testing.T) {
	original := CalendarAttendedPayload{
		Version:    1,
		ContactID:  uuid.New(),
		EventID:    "gcal-evt-1",
		OccurredAt: time.Date(2026, 4, 10, 14, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindCalendarAttended, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindCalendarAttended, Payload: raw}
	var decoded CalendarAttendedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_CalendarDeclined(t *testing.T) {
	original := CalendarDeclinedPayload{
		Version:    1,
		ContactID:  uuid.New(),
		EventID:    "gcal-evt-2",
		OccurredAt: time.Date(2026, 4, 10, 15, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindCalendarDeclined, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindCalendarDeclined, Payload: raw}
	var decoded CalendarDeclinedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_TaskCompleted(t *testing.T) {
	original := TaskCompletedPayload{
		Version:     1,
		ContactID:   uuid.New(),
		TaskID:      "6fw9cQQ5JppCp7qX",
		TaskKind:    "cadence",
		CompletedAt: time.Date(2026, 4, 10, 16, 0, 0, 0, time.UTC),
		Direction:   "mutual",
	}
	raw, err := Marshal(KindTaskCompleted, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindTaskCompleted, Payload: raw}
	var decoded TaskCompletedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_TaskSkipped(t *testing.T) {
	original := TaskSkippedPayload{
		Version:   1,
		ContactID: uuid.New(),
		TaskID:    "task-xyz",
		SkippedAt: time.Date(2026, 4, 10, 17, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindTaskSkipped, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindTaskSkipped, Payload: raw}
	var decoded TaskSkippedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_TaskOutreachDetected(t *testing.T) {
	original := TaskOutreachDetectedPayload{
		Version:    1,
		ContactID:  uuid.New(),
		TaskID:     "task-abc",
		DetectedAt: time.Date(2026, 4, 10, 18, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindTaskOutreachDetected, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindTaskOutreachDetected, Payload: raw}
	var decoded TaskOutreachDetectedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_InteractionManual(t *testing.T) {
	original := InteractionManualPayload{
		Version:     1,
		ContactID:   uuid.New(),
		Direction:   "outbound",
		OccurredAt:  time.Date(2026, 4, 10, 19, 0, 0, 0, time.UTC),
		Description: "called friend",
	}
	raw, err := Marshal(KindInteractionManual, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindInteractionManual, Payload: raw}
	var decoded InteractionManualPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_ContactMethodsAdded(t *testing.T) {
	original := ContactMethodsAddedPayload{
		Version:   1,
		ContactID: uuid.New(),
		Methods: []ContactMethodRef{
			{Type: "email", Value: "alice@example.com"},
			{Type: "phone", Value: "+15551234567"},
		},
		RematchJobID: uuid.New(),
	}
	raw, err := Marshal(KindContactMethodsAdded, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindContactMethodsAdded, Payload: raw}
	var decoded ContactMethodsAddedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

func TestMarshalUnmarshal_InteractionRecorded(t *testing.T) {
	ref := "tg:123:456"
	original := InteractionRecordedPayload{
		Version:       1,
		ContactID:     uuid.New(),
		InteractionID: uuid.New(),
		Direction:     "mutual",
		OccurredAt:    time.Date(2026, 4, 10, 20, 0, 0, 0, time.UTC),
		Source:        "telegram",
		SourceRef:     &ref,
	}
	raw, err := Marshal(KindInteractionRecorded, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindInteractionRecorded, Payload: raw}
	var decoded InteractionRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

// TestMarshal_KindPayloadMismatch_ReturnsError is the primary guardrail:
// MessageReceivedPayload and MessageSentPayload are structurally identical,
// so JSON decode alone can't tell them apart. The registry check catches
// publisher bugs.
func TestMarshal_KindPayloadMismatch_ReturnsError(t *testing.T) {
	_, err := Marshal(KindMessageReceived, MessageSentPayload{Version: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects payload")
}

func TestMarshal_UnknownKind_ReturnsError(t *testing.T) {
	_, err := Marshal(Kind("made.up"), MessageReceivedPayload{Version: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown kind")
}

func TestUnmarshal_KindPayloadMismatch_ReturnsError(t *testing.T) {
	raw, err := Marshal(KindMessageReceived, MessageReceivedPayload{Version: 1})
	require.NoError(t, err)
	env := &Envelope{Kind: KindMessageReceived, Payload: raw}
	var wrong MessageSentPayload
	err = Unmarshal(env, &wrong)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decodes to")
}

func TestUnmarshal_NilEnvelope_ReturnsError(t *testing.T) {
	var dst MessageReceivedPayload
	err := Unmarshal(nil, &dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

func TestUnmarshal_EmptyPayload_ReturnsError(t *testing.T) {
	env := &Envelope{Kind: KindMessageReceived}
	var dst MessageReceivedPayload
	err := Unmarshal(env, &dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty payload")
}

func TestUnmarshal_UnknownKind_ReturnsError(t *testing.T) {
	env := &Envelope{Kind: Kind("made.up"), Payload: json.RawMessage(`{}`)}
	var dst MessageReceivedPayload
	err := Unmarshal(env, &dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown kind")
}

// TestUnmarshal_ForwardCompatible_IgnoresUnknownFields documents that Go's
// default JSON decoder silently drops unknown fields. This is the intended
// behavior — publishers can add fields to a v1 struct without breaking old
// consumers, as long as the additions remain semantic no-ops for them.
func TestUnmarshal_ForwardCompatible_IgnoresUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{
		"version": 1,
		"contact_id": "00000000-0000-0000-0000-000000000001",
		"event_id": "evt-future",
		"occurred_at": "2026-04-10T14:00:00Z",
		"future_field": "this should be silently dropped"
	}`)
	env := &Envelope{Kind: KindCalendarAttended, Payload: raw}
	var decoded CalendarAttendedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, 1, decoded.Version)
	require.Equal(t, "evt-future", decoded.EventID)
}

// TestKindPayloadTypes_CoversAllKinds enforces that every declared Kind has
// a registered payload type. Prevents "I added a new Kind but forgot to
// update the registry" from silently passing.
func TestKindPayloadTypes_CoversAllKinds(t *testing.T) {
	require.Len(t, kindPayloadTypes, len(AllKinds))
	for _, k := range AllKinds {
		_, ok := kindPayloadTypes[k]
		require.True(t, ok, "kind %s missing from kindPayloadTypes", k)
	}
}

// TestAllKinds_ExpectedCount guards against accidental Kind additions or
// deletions from AllKinds without updating the spec. Current spec (§3.2)
// declares exactly 10 kinds (9 raw-signal + 1 derived).
func TestAllKinds_ExpectedCount(t *testing.T) {
	require.Len(t, AllKinds, 10)
}

// TestIsKnownKind_CoversAllKinds is the positive side: every Kind declared
// in AllKinds must be reported as known. Guards against registry drift.
func TestIsKnownKind_CoversAllKinds(t *testing.T) {
	for _, k := range AllKinds {
		require.True(t, IsKnownKind(k), "kind %s should be known", k)
	}
}

func TestIsKnownKind_UnknownReturnsFalse(t *testing.T) {
	require.False(t, IsKnownKind(Kind("made.up.kind")))
	require.False(t, IsKnownKind(Kind("")))
}

// buildCanonicalPayload returns a round-trippable JSON payload for a given
// Kind. Only test code needs this — it's the fixture for the
// ValidatePayload round-trip test.
func buildCanonicalPayload(t *testing.T, kind Kind) json.RawMessage {
	t.Helper()
	cid := uuid.New()
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	switch kind {
	case KindMessageReceived:
		raw, err := Marshal(kind, MessageReceivedPayload{Version: 1, PeerRef: "tg:1:2", MessageAt: at})
		require.NoError(t, err)
		return raw
	case KindMessageSent:
		raw, err := Marshal(kind, MessageSentPayload{Version: 1, PeerRef: "tg:1:2", MessageAt: at})
		require.NoError(t, err)
		return raw
	case KindCalendarAttended:
		raw, err := Marshal(kind, CalendarAttendedPayload{Version: 1, ContactID: cid, EventID: "gcal-1", OccurredAt: at})
		require.NoError(t, err)
		return raw
	case KindCalendarDeclined:
		raw, err := Marshal(kind, CalendarDeclinedPayload{Version: 1, ContactID: cid, EventID: "gcal-1", OccurredAt: at})
		require.NoError(t, err)
		return raw
	case KindTaskCompleted:
		raw, err := Marshal(kind, TaskCompletedPayload{Version: 1, ContactID: cid, TaskID: "t1", TaskKind: "cadence", CompletedAt: at, Direction: "mutual"})
		require.NoError(t, err)
		return raw
	case KindTaskSkipped:
		raw, err := Marshal(kind, TaskSkippedPayload{Version: 1, ContactID: cid, TaskID: "t1", SkippedAt: at})
		require.NoError(t, err)
		return raw
	case KindTaskOutreachDetected:
		raw, err := Marshal(kind, TaskOutreachDetectedPayload{Version: 1, ContactID: cid, TaskID: "t1", DetectedAt: at})
		require.NoError(t, err)
		return raw
	case KindInteractionManual:
		raw, err := Marshal(kind, InteractionManualPayload{Version: 1, ContactID: cid, Direction: "mutual", OccurredAt: at})
		require.NoError(t, err)
		return raw
	case KindContactMethodsAdded:
		raw, err := Marshal(kind, ContactMethodsAddedPayload{Version: 1, ContactID: cid, Methods: []ContactMethodRef{{Type: "email", Value: "a@b.com"}}, RematchJobID: uuid.New()})
		require.NoError(t, err)
		return raw
	case KindInteractionRecorded:
		raw, err := Marshal(kind, InteractionRecordedPayload{Version: 1, ContactID: cid, InteractionID: uuid.New(), Direction: "mutual", OccurredAt: at, Source: "manual"})
		require.NoError(t, err)
		return raw
	}
	t.Fatalf("unhandled kind %s", kind)
	return nil
}

// TestValidatePayload_AllKindsRoundTrip exercises the validate-only helper
// against every declared Kind. Round-tripping a canonical payload must
// succeed (no structural mismatch).
func TestValidatePayload_AllKindsRoundTrip(t *testing.T) {
	for _, k := range AllKinds {
		t.Run(string(k), func(t *testing.T) {
			env := &Envelope{Kind: k, Payload: buildCanonicalPayload(t, k)}
			require.NoError(t, ValidatePayload(env))
		})
	}
}

func TestValidatePayload_MalformedJSON(t *testing.T) {
	env := &Envelope{Kind: KindInteractionManual, Payload: json.RawMessage("{not json")}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validate interaction.manual")
}

func TestValidatePayload_EmptyPayload(t *testing.T) {
	env := &Envelope{Kind: KindInteractionManual}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty payload")
}

func TestValidatePayload_UnknownKind(t *testing.T) {
	env := &Envelope{Kind: Kind("made.up.kind"), Payload: json.RawMessage(`{}`)}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown kind")
}

func TestValidatePayload_NilEnvelope(t *testing.T) {
	err := ValidatePayload(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

// TestValidatePayload_StructuralTypeMismatch covers the case where the
// payload JSON decodes but a field's type is wrong — e.g. Version is a
// string instead of an int. json.UnmarshalTypeError surfaces through
// ValidatePayload as a wrapped error. This is the primary "payload doesn't
// match kind's type" guardrail.
func TestValidatePayload_StructuralTypeMismatch(t *testing.T) {
	// KindCalendarAttended's ContactID is uuid.UUID — if we pass an int the
	// JSON decoder rejects it with UnmarshalTypeError.
	bad := json.RawMessage(`{"version": "not-a-number", "contact_id": "00000000-0000-0000-0000-000000000000", "event_id": "e", "occurred_at": "2026-04-10T12:00:00Z"}`)
	env := &Envelope{Kind: KindCalendarAttended, Payload: bad}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validate calendar.attended")
}

// TestValidatePayload_NonObjectPayload rejects a top-level primitive JSON
// value (e.g. "just a string") that can't unmarshal into any struct.
func TestValidatePayload_NonObjectPayload(t *testing.T) {
	env := &Envelope{Kind: KindInteractionManual, Payload: json.RawMessage(`"not an object"`)}
	err := ValidatePayload(env)
	require.Error(t, err)
}
