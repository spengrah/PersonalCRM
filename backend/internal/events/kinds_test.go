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
