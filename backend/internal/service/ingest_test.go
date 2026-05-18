package service

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// TestIsHostOnlyKind_Whitelist verifies the allowlist contains exactly
// the daemon-emitted raw_message.* + external_contact.* kinds and
// rejects all others. Locks the allowlist contract at the unit level.
func TestIsHostOnlyKind_Whitelist(t *testing.T) {
	require.True(t, isHostOnlyKind(events.KindRawMessageReceived))
	require.True(t, isHostOnlyKind(events.KindRawMessageSent))
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
		require.False(t, isHostOnlyKind(k), "kind %s must NOT be on host-auth allowlist", k)
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

// ----------------------------------------------------------------------------
// external_contact.* unit tests.
//
// These cover the verifier + allowlist surface. End-to-end savepoint
// behavior (revive, delete-no-op, identity match) is covered by
// integration tests under backend/tests/api/.
// ----------------------------------------------------------------------------

func mustMarshalExtUpsert(p events.ExternalContactUpsertedPayload) []byte {
	b, err := events.Marshal(events.KindExternalContactUpserted, p)
	if err != nil {
		panic(err)
	}
	return b
}

func mustMarshalExtDelete(p events.ExternalContactDeletedPayload) []byte {
	b, err := events.Marshal(events.KindExternalContactDeleted, p)
	if err != nil {
		panic(err)
	}
	return b
}

// validUpsertedEnv returns a well-formed external_contact.upserted
// envelope keyed off the given host + entity id. The source_id's hash
// suffix is computed via the same JCS+SHA-256 recipe the production
// ingest path expects, so verifyExternalContactInvariants's hash check
// passes.
func validUpsertedEnv(host uuid.UUID, entityID string) *events.Envelope {
	payload := mustMarshalExtUpsert(events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   host,
		Source:   "icloud_contacts",
		EntityID: entityID,
	})
	hashHex, err := ComputeContentHash(payload)
	if err != nil {
		panic(err)
	}
	return &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: entityID + "@" + hashHex,
		Kind:     events.KindExternalContactUpserted,
		Payload:  payload,
	}
}

// validDeletedEnv mirrors validUpsertedEnv for the deleted kind. The
// hash suffix here is arbitrary (the delete handler validates against
// the row's STORED last_content_hash, not against the delete payload);
// we use a deterministic 64-hex tail for verifier shape tests.
func validDeletedEnv(host uuid.UUID, entityID string) *events.Envelope {
	hashHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	return &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: entityID + "@deleted@" + hashHex,
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version:  1,
			HostID:   host,
			Source:   "icloud_contacts",
			EntityID: entityID,
		}),
	}
}

func TestIsHostOnlyKind_IncludesExternalContact(t *testing.T) {
	require.True(t, isHostOnlyKind(events.KindExternalContactUpserted))
	require.True(t, isHostOnlyKind(events.KindExternalContactDeleted))
}

func TestIsExternalContactKind(t *testing.T) {
	require.True(t, isExternalContactKind(events.KindExternalContactUpserted))
	require.True(t, isExternalContactKind(events.KindExternalContactDeleted))
	require.False(t, isExternalContactKind(events.KindRawMessageReceived))
	require.False(t, isExternalContactKind(events.KindMessageReceived))
}

func TestVerifyExternalContactInvariants_HappyPath_Upsert(t *testing.T) {
	host := uuid.New()
	require.Nil(t, verifyExternalContactInvariants(validUpsertedEnv(host, "CN-1"), host))
}

func TestVerifyExternalContactInvariants_HappyPath_Delete(t *testing.T) {
	host := uuid.New()
	require.Nil(t, verifyExternalContactInvariants(validDeletedEnv(host, "CN-1"), host))
}

func TestVerifyExternalContactInvariants_DeleteWithUnknownHash(t *testing.T) {
	host := uuid.New()
	env := validDeletedEnv(host, "CN-1")
	env.SourceID = "CN-1@deleted@unknown"
	require.Nil(t, verifyExternalContactInvariants(env, host))
}

func TestVerifyExternalContactInvariants_HostMismatch(t *testing.T) {
	authHost := uuid.New()
	other := uuid.New()
	env := validUpsertedEnv(other, "CN-1")
	rej := verifyExternalContactInvariants(env, authHost)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "host_id")
}

func TestVerifyExternalContactInvariants_SourceMismatch(t *testing.T) {
	host := uuid.New()
	env := validUpsertedEnv(host, "CN-1")
	env.Source = "anarlog_humans"
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "not supported")
}

func TestVerifyExternalContactInvariants_PayloadVsEnvelopeSource(t *testing.T) {
	host := uuid.New()
	hashHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-1@" + hashHex,
		Kind:     events.KindExternalContactUpserted,
		Payload: mustMarshalExtUpsert(events.ExternalContactUpsertedPayload{
			Version:  1,
			HostID:   host,
			Source:   "different",
			EntityID: "CN-1",
		}),
	}
	// The payload-source check fires before the hash check, so an
	// arbitrary hash placeholder is fine.
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "payload source")
}

func TestVerifyExternalContactInvariants_EmptyEntityID(t *testing.T) {
	host := uuid.New()
	hashHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "@" + hashHex,
		Kind:     events.KindExternalContactUpserted,
		Payload: mustMarshalExtUpsert(events.ExternalContactUpsertedPayload{
			Version:  1,
			HostID:   host,
			Source:   "icloud_contacts",
			EntityID: "",
		}),
	}
	// The entity_id check fires before the hash check.
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "entity_id is required")
}

func TestVerifyExternalContactInvariants_BadSourceIDShape_Upsert(t *testing.T) {
	host := uuid.New()
	env := validUpsertedEnv(host, "CN-1")
	// SourceID with non-hex hash.
	env.SourceID = "CN-1@notanhash"
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "source_id")
}

func TestVerifyExternalContactInvariants_BadSourceIDShape_Delete(t *testing.T) {
	host := uuid.New()
	env := validDeletedEnv(host, "CN-1")
	// Wrong scheme — missing the @deleted@ segment.
	env.SourceID = "CN-1@" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "source_id")
}

func TestVerifyExternalContactInvariants_SourceIDEntityPrefixMismatch(t *testing.T) {
	host := uuid.New()
	env := validUpsertedEnv(host, "CN-1")
	hashHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// Envelope says CN-2 but payload entity_id is CN-1.
	env.SourceID = "CN-2@" + hashHex
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "entity prefix")
}

// stubExternalContactWriter records method invocations so tests can
// drive narrow scenarios without a live DB. The pgx.Tx args are
// ignored — the writer doesn't touch the DB at all.
type stubExternalContactWriter struct {
	getResp     *repository.ExternalContact
	getErr      error
	upsertResp  *repository.ExternalContact
	upsertErr   error
	updateResp  *repository.ExternalContact
	updateErr   error
	reviveResp  *repository.ExternalContact
	reviveErr   error
	softDelErr  error
	reviveCalls int
	updateCalls int
}

func (s *stubExternalContactWriter) GetBySourceTx(_ context.Context, _ pgx.Tx, _ string, _ string, _ *string) (*repository.ExternalContact, error) {
	return s.getResp, s.getErr
}
func (s *stubExternalContactWriter) UpsertTx(_ context.Context, _ pgx.Tx, _ repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	return s.upsertResp, s.upsertErr
}
func (s *stubExternalContactWriter) UpdateMatchTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ *uuid.UUID, _ repository.MatchStatus) (*repository.ExternalContact, error) {
	s.updateCalls++
	return s.updateResp, s.updateErr
}
func (s *stubExternalContactWriter) ReviveTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.ExternalContact, error) {
	s.reviveCalls++
	return s.reviveResp, s.reviveErr
}
func (s *stubExternalContactWriter) SoftDeleteTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) error {
	return s.softDelErr
}

// stubIdentityMatcher returns a configured MatchResult / error for
// every MatchOrCreateTx call.
type stubIdentityMatcher struct {
	result *MatchResult
	err    error
	calls  int
}

func (s *stubIdentityMatcher) MatchOrCreateTx(_ context.Context, _ pgx.Tx, _ MatchRequest) (*MatchResult, error) {
	s.calls++
	return s.result, s.err
}

// TestHandleExternalContactUpserted_PostUpsertTombstoneTriggersRevive
// exercises the defensive race-recovery path: the pre-read returns
// "not found" so wasTombstoned=false, but a concurrent delete
// tombstoned the row between our pre-read and our upsert, so UpsertTx
// returns a row with DeletedAt != nil. The handler MUST call ReviveTx
// in that case (post-upsert deleted_at check), even though
// wasTombstoned=false.
func TestHandleExternalContactUpserted_PostUpsertTombstoneTriggersRevive(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	deletedAt := accelerated.GetCurrentTime()
	postUpsertRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-race",
		MatchStatus: repository.MatchStatusUnmatched,
		DeletedAt:   &deletedAt, // concurrent delete tombstoned it
	}
	revivedRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-race",
		MatchStatus: repository.MatchStatusUnmatched,
		DeletedAt:   nil, // ReviveTx cleared it
	}
	stubExt := &stubExternalContactWriter{
		// Pre-read says "not found" — wasTombstoned will be false.
		getResp:    nil,
		getErr:     db.ErrNotFound,
		upsertResp: postUpsertRow, // but post-upsert returns a tombstoned row
		reviveResp: revivedRow,
	}
	stubIdent := &stubIdentityMatcher{result: &MatchResult{}}

	svc := &IngestService{
		identity:         stubIdent,
		externalContacts: stubExt,
	}
	env := validUpsertedEnv(hostID, "CN-race")
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej, "handler must not reject when revive succeeds")
	require.Equal(t, 1, stubExt.reviveCalls, "ReviveTx must be called when post-upsert row is tombstoned")
}

// TestHandleExternalContactUpserted_LiveUpsertSkipsRevive confirms the
// happy path does not invoke ReviveTx — keeps the previous test honest
// (a regression that always called ReviveTx would still pass the
// other test, but fail this one).
func TestHandleExternalContactUpserted_LiveUpsertSkipsRevive(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-live",
		MatchStatus: repository.MatchStatusUnmatched,
		DeletedAt:   nil,
	}
	stubExt := &stubExternalContactWriter{
		getResp:    nil,
		getErr:     db.ErrNotFound,
		upsertResp: liveRow,
	}
	stubIdent := &stubIdentityMatcher{result: &MatchResult{}}

	svc := &IngestService{
		identity:         stubIdent,
		externalContacts: stubExt,
	}
	env := validUpsertedEnv(hostID, "CN-live")
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej)
	require.Equal(t, 0, stubExt.reviveCalls, "ReviveTx must NOT be called on a live first-insert")
}

// TestVerifyExternalContactInvariants_HashMismatchRejected proves the
// Pi-side JCS recomputation catches a daemon-supplied source_id whose
// hash does NOT match the canonical SHA-256(JCS(payload \ {host_id})).
func TestVerifyExternalContactInvariants_HashMismatchRejected(t *testing.T) {
	host := uuid.New()
	env := validUpsertedEnv(host, "CN-1")
	// Replace the real hash with a different valid-shaped hex string.
	bogus := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	env.SourceID = "CN-1@" + bogus
	rej := verifyExternalContactInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectExternalContactHashMismatch, rej.Code)
	require.Contains(t, rej.Message, bogus)
}

// TestVerifyExternalContactInvariants_HashWithExtraHostIDStillMatches
// guards against a future implementer accidentally including host_id
// in the JCS input — sjson.DeleteBytes is the contract.
func TestVerifyExternalContactInvariants_HashWithExtraHostIDStillMatches(t *testing.T) {
	host := uuid.New()
	// validUpsertedEnv already includes host_id in the payload (the
	// Marshal step encodes the HostID field). The hash recipe strips
	// host_id before canonicalizing; if the strip stops working, the
	// hash recomputation would diverge from the source_id suffix.
	env := validUpsertedEnv(host, "CN-extra")
	require.Nil(t, verifyExternalContactInvariants(env, host))
}

// TestHandleExternalContactUpserted_WritesHostIDAndHash confirms the
// upsert request carries both new fields when the inline handler runs
// — enforces the application-level invariant that icloud_contacts
// rows always carry both fields.
func TestHandleExternalContactUpserted_WritesHostIDAndHash(t *testing.T) {
	hostID := uuid.New()
	rowID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-fields",
		MatchStatus: repository.MatchStatusUnmatched,
	}
	recorder := &recordingExternalContactWriter{
		stubExternalContactWriter: stubExternalContactWriter{
			getResp:    nil,
			getErr:     db.ErrNotFound,
			upsertResp: liveRow,
		},
	}
	svc := &IngestService{
		identity:         &stubIdentityMatcher{result: &MatchResult{}},
		externalContacts: recorder,
	}
	env := validUpsertedEnv(hostID, "CN-fields")
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej)
	require.NotNil(t, recorder.lastUpsert)
	require.NotNil(t, recorder.lastUpsert.HostID, "HostID must be set on the upsert request")
	require.Equal(t, hostID, *recorder.lastUpsert.HostID)
	require.NotNil(t, recorder.lastUpsert.LastContentHash, "LastContentHash must be set on the upsert request")
	require.Len(t, *recorder.lastUpsert.LastContentHash, 64,
		"LastContentHash must be the 64-char hex suffix from source_id")
	// The recorded hash must equal the suffix the verifier already
	// validated against the JCS recomputation.
	require.Equal(t, env.SourceID[len(env.SourceID)-64:], *recorder.lastUpsert.LastContentHash)
}

// TestHandleExternalContactDeleted_HashMatch accepts a delete whose
// source_id hash equals the stored last_content_hash.
func TestHandleExternalContactDeleted_HashMatch(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	rowID := uuid.New()
	storedRow := &repository.ExternalContact{
		ID:              rowID,
		Source:          "icloud_contacts",
		SourceID:        "CN-del",
		LastContentHash: &hash,
	}
	stubExt := &stubExternalContactWriter{getResp: storedRow}
	svc := &IngestService{externalContacts: stubExt}
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-del@deleted@" + hash,
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts", EntityID: "CN-del",
		}),
	}
	rej := svc.handleExternalContactDeleted(context.Background(), nil, env, uuid.UUID{})
	require.Nil(t, rej)
}

// TestHandleExternalContactDeleted_HashMismatchRejected proves the
// lookup-based hash check fires when source_id's hash differs from
// the row's stored last_content_hash.
func TestHandleExternalContactDeleted_HashMismatchRejected(t *testing.T) {
	storedHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	claimedHash := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	storedRow := &repository.ExternalContact{
		ID:              uuid.New(),
		Source:          "icloud_contacts",
		SourceID:        "CN-del",
		LastContentHash: &storedHash,
	}
	stubExt := &stubExternalContactWriter{getResp: storedRow}
	svc := &IngestService{externalContacts: stubExt}
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-del@deleted@" + claimedHash,
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts", EntityID: "CN-del",
		}),
	}
	rej := svc.handleExternalContactDeleted(context.Background(), nil, env, uuid.UUID{})
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectExternalContactDeleteHashMismatch, rej.Code)
	require.Contains(t, rej.Message, claimedHash)
	require.Contains(t, rej.Message, storedHash)
}

// TestHandleExternalContactDeleted_UnknownSentinelAccepted covers the
// spec line 343 fallback: @deleted@unknown skips the hash check.
func TestHandleExternalContactDeleted_UnknownSentinelAccepted(t *testing.T) {
	storedHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	storedRow := &repository.ExternalContact{
		ID:              uuid.New(),
		Source:          "icloud_contacts",
		SourceID:        "CN-del",
		LastContentHash: &storedHash,
	}
	stubExt := &stubExternalContactWriter{getResp: storedRow}
	svc := &IngestService{externalContacts: stubExt}
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-del@deleted@unknown",
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts", EntityID: "CN-del",
		}),
	}
	rej := svc.handleExternalContactDeleted(context.Background(), nil, env, uuid.UUID{})
	require.Nil(t, rej, "@unknown sentinel must be accepted even when stored hash exists")
}

// TestHandleExternalContactDeleted_NullStoredHashAccepted exercises
// the legacy-row exception: a pre-existing row has last_content_hash=NULL,
// so the Pi has no reference value to compare against. Accept and
// soft-delete.
func TestHandleExternalContactDeleted_NullStoredHashAccepted(t *testing.T) {
	storedRow := &repository.ExternalContact{
		ID:              uuid.New(),
		Source:          "icloud_contacts",
		SourceID:        "CN-legacy",
		LastContentHash: nil, // legacy row
	}
	stubExt := &stubExternalContactWriter{getResp: storedRow}
	svc := &IngestService{externalContacts: stubExt}
	anyHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-legacy@deleted@" + anyHash,
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version: 1, HostID: uuid.New(), Source: "icloud_contacts", EntityID: "CN-legacy",
		}),
	}
	rej := svc.handleExternalContactDeleted(context.Background(), nil, env, uuid.UUID{})
	require.Nil(t, rej, "legacy row with NULL stored hash must accept any delete hash")
}

// TestHandleExternalContactDeleted_CrossHostNoOp proves the host-scope
// guard: when prior.HostID != authenticatedHostID, the handler returns
// nil (silent no-op) without calling SoftDeleteTx. softDelErr is set to
// a sentinel; if the guard didn't fire, that error would surface as a
// rejection.
func TestHandleExternalContactDeleted_CrossHostNoOp(t *testing.T) {
	hostA := uuid.New()
	hostB := uuid.New()
	storedHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	storedRow := &repository.ExternalContact{
		ID:              uuid.New(),
		Source:          "icloud_contacts",
		SourceID:        "CN-xhost",
		HostID:          &hostA,
		LastContentHash: &storedHash,
	}
	stubExt := &stubExternalContactWriter{
		getResp:    storedRow,
		softDelErr: errors.New("SoftDeleteTx must not be called"),
	}
	svc := &IngestService{externalContacts: stubExt}
	env := &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: "CN-xhost@deleted@" + storedHash,
		Kind:     events.KindExternalContactDeleted,
		Payload: mustMarshalExtDelete(events.ExternalContactDeletedPayload{
			Version: 1, HostID: hostB, Source: "icloud_contacts", EntityID: "CN-xhost",
		}),
	}
	rej := svc.handleExternalContactDeleted(context.Background(), nil, env, hostB)
	require.Nil(t, rej, "cross-host delete must be a silent no-op")
}

// recordingExternalContactWriter wraps stubExternalContactWriter to
// snapshot the last UpsertTx request so tests can assert on the
// values the handler computed (HostID, LastContentHash, etc.).
type recordingExternalContactWriter struct {
	stubExternalContactWriter
	lastUpsert *repository.UpsertExternalContactRequest
}

func (r *recordingExternalContactWriter) UpsertTx(
	ctx context.Context, tx pgx.Tx, req repository.UpsertExternalContactRequest,
) (*repository.ExternalContact, error) {
	cp := req
	r.lastUpsert = &cp
	return r.stubExternalContactWriter.UpsertTx(ctx, tx, req)
}
