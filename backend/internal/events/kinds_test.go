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

// TestMarshalUnmarshal_MessageReceived_WithDirectionOverride verifies the
// optional Direction field round-trips. Fresh-mutual telegram sessions rely
// on this field to carry "mutual" through the shadow-mode bake (plan
// Decision 6).
func TestMarshalUnmarshal_MessageReceived_WithDirectionOverride(t *testing.T) {
	cid := uuid.New()
	original := MessageReceivedPayload{
		Version:   1,
		ContactID: &cid,
		PeerRef:   "tg:123:456",
		MessageAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Direction: "mutual",
	}
	raw, err := Marshal(KindMessageReceived, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMessageReceived, Payload: raw}
	var decoded MessageReceivedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, "mutual", decoded.Direction)
	require.Equal(t, original, decoded)
}

// TestMarshalUnmarshal_MessageReceived_EmptyDirectionOmitted verifies that
// an absent Direction field decodes to "" (the kind-default sentinel the
// consumer uses to fall back to "inbound"/"outbound").
func TestMarshalUnmarshal_MessageReceived_EmptyDirectionOmitted(t *testing.T) {
	original := MessageReceivedPayload{
		Version:   1,
		PeerRef:   "tg:1:2",
		MessageAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindMessageReceived, original)
	require.NoError(t, err)
	// `omitempty` means Direction is absent from the JSON when empty.
	require.NotContains(t, string(raw), "direction")
	env := &Envelope{Kind: KindMessageReceived, Payload: raw}
	var decoded MessageReceivedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, "", decoded.Direction)
}

func TestMarshalUnmarshal_MessageSent_WithDirectionOverride(t *testing.T) {
	cid := uuid.New()
	original := MessageSentPayload{
		Version:   1,
		ContactID: &cid,
		PeerRef:   "tg:123:456",
		MessageAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Direction: "mutual",
	}
	raw, err := Marshal(KindMessageSent, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMessageSent, Payload: raw}
	var decoded MessageSentPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, "mutual", decoded.Direction)
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

// TestMarshalUnmarshal_MessagePayloads_WithMessageIDs asserts the
// cutover-era MessageIDs field (plan Decision 10) round-trips cleanly on
// both MessageReceivedPayload and MessageSentPayload.
func TestMarshalUnmarshal_MessagePayloads_WithMessageIDs(t *testing.T) {
	msgID1 := uuid.New()
	msgID2 := uuid.New()
	cid := uuid.New()

	t.Run("received_with_message_ids", func(t *testing.T) {
		original := MessageReceivedPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           "tg:1:2",
			MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			ExternalMessageID: "tg:1:42",
			MessageIDs:        []uuid.UUID{msgID1, msgID2},
		}
		raw, err := Marshal(KindMessageReceived, original)
		require.NoError(t, err)
		require.Contains(t, string(raw), "message_ids")

		env := &Envelope{Kind: KindMessageReceived, Payload: raw}
		var decoded MessageReceivedPayload
		require.NoError(t, Unmarshal(env, &decoded))
		require.Equal(t, original, decoded)
		require.Len(t, decoded.MessageIDs, 2)
	})

	t.Run("received_empty_message_ids_omitted", func(t *testing.T) {
		original := MessageReceivedPayload{
			Version:           1,
			PeerRef:           "tg:1:2",
			MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			ExternalMessageID: "tg:1:42",
		}
		raw, err := Marshal(KindMessageReceived, original)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "message_ids",
			"omitempty should elide empty slice from JSON")
	})

	t.Run("sent_with_message_ids", func(t *testing.T) {
		original := MessageSentPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           "tg:1:2",
			MessageAt:         time.Date(2026, 4, 10, 13, 0, 0, 0, time.UTC),
			ExternalMessageID: "tg:1:43",
			MessageIDs:        []uuid.UUID{msgID1},
		}
		raw, err := Marshal(KindMessageSent, original)
		require.NoError(t, err)
		env := &Envelope{Kind: KindMessageSent, Payload: raw}
		var decoded MessageSentPayload
		require.NoError(t, Unmarshal(env, &decoded))
		require.Equal(t, original, decoded)
	})
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

// TestMarshalUnmarshal_EmailEventPayload round-trips an EmailEventPayload
// under both email kinds, asserting every field survives including the
// optional Subject pointer. One struct serves both kinds; the kind is the
// only discriminator.
func TestMarshalUnmarshal_EmailEventPayload(t *testing.T) {
	subject := "Lunch next week?"
	cid := uuid.New()

	t.Run("received", func(t *testing.T) {
		original := EmailEventPayload{
			Version:    1,
			ContactID:  cid,
			ExternalID: "<msg-abc@example.com>",
			ThreadID:   "thread-xyz",
			LocalDay:   "2026-04-10",
			SentAt:     time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			Direction:  "inbound",
			Subject:    &subject,
		}
		raw, err := Marshal(KindEmailReceived, original)
		require.NoError(t, err)
		env := &Envelope{Kind: KindEmailReceived, Payload: raw}
		var decoded EmailEventPayload
		require.NoError(t, Unmarshal(env, &decoded))
		require.Equal(t, original, decoded)
	})

	t.Run("sent_nil_subject", func(t *testing.T) {
		original := EmailEventPayload{
			Version:    1,
			ContactID:  cid,
			ExternalID: "nomsgid:user@example.com:gmail-1",
			ThreadID:   "thread-2",
			LocalDay:   "2026-04-11",
			SentAt:     time.Date(2026, 4, 11, 9, 30, 0, 0, time.UTC),
			Direction:  "outbound",
		}
		raw, err := Marshal(KindEmailSent, original)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "subject",
			"omitempty should elide a nil Subject from JSON")
		env := &Envelope{Kind: KindEmailSent, Payload: raw}
		var decoded EmailEventPayload
		require.NoError(t, Unmarshal(env, &decoded))
		require.Equal(t, original, decoded)
		require.Nil(t, decoded.Subject)
	})

	t.Run("kind_mismatch_on_unmarshal", func(t *testing.T) {
		raw, err := Marshal(KindEmailReceived, EmailEventPayload{Version: 1, ContactID: cid})
		require.NoError(t, err)
		// Decoding an email.received envelope into the calendar payload type
		// must be rejected by the registry's kind-vs-type guard.
		env := &Envelope{Kind: KindEmailReceived, Payload: raw}
		var wrong CalendarAttendedPayload
		err = Unmarshal(env, &wrong)
		require.Error(t, err)
		require.Contains(t, err.Error(), "decodes to")
	})
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

// TestMarshalUnmarshal_InteractionRecorded_V2 asserts the V2 payload shape
// added in PR 7 (plan Decision 2a): PrevCadenceSnapshot and
// PrevCadenceValue round-trip through JSON and reach the consumer intact.
func TestMarshalUnmarshal_InteractionRecorded_V2(t *testing.T) {
	ref := "tg:123:456"
	cadenceStr := "weekly"
	lastContacted := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	lastOutreach := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	lastResponse := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	contactBy := time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC)
	original := InteractionRecordedPayload{
		Version:       2,
		ContactID:     uuid.New(),
		InteractionID: uuid.New(),
		Direction:     "mutual",
		OccurredAt:    time.Date(2026, 4, 10, 20, 0, 0, 0, time.UTC),
		Source:        "telegram",
		SourceRef:     &ref,
		PrevCadenceSnapshot: &CadenceFieldsSnapshot{
			LastContacted:  &lastContacted,
			LastOutreachAt: &lastOutreach,
			LastResponseAt: &lastResponse,
			ContactBy:      &contactBy,
		},
		PrevCadenceValue: &cadenceStr,
	}
	raw, err := Marshal(KindInteractionRecorded, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindInteractionRecorded, Payload: raw}
	var decoded InteractionRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, 2, decoded.Version)
	require.NotNil(t, decoded.PrevCadenceSnapshot)
	require.Equal(t, original.PrevCadenceSnapshot.LastContacted.Unix(), decoded.PrevCadenceSnapshot.LastContacted.Unix())
	require.Equal(t, original.PrevCadenceSnapshot.LastOutreachAt.Unix(), decoded.PrevCadenceSnapshot.LastOutreachAt.Unix())
	require.Equal(t, original.PrevCadenceSnapshot.LastResponseAt.Unix(), decoded.PrevCadenceSnapshot.LastResponseAt.Unix())
	require.NotNil(t, decoded.PrevCadenceSnapshot.ContactBy)
	require.NotNil(t, decoded.PrevCadenceValue)
	require.Equal(t, "weekly", *decoded.PrevCadenceValue)
}

// TestMarshalUnmarshal_InteractionRecorded_V3 asserts the V3 payload
// shape carries SuppressFollowUp through the wire format. Polarity is
// zero=do-not-suppress so a missing field decodes safely as false;
// explicit true is set only by the Todoist provider for kind=send
// completions.
func TestMarshalUnmarshal_InteractionRecorded_V3(t *testing.T) {
	original := InteractionRecordedPayload{
		Version:          3,
		ContactID:        uuid.New(),
		InteractionID:    uuid.New(),
		Direction:        "outbound",
		OccurredAt:       time.Date(2026, 4, 10, 20, 0, 0, 0, time.UTC),
		Source:           "todoist",
		SuppressFollowUp: true,
	}
	raw, err := Marshal(KindInteractionRecorded, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindInteractionRecorded, Payload: raw}
	var decoded InteractionRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, 3, decoded.Version)
	require.True(t, decoded.SuppressFollowUp,
		"V3 SuppressFollowUp must round-trip as true")
}

// TestInteractionRecordedPayload_V2Decode_DefaultsSuppressFollowUpFalse
// is the load-bearing polarity test (plan §6.3): a V2 envelope (no
// suppress_follow_up field) must decode with SuppressFollowUp=false so
// existing in-flight events continue to spawn follow-ups.
func TestInteractionRecordedPayload_V2Decode_DefaultsSuppressFollowUpFalse(t *testing.T) {
	v2Payload := `{
		"version": 2,
		"contact_id": "00000000-0000-0000-0000-000000000001",
		"interaction_id": "00000000-0000-0000-0000-000000000002",
		"direction": "mutual",
		"occurred_at": "2026-04-10T20:00:00Z",
		"source": "telegram"
	}`
	env := &Envelope{Kind: KindInteractionRecorded, Payload: json.RawMessage(v2Payload)}
	var decoded InteractionRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.False(t, decoded.SuppressFollowUp,
		"V2 payload missing suppress_follow_up MUST decode false (zero=do-not-suppress)")
}

// TestTaskCompletedPayload_V1Decode_DefaultsSuppressFollowUpFalse is
// the V1 task.completed analogue: a legacy envelope (no
// suppress_follow_up field) decodes with SuppressFollowUp=false.
// Companion to the V2 InteractionRecorded test above; together they
// establish that the polarity-safe default holds across both upstream
// and downstream payload migrations.
func TestTaskCompletedPayload_V1Decode_DefaultsSuppressFollowUpFalse(t *testing.T) {
	v1Payload := `{
		"version": 1,
		"contact_id": "00000000-0000-0000-0000-000000000001",
		"task_id": "task-abc",
		"task_kind": "cadence",
		"completed_at": "2026-04-10T16:00:00Z",
		"direction": "mutual"
	}`
	env := &Envelope{Kind: KindTaskCompleted, Payload: json.RawMessage(v1Payload)}
	var decoded TaskCompletedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.False(t, decoded.SuppressFollowUp,
		"V1 task.completed missing suppress_follow_up MUST decode false")
}

// TestMarshalUnmarshal_InteractionRecorded_V1BackCompat asserts that a
// PR 5/6-era Version=1 payload (no PrevCadenceSnapshot / PrevCadenceValue)
// still unmarshals cleanly — the new fields are nil. The PR 7 consumer
// logs ERROR and skips when it sees V1, but the unmarshal itself must
// succeed so the ERROR path can fire with the envelope context.
func TestMarshalUnmarshal_InteractionRecorded_V1BackCompat(t *testing.T) {
	v1Payload := `{
		"version": 1,
		"contact_id": "00000000-0000-0000-0000-000000000001",
		"interaction_id": "00000000-0000-0000-0000-000000000002",
		"direction": "mutual",
		"occurred_at": "2026-04-10T20:00:00Z",
		"source": "telegram"
	}`
	env := &Envelope{Kind: KindInteractionRecorded, Payload: json.RawMessage(v1Payload)}
	var decoded InteractionRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, 1, decoded.Version)
	require.Nil(t, decoded.PrevCadenceSnapshot)
	require.Nil(t, decoded.PrevCadenceValue)
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
// declares 10 base kinds (9 raw-signal + 1 derived), plus the two
// daemon-emitted raw_message.* kinds (messages source), plus the two
// external_contact.* kinds (iCloud Contacts source), plus the two
// meeting_note.* kinds (anarlog_sessions source), plus the two call.*
// kinds (phone_calls source, v1.5), plus the two email.* kinds (Gmail
// source, published by GmailSyncProvider), plus the five assertion.*
// lifecycle kinds (graph foundation, published by AssertService).
func TestAllKinds_ExpectedCount(t *testing.T) {
	require.Len(t, AllKinds, 25)
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
	case KindEmailReceived, KindEmailSent:
		direction := "inbound"
		if kind == KindEmailSent {
			direction = "outbound"
		}
		raw, err := Marshal(kind, EmailEventPayload{Version: 1, ContactID: cid, ExternalID: "<msg-1@example.com>", ThreadID: "thread-1", LocalDay: "2026-04-10", SentAt: at, Direction: direction})
		require.NoError(t, err)
		return raw
	case KindInteractionRecorded:
		raw, err := Marshal(kind, InteractionRecordedPayload{Version: 1, ContactID: cid, InteractionID: uuid.New(), Direction: "mutual", OccurredAt: at, Source: "manual"})
		require.NoError(t, err)
		return raw
	case KindRawMessageReceived:
		raw, err := Marshal(kind, RawMessageReceivedPayload{
			Version: 1, HostID: uuid.New(), Source: "messages",
			Guid: "g-1", ChatID: "iMessage;-;chat-x", PeerHandle: "+15551234567",
			MessageType: "text", SentAt: at,
		})
		require.NoError(t, err)
		return raw
	case KindRawMessageSent:
		raw, err := Marshal(kind, RawMessageSentPayload{
			Version: 1, HostID: uuid.New(), Source: "messages",
			Guid: "g-2", ChatID: "iMessage;-;chat-x", PeerHandle: "+15551234567",
			MessageType: "text", SentAt: at,
		})
		require.NoError(t, err)
		return raw
	case KindExternalContactUpserted:
		raw, err := Marshal(kind, ExternalContactUpsertedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts",
			EntityID: "CN-canonical-1",
		})
		require.NoError(t, err)
		return raw
	case KindExternalContactDeleted:
		raw, err := Marshal(kind, ExternalContactDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts",
			EntityID: "CN-canonical-2",
		})
		require.NoError(t, err)
		return raw
	case KindMeetingNoteRecorded:
		raw, err := Marshal(kind, MeetingNoteRecordedPayload{
			Version: 1, HostID: uuid.New(), Source: "anarlog_sessions",
			SourceID: uuid.NewString(), MeetingAt: at,
		})
		require.NoError(t, err)
		return raw
	case KindMeetingNoteDeleted:
		raw, err := Marshal(kind, MeetingNoteDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "anarlog_sessions",
			SourceID: uuid.NewString(),
		})
		require.NoError(t, err)
		return raw
	case KindCallReceived:
		answered := true
		raw, err := Marshal(kind, CallPayload{
			Version: 1, HostID: uuid.New(), Source: "phone_calls",
			CallUniqueID: "C-canonical-1", PeerHandle: "+15551234567",
			PeerNormalized: "+15551234567", Service: "voice", Direction: "inbound",
			Answered: &answered, HasVoicemail: false, DurationSeconds: 42,
			StartedAt: at,
		})
		require.NoError(t, err)
		return raw
	case KindCallSent:
		raw, err := Marshal(kind, CallPayload{
			Version: 1, HostID: uuid.New(), Source: "phone_calls",
			CallUniqueID: "C-canonical-2", PeerHandle: "+15551234567",
			PeerNormalized: "+15551234567", Service: "facetime_audio", Direction: "outbound",
			Answered: nil, HasVoicemail: false, DurationSeconds: 120,
			StartedAt: at,
		})
		require.NoError(t, err)
		return raw
	case KindAssertionProposed, KindAssertionAccepted, KindAssertionSuperseded,
		KindAssertionRejected, KindAssertionProvenanceAdded:
		raw, err := Marshal(kind, AssertionEventPayload{
			Version: 1, AssertionID: uuid.New(), SubjectNodeID: uuid.New(),
			PredicateKey: "lives_in",
		})
		require.NoError(t, err)
		return raw
	}
	t.Fatalf("unhandled kind %s", kind)
	return nil
}

// TestMarshalUnmarshal_RawMessageReceived round-trips the daemon-emitted
// raw_message.received payload including the omitempty optional fields.
func TestMarshalUnmarshal_RawMessageReceived(t *testing.T) {
	host := uuid.New()
	name := "Alice"
	text := "hello"
	reply := "g-reply"
	size := int64(123)
	filename := "kitten.jpg"
	mime := "image/jpeg"
	original := RawMessageReceivedPayload{
		Version: 1, HostID: host, Source: "messages",
		Guid: "g-1", ChatID: "iMessage;-;chat-xyz", PeerHandle: "+15551234567",
		PeerName: &name, Text: &text, MessageType: "photo", IsGroup: false,
		SentAt:      time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ReplyToGuid: &reply,
		Attachments: []AttachmentMeta{
			{Type: "photo", Filename: &filename, MimeType: &mime, Size: &size},
		},
	}
	raw, err := Marshal(KindRawMessageReceived, original)
	require.NoError(t, err)

	env := &Envelope{Kind: KindRawMessageReceived, Payload: raw}
	var decoded RawMessageReceivedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

// TestMarshalUnmarshal_RawMessageSent round-trips the outbound twin.
func TestMarshalUnmarshal_RawMessageSent(t *testing.T) {
	host := uuid.New()
	original := RawMessageSentPayload{
		Version: 1, HostID: host, Source: "messages",
		Guid: "g-2", ChatID: "iMessage;-;chat-xyz", PeerHandle: "+15551234567",
		MessageType: "text",
		SentAt:      time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	raw, err := Marshal(KindRawMessageSent, original)
	require.NoError(t, err)

	env := &Envelope{Kind: KindRawMessageSent, Payload: raw}
	var decoded RawMessageSentPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original, decoded)
}

// TestValidatePayload_RawMessage_RejectsNonCanonicalMessageType asserts
// the per-kind value check rejects a MessageType outside the canonical
// CHECK-constraint set.
func TestValidatePayload_RawMessage_RejectsNonCanonicalMessageType(t *testing.T) {
	raw := json.RawMessage(`{
		"version": 1,
		"host_id": "00000000-0000-0000-0000-000000000001",
		"source": "messages",
		"guid": "g-1",
		"chat_id": "iMessage;-;chat-xyz",
		"peer_handle": "+15551234567",
		"message_type": "sticker",
		"sent_at": "2026-04-10T12:00:00Z"
	}`)
	env := &Envelope{Kind: KindRawMessageReceived, Payload: raw}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "message_type")
}

// TestValidatePayload_RawMessage_AcceptsAllCanonicalMessageTypes covers
// every value in the canonical set passes ValidatePayload.
func TestValidatePayload_RawMessage_AcceptsAllCanonicalMessageTypes(t *testing.T) {
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	for _, mt := range []string{"text", "photo", "audio", "video", "document", "other"} {
		t.Run(mt, func(t *testing.T) {
			raw, err := Marshal(KindRawMessageReceived, RawMessageReceivedPayload{
				Version: 1, HostID: uuid.New(), Source: "messages",
				Guid: "g-" + mt, ChatID: "iMessage;-;chat-x", PeerHandle: "+1",
				MessageType: mt, SentAt: at,
			})
			require.NoError(t, err)
			env := &Envelope{Kind: KindRawMessageReceived, Payload: raw}
			require.NoError(t, ValidatePayload(env))
		})
	}
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

// TestValidatePayload_NullPayload rejects a literal JSON `null`. stdlib
// json.Unmarshal happily decodes `null` into a struct pointer (leaving
// the struct zero-valued), which would silently admit garbage events
// into the event log. The ingest boundary is the right place to reject.
func TestValidatePayload_NullPayload(t *testing.T) {
	env := &Envelope{Kind: KindInteractionManual, Payload: json.RawMessage(`null`)}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "got null")
}

// TestValidatePayload_NullPayloadWithWhitespace documents that whitespace
// around the literal `null` still triggers the rejection.
func TestValidatePayload_NullPayloadWithWhitespace(t *testing.T) {
	env := &Envelope{Kind: KindInteractionManual, Payload: json.RawMessage("  null\n")}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "got null")
}

// ----------------------------------------------------------------------------
// external_contact.* payload tests.
// ----------------------------------------------------------------------------

func makeExternalContactUpsertedRaw(t *testing.T, mutate func(*ExternalContactUpsertedPayload)) json.RawMessage {
	t.Helper()
	bday := "1990-04-21"
	dn := "Sample User"
	p := ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      uuid.New(),
		Source:      "icloud_contacts",
		EntityID:    "CN-entity-1",
		DisplayName: &dn,
		Emails:      []ExternalContactMethodValue{{Value: "u@example.com", Type: "home", Primary: true}},
		Phones:      []ExternalContactMethodValue{{Value: "+15551234567", Type: "mobile"}},
		Birthday:    &bday,
	}
	if mutate != nil {
		mutate(&p)
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return json.RawMessage(b)
}

func TestValidatePayload_ExternalContactUpserted_AcceptsValid(t *testing.T) {
	env := &Envelope{
		Kind:    KindExternalContactUpserted,
		Payload: makeExternalContactUpsertedRaw(t, nil),
	}
	require.NoError(t, ValidatePayload(env))
}

func TestValidatePayload_ExternalContactUpserted_RejectsEmptyEntityID(t *testing.T) {
	env := &Envelope{
		Kind: KindExternalContactUpserted,
		Payload: makeExternalContactUpsertedRaw(t, func(p *ExternalContactUpsertedPayload) {
			p.EntityID = ""
		}),
	}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "entity_id is required")
}

func TestValidatePayload_ExternalContactUpserted_RejectsZeroHostID(t *testing.T) {
	env := &Envelope{
		Kind: KindExternalContactUpserted,
		Payload: makeExternalContactUpsertedRaw(t, func(p *ExternalContactUpsertedPayload) {
			p.HostID = uuid.Nil
		}),
	}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host_id is required")
}

func TestValidatePayload_ExternalContactUpserted_RejectsMalformedBirthday(t *testing.T) {
	env := &Envelope{
		Kind: KindExternalContactUpserted,
		Payload: makeExternalContactUpsertedRaw(t, func(p *ExternalContactUpsertedPayload) {
			bad := "1990/04/21"
			p.Birthday = &bad
		}),
	}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "birthday")
}

func TestValidatePayload_ExternalContactUpserted_OmittedBirthdayAccepted(t *testing.T) {
	env := &Envelope{
		Kind: KindExternalContactUpserted,
		Payload: makeExternalContactUpsertedRaw(t, func(p *ExternalContactUpsertedPayload) {
			p.Birthday = nil
		}),
	}
	require.NoError(t, ValidatePayload(env))
}

func TestMarshalUnmarshal_ExternalContactUpserted_RoundTrip(t *testing.T) {
	bday := "1990-04-21"
	dn := "Sample User"
	org := "ACME"
	original := ExternalContactUpsertedPayload{
		Version:      1,
		HostID:       uuid.New(),
		Source:       "icloud_contacts",
		EntityID:     "CN-entity-2",
		DisplayName:  &dn,
		Emails:       []ExternalContactMethodValue{{Value: "u@example.com"}, {Value: "w@example.com", Type: "work"}},
		Phones:       []ExternalContactMethodValue{{Value: "+15551234567", Primary: true}},
		Addresses:    []ExternalContactAddressValue{{Formatted: "1 Test St", Type: "home"}},
		Organization: &org,
		Birthday:     &bday,
		Metadata:     map[string]any{"container_identifier": "CN-container-1"},
	}
	raw, err := Marshal(KindExternalContactUpserted, original)
	require.NoError(t, err)

	env := &Envelope{Kind: KindExternalContactUpserted, Payload: raw}
	var decoded ExternalContactUpsertedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original.EntityID, decoded.EntityID)
	require.Equal(t, original.HostID, decoded.HostID)
	require.Equal(t, original.Emails, decoded.Emails)
	require.Equal(t, original.Phones, decoded.Phones)
	require.Equal(t, original.Addresses, decoded.Addresses)
	require.Equal(t, *original.Birthday, *decoded.Birthday)
	require.Equal(t, original.Metadata, decoded.Metadata)
}

func TestValidatePayload_ExternalContactDeleted_AcceptsValid(t *testing.T) {
	p := ExternalContactDeletedPayload{
		Version:  1,
		HostID:   uuid.New(),
		Source:   "icloud_contacts",
		EntityID: "CN-entity-3",
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindExternalContactDeleted, Payload: raw}
	require.NoError(t, ValidatePayload(env))
}

func TestValidatePayload_ExternalContactDeleted_RejectsEmptyEntityID(t *testing.T) {
	p := ExternalContactDeletedPayload{
		Version: 1,
		HostID:  uuid.New(),
		Source:  "icloud_contacts",
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindExternalContactDeleted, Payload: raw}
	err = ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "entity_id is required")
}

func TestValidatePayload_ExternalContactDeleted_RejectsZeroHostID(t *testing.T) {
	p := ExternalContactDeletedPayload{
		Version:  1,
		Source:   "icloud_contacts",
		EntityID: "CN-entity-4",
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindExternalContactDeleted, Payload: raw}
	err = ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host_id is required")
}

// TestExternalContactPayload_JSONShape locks the omitempty wire shape
// for both new kinds — a future field rename or json tag drift would
// break daemon compatibility silently.
func TestExternalContactPayload_JSONShape(t *testing.T) {
	hid := uuid.New()
	p := ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   hid,
		Source:   "icloud_contacts",
		EntityID: "CN-1",
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	asMap := map[string]any{}
	require.NoError(t, json.Unmarshal(b, &asMap))
	// Required fields always present.
	require.Equal(t, float64(1), asMap["version"])
	require.Equal(t, hid.String(), asMap["host_id"])
	require.Equal(t, "icloud_contacts", asMap["source"])
	require.Equal(t, "CN-1", asMap["entity_id"])
	// Omit-empty fields are absent.
	_, hasEmails := asMap["emails"]
	require.False(t, hasEmails, "emails should be omitted when empty")
	_, hasBirthday := asMap["birthday"]
	require.False(t, hasBirthday, "birthday should be omitted when nil")
	_, hasMetadata := asMap["metadata"]
	require.False(t, hasMetadata, "metadata should be omitted when nil")
}

// ----------------------------------------------------------------------------
// meeting_note.* payload tests.
// ----------------------------------------------------------------------------

func makeMeetingNoteRecordedRaw(t *testing.T, mutate func(*MeetingNoteRecordedPayload)) json.RawMessage {
	t.Helper()
	title := "Sample Session"
	p := MeetingNoteRecordedPayload{
		Version:        1,
		HostID:         uuid.New(),
		Source:         "anarlog_sessions",
		SourceID:       uuid.NewString(),
		Title:          &title,
		MeetingAt:      time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC),
		ParticipantIDs: []string{uuid.NewString(), uuid.NewString()},
	}
	if mutate != nil {
		mutate(&p)
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return json.RawMessage(b)
}

func TestValidatePayload_MeetingNoteRecorded_AcceptsValid(t *testing.T) {
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: makeMeetingNoteRecordedRaw(t, nil)}
	require.NoError(t, ValidatePayload(env))
}

func TestValidatePayload_MeetingNoteRecorded_RejectsZeroHostID(t *testing.T) {
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: makeMeetingNoteRecordedRaw(t, func(p *MeetingNoteRecordedPayload) {
		p.HostID = uuid.Nil
	})}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host_id is required")
}

func TestValidatePayload_MeetingNoteRecorded_RejectsNonUUIDSourceID(t *testing.T) {
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: makeMeetingNoteRecordedRaw(t, func(p *MeetingNoteRecordedPayload) {
		p.SourceID = "not-a-uuid"
	})}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "source_id must be a UUID")
}

func TestValidatePayload_MeetingNoteRecorded_RejectsZeroMeetingAt(t *testing.T) {
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: makeMeetingNoteRecordedRaw(t, func(p *MeetingNoteRecordedPayload) {
		p.MeetingAt = time.Time{}
	})}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "meeting_at is required")
}

func TestValidatePayload_MeetingNoteRecorded_RejectsNonUUIDParticipant(t *testing.T) {
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: makeMeetingNoteRecordedRaw(t, func(p *MeetingNoteRecordedPayload) {
		p.ParticipantIDs = []string{uuid.NewString(), "not-a-uuid"}
	})}
	err := ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "participant_ids[1] not a UUID")
}

func TestValidatePayload_MeetingNoteDeleted_AcceptsValid(t *testing.T) {
	p := MeetingNoteDeletedPayload{
		Version:  1,
		HostID:   uuid.New(),
		Source:   "anarlog_sessions",
		SourceID: uuid.NewString(),
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMeetingNoteDeleted, Payload: raw}
	require.NoError(t, ValidatePayload(env))
}

func TestValidatePayload_MeetingNoteDeleted_RejectsZeroHostID(t *testing.T) {
	p := MeetingNoteDeletedPayload{
		Version:  1,
		Source:   "anarlog_sessions",
		SourceID: uuid.NewString(),
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMeetingNoteDeleted, Payload: raw}
	err = ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host_id is required")
}

func TestValidatePayload_MeetingNoteDeleted_RejectsNonUUIDSourceID(t *testing.T) {
	p := MeetingNoteDeletedPayload{
		Version:  1,
		HostID:   uuid.New(),
		Source:   "anarlog_sessions",
		SourceID: "not-a-uuid",
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMeetingNoteDeleted, Payload: raw}
	err = ValidatePayload(env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "source_id must be a UUID")
}

func TestMarshalUnmarshal_MeetingNoteRecorded_RoundTrip(t *testing.T) {
	title := "Round-Trip Test"
	summary := "session summary"
	original := MeetingNoteRecordedPayload{
		Version:        1,
		HostID:         uuid.New(),
		Source:         "anarlog_sessions",
		SourceID:       uuid.NewString(),
		Title:          &title,
		MeetingAt:      time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		Summary:        &summary,
		ParticipantIDs: []string{uuid.NewString(), uuid.NewString()},
		Tags:           []string{"tag-a", "tag-b"},
	}
	raw, err := Marshal(KindMeetingNoteRecorded, original)
	require.NoError(t, err)
	env := &Envelope{Kind: KindMeetingNoteRecorded, Payload: raw}
	var decoded MeetingNoteRecordedPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, original.SourceID, decoded.SourceID)
	require.Equal(t, original.HostID, decoded.HostID)
	require.Equal(t, original.ParticipantIDs, decoded.ParticipantIDs)
	require.Equal(t, original.Tags, decoded.Tags)
	require.True(t, original.MeetingAt.Equal(decoded.MeetingAt))
	require.Equal(t, *original.Title, *decoded.Title)
	require.Equal(t, *original.Summary, *decoded.Summary)
}

// ----------------------------------------------------------------------------
// call.* payload tests.
// ----------------------------------------------------------------------------

// TestValidatePayload_Call_RejectsBadInputs covers the per-kind value
// checks for CallPayload: unknown service, mismatched direction-vs-kind,
// empty required fields, negative duration.
func TestValidatePayload_Call_RejectsBadInputs(t *testing.T) {
	host := uuid.New()
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		kind   Kind
		mutate func(p *CallPayload)
		errSub string
	}{
		{
			name:   "unknown service",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.Service = "skype" },
			errSub: "service",
		},
		{
			name:   "service empty",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.Service = "" },
			errSub: "service",
		},
		{
			name:   "direction-vs-kind mismatch (received with outbound)",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.Direction = "outbound" },
			errSub: "direction",
		},
		{
			name:   "direction-vs-kind mismatch (sent with inbound)",
			kind:   KindCallSent,
			mutate: func(p *CallPayload) { p.Direction = "inbound" },
			errSub: "direction",
		},
		{
			name:   "empty call_unique_id",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.CallUniqueID = "" },
			errSub: "call_unique_id",
		},
		{
			name:   "empty peer_handle",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.PeerHandle = "" },
			errSub: "peer_handle",
		},
		{
			name:   "empty peer_normalized",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.PeerNormalized = "" },
			errSub: "peer_normalized",
		},
		{
			name:   "negative duration",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.DurationSeconds = -1 },
			errSub: "duration_seconds",
		},
		{
			name:   "zero started_at",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.StartedAt = time.Time{} },
			errSub: "started_at",
		},
		{
			name:   "empty host_id",
			kind:   KindCallReceived,
			mutate: func(p *CallPayload) { p.HostID = uuid.Nil },
			errSub: "host_id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answered := true
			p := CallPayload{
				Version: 1, HostID: host, Source: "phone_calls",
				CallUniqueID: "C-bad-input", PeerHandle: "+15551234567",
				PeerNormalized: "+15551234567", Service: "voice", Direction: "inbound",
				Answered: &answered, HasVoicemail: false, DurationSeconds: 1,
				StartedAt: at,
			}
			tc.mutate(&p)
			raw, err := Marshal(tc.kind, p)
			require.NoError(t, err)
			env := &Envelope{Kind: tc.kind, Payload: raw}
			err = ValidatePayload(env)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errSub)
		})
	}
}

// TestMarshalUnmarshal_CallPayload round-trips a connected inbound and a
// missed outbound call.
func TestMarshalUnmarshal_CallPayload(t *testing.T) {
	host := uuid.New()
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	answered := true
	inbound := CallPayload{
		Version: 1, HostID: host, Source: "phone_calls",
		CallUniqueID: "uniq-in-1", PeerHandle: "+1 (555) 123-4567",
		PeerNormalized: "+15551234567", Service: "voice", Direction: "inbound",
		Answered: &answered, HasVoicemail: false, DurationSeconds: 30,
		StartedAt: at,
	}
	raw, err := Marshal(KindCallReceived, inbound)
	require.NoError(t, err)
	env := &Envelope{Kind: KindCallReceived, Payload: raw}
	var decoded CallPayload
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, inbound, decoded)

	missedOutbound := CallPayload{
		Version: 1, HostID: host, Source: "phone_calls",
		CallUniqueID: "uniq-out-1", PeerHandle: "user@example.com",
		PeerNormalized: "user@example.com", Service: "facetime_audio", Direction: "outbound",
		Answered: nil, HasVoicemail: false, DurationSeconds: 0,
		StartedAt: at,
	}
	raw, err = Marshal(KindCallSent, missedOutbound)
	require.NoError(t, err)
	env = &Envelope{Kind: KindCallSent, Payload: raw}
	decoded = CallPayload{}
	require.NoError(t, Unmarshal(env, &decoded))
	require.Equal(t, missedOutbound, decoded)
}
