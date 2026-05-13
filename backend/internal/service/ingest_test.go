package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestIngestService_EmptyBatch_FastPath ensures the empty-slice case does
// NOT open a transaction — it returns (0, 0, nil, nil) immediately. Passing
// a nil database + nil bus would panic if the code tried to Begin on the
// pool, so the test proves the short-circuit works.
func TestIngestService_EmptyBatch_FastPath(t *testing.T) {
	svc := &IngestService{} // no DB, no bus — fast path must not touch them
	accepted, duplicate, rejections, err := svc.IngestBatch(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)

	// Same for an explicit zero-length slice.
	accepted, duplicate, rejections, err = svc.IngestBatch(context.Background(), []*events.Envelope{}, []int{}, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
}

// TestIngestService_RejectsNilEnvelope guards against a caller bug where
// an envelope slot is nil. Surface a caller error rather than panicking
// inside the publish loop once a transaction is open.
func TestIngestService_RejectsNilEnvelope(t *testing.T) {
	svc := &IngestService{} // precondition check fires before DB access
	_, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{nil}, []int{0}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "envelope at index 0 is nil")
}

// TestIngestService_RejectsPreAssignedID guards the fragile env.ID
// sentinel contract: callers MUST hand in uuid.Nil envelopes so the
// service can count duplicates correctly. A non-zero ID on entry would
// cause a duplicate to look accepted. Surface it as a caller bug at the
// service boundary.
func TestIngestService_RejectsPreAssignedID(t *testing.T) {
	svc := &IngestService{} // precondition check fires before DB access
	env := &events.Envelope{ID: uuid.New()}
	_, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{env}, []int{0}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pre-assigned ID")
}

// TestIngestService_RejectsLenMismatchOnOriginalIndices guards the
// callerProvided originalIndices contract: a length mismatch is a
// caller bug and must surface before we open a transaction.
func TestIngestService_RejectsLenMismatchOnOriginalIndices(t *testing.T) {
	svc := &IngestService{}
	env := &events.Envelope{} // valid (uuid.Nil)
	_, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{env}, []int{0, 1}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "originalIndices length")
}

// --- host-auth allowlist / raw_message invariant unit tests ---
//
// These tests cover the per-event domain rejections that the service
// can evaluate without touching the DB (allowlist + payload
// invariants). End-to-end tx-bound paths (savepoint behavior, identity
// match + staging upsert, aggregator enqueue) are covered by the
// integration tests under backend/tests/api/.

// TestIsHostAuthAllowedKind_Whitelist verifies the allowlist contains
// exactly the daemon-emitted raw_message.* kinds and rejects all
// others. Locks the allowlist contract at the unit level.
func TestIsHostAuthAllowedKind_Whitelist(t *testing.T) {
	require.True(t, isHostAuthAllowedKind(events.KindRawMessageReceived))
	require.True(t, isHostAuthAllowedKind(events.KindRawMessageSent))
	// Sample of kinds that MUST NOT be allowed on host-auth.
	for _, k := range []events.Kind{
		events.KindMessageReceived,
		events.KindMessageSent,
		events.KindCalendarAttended,
		events.KindTaskCompleted,
		events.KindInteractionManual,
		events.KindContactMethodsAdded,
		events.KindInteractionRecorded,
	} {
		require.False(t, isHostAuthAllowedKind(k), "kind %s must NOT be on host-auth allowlist", k)
	}
}

// TestIsRawMessageKind verifies the helper.
func TestIsRawMessageKind(t *testing.T) {
	require.True(t, isRawMessageKind(events.KindRawMessageReceived))
	require.True(t, isRawMessageKind(events.KindRawMessageSent))
	require.False(t, isRawMessageKind(events.KindMessageReceived))
	require.False(t, isRawMessageKind(events.KindCalendarAttended))
}

// TestVerifyRawMessageInvariants_HappyPath confirms a well-formed
// envelope + payload passes.
func TestVerifyRawMessageInvariants_HappyPath(t *testing.T) {
	host := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version:     1,
		HostID:      host,
		Source:      "messages",
		Guid:        "guid-1",
		ChatID:      "chat-1",
		PeerHandle:  "+15551234567",
		MessageType: "text",
	})
	env := &events.Envelope{
		Source:   "messages",
		SourceID: "guid-1",
		Kind:     events.KindRawMessageReceived,
		Payload:  pBytes,
	}
	require.Nil(t, verifyRawMessageInvariants(env, host))
}

// TestVerifyRawMessageInvariants_HostMismatch rejects when payload.HostID
// disagrees with the authenticated host.
func TestVerifyRawMessageInvariants_HostMismatch(t *testing.T) {
	authHost := uuid.New()
	other := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version: 1, HostID: other, Source: "messages", Guid: "g",
		ChatID: "c", PeerHandle: "x", MessageType: "text",
	})
	env := &events.Envelope{Source: "messages", SourceID: "g", Kind: events.KindRawMessageReceived, Payload: pBytes}
	rej := verifyRawMessageInvariants(env, authHost)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "host_id")
}

// TestVerifyRawMessageInvariants_SourceMismatch rejects an envelope
// whose source is not "messages" (currently the only supported one).
func TestVerifyRawMessageInvariants_SourceMismatch(t *testing.T) {
	host := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version: 1, HostID: host, Source: "whatsapp", Guid: "g",
		ChatID: "c", PeerHandle: "x", MessageType: "text",
	})
	env := &events.Envelope{Source: "whatsapp", SourceID: "g", Kind: events.KindRawMessageReceived, Payload: pBytes}
	rej := verifyRawMessageInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "not supported")
}

// TestVerifyRawMessageInvariants_PayloadSourceVsEnvelopeSource rejects
// when the payload's source disagrees with the envelope's source.
func TestVerifyRawMessageInvariants_PayloadSourceVsEnvelopeSource(t *testing.T) {
	host := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version: 1, HostID: host, Source: "different", Guid: "g",
		ChatID: "c", PeerHandle: "x", MessageType: "text",
	})
	env := &events.Envelope{Source: "messages", SourceID: "g", Kind: events.KindRawMessageReceived, Payload: pBytes}
	rej := verifyRawMessageInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "payload source")
}

// TestVerifyRawMessageInvariants_GuidMismatch rejects when payload.Guid
// disagrees with env.SourceID — the staging-table dedup key and the
// event-log dedup key MUST be the same string.
func TestVerifyRawMessageInvariants_GuidMismatch(t *testing.T) {
	host := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version: 1, HostID: host, Source: "messages", Guid: "guid-A",
		ChatID: "c", PeerHandle: "x", MessageType: "text",
	})
	env := &events.Envelope{Source: "messages", SourceID: "guid-B", Kind: events.KindRawMessageReceived, Payload: pBytes}
	rej := verifyRawMessageInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "must equal envelope source_id")
}

// TestVerifyRawMessageInvariants_EmptyGuid rejects when payload.Guid is
// empty even if env.SourceID matches — the guid IS the dedup key.
func TestVerifyRawMessageInvariants_EmptyGuid(t *testing.T) {
	host := uuid.New()
	pBytes := mustMarshalRawMsg(events.RawMessageReceivedPayload{
		Version: 1, HostID: host, Source: "messages",
		ChatID: "c", PeerHandle: "x", MessageType: "text",
	})
	env := &events.Envelope{Source: "messages", SourceID: "", Kind: events.KindRawMessageReceived, Payload: pBytes}
	rej := verifyRawMessageInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "guid is required")
}

func mustMarshalRawMsg(p events.RawMessageReceivedPayload) []byte {
	b, err := events.Marshal(events.KindRawMessageReceived, p)
	if err != nil {
		panic(err)
	}
	return b
}
