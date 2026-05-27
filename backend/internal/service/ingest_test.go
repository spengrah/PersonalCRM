package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
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
	accepted, duplicate, rejections, needsAttention, err := svc.IngestBatch(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
	require.Empty(t, needsAttention)

	// Same for an explicit zero-length slice.
	accepted, duplicate, rejections, needsAttention, err = svc.IngestBatch(context.Background(), []*events.Envelope{}, []int{}, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
	require.Empty(t, needsAttention)
}

// TestIngestService_RejectsNilEnvelope guards against a caller bug where
// an envelope slot is nil. Surface a caller error rather than panicking
// inside the publish loop once a transaction is open.
func TestIngestService_RejectsNilEnvelope(t *testing.T) {
	svc := &IngestService{} // precondition check fires before DB access
	_, _, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{nil}, []int{0}, nil)
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
	_, _, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{env}, []int{0}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pre-assigned ID")
}

// TestIngestService_RejectsLenMismatchOnOriginalIndices guards the
// callerProvided originalIndices contract: a length mismatch is a
// caller bug and must surface before we open a transaction.
func TestIngestService_RejectsLenMismatchOnOriginalIndices(t *testing.T) {
	svc := &IngestService{}
	env := &events.Envelope{} // valid (uuid.Nil)
	_, _, _, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{env}, []int{0, 1}, nil)
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
	env.Source = "gcontacts" // not in allowedExternalContactSources
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
// every MatchOrCreateTx call. requests records the inputs of each call
// so tests can assert which raw values reached the matcher (used by the
// "skip un-normalizable identifiers" tests, which need to prove that
// junk values like "+" or "   " never even hit the matcher).
type stubIdentityMatcher struct {
	result   *MatchResult
	err      error
	calls    int
	requests []MatchRequest
}

func (s *stubIdentityMatcher) MatchOrCreateTx(_ context.Context, _ pgx.Tx, req MatchRequest) (*MatchResult, error) {
	s.calls++
	s.requests = append(s.requests, req)
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

// upsertEnvWithContactMethods builds an external_contact.upserted
// envelope with the provided emails/phones. Mirrors validUpsertedEnv
// but lets callers populate contact-method values, which the
// un-normalizable-identifier tests need. The source_id hash is
// recomputed over the actual payload bytes so the envelope would pass
// verifyExternalContactInvariants if it were run (these unit tests
// invoke handleExternalContactUpserted directly so the verifier is
// bypassed regardless, but recomputing keeps the helper honest).
func upsertEnvWithContactMethods(t *testing.T, host uuid.UUID, entityID string, emails, phones []string) *events.Envelope {
	t.Helper()
	em := make([]events.ExternalContactMethodValue, 0, len(emails))
	for _, v := range emails {
		em = append(em, events.ExternalContactMethodValue{Value: v})
	}
	ph := make([]events.ExternalContactMethodValue, 0, len(phones))
	for _, v := range phones {
		ph = append(ph, events.ExternalContactMethodValue{Value: v})
	}
	payload := mustMarshalExtUpsert(events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   host,
		Source:   "icloud_contacts",
		EntityID: entityID,
		Emails:   em,
		Phones:   ph,
	})
	hashHex, err := ComputeContentHash(payload)
	require.NoError(t, err)
	return &events.Envelope{
		Source:   "icloud_contacts",
		SourceID: entityID + "@" + hashHex,
		Kind:     events.KindExternalContactUpserted,
		Payload:  payload,
	}
}

// TestHandleExternalContactUpserted_SkipsPhonesThatNormalizeToEmpty
// proves the call-site pre-check filters phones whose value normalizes
// to empty (e.g. "+", whitespace-only) BEFORE invoking MatchOrCreateTx.
// This is the unit-level proof that the cursor-stall fix avoids the
// lower-layer "empty identifier after normalization" rejection — the
// matcher never sees those values at all.
func TestHandleExternalContactUpserted_SkipsPhonesThatNormalizeToEmpty(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-junk-phone",
		MatchStatus: repository.MatchStatusUnmatched,
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
	env := upsertEnvWithContactMethods(t, hostID, "CN-junk-phone",
		nil, []string{"+", "   ", "+15551234567"})
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej, "handler must not reject when junk phones are skipped")
	require.Equal(t, 1, stubIdent.calls, "matcher must be invoked exactly once (for the valid phone)")
	require.Len(t, stubIdent.requests, 1)
	require.Equal(t, "+15551234567", stubIdent.requests[0].RawIdentifier)
	require.Equal(t, identity.IdentifierTypePhone, stubIdent.requests[0].Type)
}

// TestHandleExternalContactUpserted_SkipsEmailsThatNormalizeToEmpty
// mirrors the phone case for emails. Whitespace/tab-only values must
// not reach MatchOrCreateTx.
func TestHandleExternalContactUpserted_SkipsEmailsThatNormalizeToEmpty(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-junk-email",
		MatchStatus: repository.MatchStatusUnmatched,
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
	env := upsertEnvWithContactMethods(t, hostID, "CN-junk-email",
		[]string{"   ", "\t", "ok@example.com"}, nil)
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej, "handler must not reject when junk emails are skipped")
	require.Equal(t, 1, stubIdent.calls, "matcher must be invoked exactly once (for the valid email)")
	require.Len(t, stubIdent.requests, 1)
	require.Equal(t, "ok@example.com", stubIdent.requests[0].RawIdentifier)
	require.Equal(t, identity.IdentifierTypeEmail, stubIdent.requests[0].Type)
}

// TestHandleExternalContactUpserted_AllPhonesAndEmailsNormalizeToEmpty_NoMatcherCalls
// proves that an envelope whose every identifier normalizes to empty
// still ingests cleanly — no matcher calls, no rejection. The contact
// lands as an external_contact row with zero linked identities,
// available for manual review.
func TestHandleExternalContactUpserted_AllPhonesAndEmailsNormalizeToEmpty_NoMatcherCalls(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-all-junk",
		MatchStatus: repository.MatchStatusUnmatched,
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
	env := upsertEnvWithContactMethods(t, hostID, "CN-all-junk",
		[]string{"\t"}, []string{"+"})
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej, "handler must accept envelope even if all identifiers normalize to empty")
	require.Equal(t, 0, stubIdent.calls, "matcher must NOT be invoked when all identifiers are un-normalizable")
}

// TestHandleExternalContactUpserted_MixedJunkAndValid_CallsMatcherOnceWithValid
// proves both loops run to completion and the matcher receives exactly
// the two valid values, in source order (emails first, then phones).
// Junk in one loop must not short-circuit the other.
func TestHandleExternalContactUpserted_MixedJunkAndValid_CallsMatcherOnceWithValid(t *testing.T) {
	rowID := uuid.New()
	hostID := uuid.New()
	liveRow := &repository.ExternalContact{
		ID:          rowID,
		Source:      "icloud_contacts",
		SourceID:    "CN-mixed",
		MatchStatus: repository.MatchStatusUnmatched,
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
	env := upsertEnvWithContactMethods(t, hostID, "CN-mixed",
		[]string{"   ", "ok@example.com"},
		[]string{"+", "+15551234567"})
	rej := svc.handleExternalContactUpserted(context.Background(), nil, env, hostID)
	require.Nil(t, rej)
	require.Equal(t, 2, stubIdent.calls, "matcher must be invoked once per valid identifier")
	require.Len(t, stubIdent.requests, 2)
	// Emails loop runs first, then phones.
	require.Equal(t, "ok@example.com", stubIdent.requests[0].RawIdentifier)
	require.Equal(t, identity.IdentifierTypeEmail, stubIdent.requests[0].Type)
	require.Equal(t, "+15551234567", stubIdent.requests[1].RawIdentifier)
	require.Equal(t, identity.IdentifierTypePhone, stubIdent.requests[1].Type)
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

// ----------------------------------------------------------------------------
// meeting_note.* invariant + helper unit tests
// ----------------------------------------------------------------------------

// TestIsMeetingNoteKind exercises the kind-classifier helper.
func TestIsMeetingNoteKind(t *testing.T) {
	require.True(t, isMeetingNoteKind(events.KindMeetingNoteRecorded))
	require.True(t, isMeetingNoteKind(events.KindMeetingNoteDeleted))
	require.False(t, isMeetingNoteKind(events.KindExternalContactUpserted))
	require.False(t, isMeetingNoteKind(events.KindMessageReceived))
}

// validMeetingNoteRecordedEnv constructs a structurally-valid envelope
// for verifyMeetingNoteInvariants tests. The content hash is computed
// per the canonical recipe so the verifier's hash check passes.
func validMeetingNoteRecordedEnv(t *testing.T, host uuid.UUID, sessionUUID string) *events.Envelope {
	t.Helper()
	title := "Test Session"
	p := events.MeetingNoteRecordedPayload{
		Version:   1,
		HostID:    host,
		Source:    "anarlog_sessions",
		SourceID:  sessionUUID,
		Title:     &title,
		MeetingAt: time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC),
	}
	pBytes, err := events.Marshal(events.KindMeetingNoteRecorded, p)
	require.NoError(t, err)
	hashHex, err := ComputeContentHash(pBytes)
	require.NoError(t, err)
	return &events.Envelope{
		Source:   "anarlog_sessions",
		SourceID: sessionUUID + "@" + hashHex,
		Kind:     events.KindMeetingNoteRecorded,
		Payload:  pBytes,
	}
}

func TestVerifyMeetingNoteInvariants_HappyPath(t *testing.T) {
	host := uuid.New()
	env := validMeetingNoteRecordedEnv(t, host, uuid.NewString())
	require.Nil(t, verifyMeetingNoteInvariants(env, host))
}

func TestVerifyMeetingNoteInvariants_HostMismatch(t *testing.T) {
	authHost := uuid.New()
	otherHost := uuid.New()
	env := validMeetingNoteRecordedEnv(t, otherHost, uuid.NewString())
	rej := verifyMeetingNoteInvariants(env, authHost)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "host_id")
}

func TestVerifyMeetingNoteInvariants_SourceMismatch(t *testing.T) {
	host := uuid.New()
	env := validMeetingNoteRecordedEnv(t, host, uuid.NewString())
	env.Source = "icloud_contacts" // not in allowedMeetingNoteSources
	rej := verifyMeetingNoteInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "not supported")
}

func TestVerifyMeetingNoteInvariants_NonUUIDSourceID(t *testing.T) {
	host := uuid.New()
	// Hand-craft payload with non-UUID source_id; ValidatePayload would
	// also catch this at the handler boundary, but verifyMeetingNoteInvariants
	// runs the service-side check for defense-in-depth.
	p := events.MeetingNoteRecordedPayload{
		Version: 1, HostID: host, Source: "anarlog_sessions",
		SourceID: "not-a-uuid", MeetingAt: accelerated.GetCurrentTime(),
	}
	pBytes, err := events.Marshal(events.KindMeetingNoteRecorded, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "anarlog_sessions", SourceID: "not-a-uuid@" + strings.Repeat("a", 64),
		Kind: events.KindMeetingNoteRecorded, Payload: pBytes}
	rej := verifyMeetingNoteInvariants(env, host)
	require.NotNil(t, rej)
	require.Contains(t, rej.Message, "not a UUID")
}

func TestVerifyMeetingNoteInvariants_HashMismatch(t *testing.T) {
	host := uuid.New()
	env := validMeetingNoteRecordedEnv(t, host, uuid.NewString())
	// Tamper with the source_id hash suffix while keeping the regex shape.
	tampered := env.SourceID[:len(env.SourceID)-64] + strings.Repeat("0", 64)
	env.SourceID = tampered
	rej := verifyMeetingNoteInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectMeetingNoteHashMismatch, rej.Code)
}

func TestVerifyMeetingNoteInvariants_DeleteAcceptsUnknownSentinel(t *testing.T) {
	host := uuid.New()
	sessionUUID := uuid.NewString()
	p := events.MeetingNoteDeletedPayload{
		Version: 1, HostID: host, Source: "anarlog_sessions", SourceID: sessionUUID,
	}
	pBytes, err := events.Marshal(events.KindMeetingNoteDeleted, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "anarlog_sessions", SourceID: sessionUUID + "@deleted@unknown",
		Kind: events.KindMeetingNoteDeleted, Payload: pBytes}
	require.Nil(t, verifyMeetingNoteInvariants(env, host))
}

// TestComputeMeetingNoteInputHash_StableAcrossOrder confirms that the
// input hash is invariant under participant_id order — sorting happens
// inside the recipe.
func TestComputeMeetingNoteInputHash_StableAcrossOrder(t *testing.T) {
	at := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	title := "Same Title"
	a := []string{"uuid-1", "uuid-2", "uuid-3"}
	b := []string{"uuid-3", "uuid-1", "uuid-2"}
	hashA, err := computeMeetingNoteInputHash(at, &title, a)
	require.NoError(t, err)
	hashB, err := computeMeetingNoteInputHash(at, &title, b)
	require.NoError(t, err)
	require.Equal(t, hashA, hashB)
}

// TestComputeMeetingNoteInputHash_TitleAffectsHash confirms the title
// is part of the recipe — a future title-parser does not have to
// migrate hashes.
func TestComputeMeetingNoteInputHash_TitleAffectsHash(t *testing.T) {
	at := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	ids := []string{"uuid-1"}
	a := "Alpha"
	b := "Bravo"
	hashA, err := computeMeetingNoteInputHash(at, &a, ids)
	require.NoError(t, err)
	hashB, err := computeMeetingNoteInputHash(at, &b, ids)
	require.NoError(t, err)
	require.NotEqual(t, hashA, hashB)
}

// TestComputeResolvedSetHash_StableAcrossOrder confirms order
// independence for the resolved-contact-id set hash.
func TestComputeResolvedSetHash_StableAcrossOrder(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()
	hashA, err := computeResolvedSetHash([]uuid.UUID{id1, id2, id3}, nil)
	require.NoError(t, err)
	hashB, err := computeResolvedSetHash([]uuid.UUID{id3, id1, id2}, nil)
	require.NoError(t, err)
	require.Equal(t, hashA, hashB)
}

// TestComputeResolvedSetHash_UnionsTaggedAndTitleMatched confirms the
// hash includes BOTH tag-resolved and title-matched contact IDs so a
// drift in either resolution path bumps the hash.
func TestComputeResolvedSetHash_UnionsTaggedAndTitleMatched(t *testing.T) {
	tagged := uuid.New()
	titled := uuid.New()
	taggedOnly, err := computeResolvedSetHash([]uuid.UUID{tagged}, nil)
	require.NoError(t, err)
	taggedAndTitled, err := computeResolvedSetHash([]uuid.UUID{tagged}, []uuid.UUID{titled})
	require.NoError(t, err)
	require.NotEqual(t, taggedOnly, taggedAndTitled,
		"adding a title-matched contact must change the hash")

	// Same set via different paths → same hash (dedup across slices).
	viaTagged, err := computeResolvedSetHash([]uuid.UUID{tagged, titled}, nil)
	require.NoError(t, err)
	viaSplit, err := computeResolvedSetHash([]uuid.UUID{tagged}, []uuid.UUID{titled})
	require.NoError(t, err)
	require.Equal(t, viaTagged, viaSplit,
		"hash depends on the union, not which slice contributed which id")
}

// TestComputeResolvedSetHash_EmptyTitleMatchedPreservesLegacyHash
// confirms the carry-forward semantic stays intact for legacy rows
// that have no title-matched contacts.
func TestComputeResolvedSetHash_EmptyTitleMatchedPreservesLegacyHash(t *testing.T) {
	tagged := uuid.New()
	withNil, err := computeResolvedSetHash([]uuid.UUID{tagged}, nil)
	require.NoError(t, err)
	withEmpty, err := computeResolvedSetHash([]uuid.UUID{tagged}, []uuid.UUID{})
	require.NoError(t, err)
	require.Equal(t, withNil, withEmpty)
}

// TestDecideLinkage_NoCandidatesNoTagged returns orphan_needs_review
// with no interactions.
func TestDecideLinkage_NoCandidatesNoTagged(t *testing.T) {
	sessionID := uuid.New()
	state, kind, id, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, nil, nil, nil)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Empty(t, desired)
}

// TestDecideLinkage_NoCandidatesNoTaggedWithTitleMatches confirms the
// spec invariant: title matches DO NOT promote a no-anchor orphan to
// augmented (the anchor must be tagged humans).
func TestDecideLinkage_NoCandidatesNoTaggedWithTitleMatches(t *testing.T) {
	sessionID := uuid.New()
	titled := uuid.New()
	titleMatched := []resolvedTitle{
		{Token: "Alice", NormalizedToken: "alice", ContactID: titled},
	}
	state, kind, id, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, nil, nil, titleMatched)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Empty(t, desired, "title matches alone never produce interactions")
}

// TestDecideLinkage_NoCandidatesWithTagged returns linked_impromptu and
// one interaction per resolved tagged contact.
func TestDecideLinkage_NoCandidatesWithTagged(t *testing.T) {
	sessionID := uuid.New()
	cA := uuid.New()
	cB := uuid.New()
	resolved := []resolvedTag{
		{AnarlogID: "human-a", ContactID: cA},
		{AnarlogID: "human-b", ContactID: cB},
	}
	state, kind, id, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, nil, resolved, nil)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Len(t, desired, 2)
	refA := "anarlog:" + sessionID.String() + ":" + cA.String()
	refB := "anarlog:" + sessionID.String() + ":" + cB.String()
	gotRefs := []string{desired[0].SourceRef, desired[1].SourceRef}
	require.Contains(t, gotRefs, refA)
	require.Contains(t, gotRefs, refB)
}

// TestDecideLinkage_NoCandidatesWithTaggedAndTitleMatches returns
// orphan_title_augmented with one tagged interaction PLUS one
// `:title:` interaction per title-matched contact.
func TestDecideLinkage_NoCandidatesWithTaggedAndTitleMatches(t *testing.T) {
	sessionID := uuid.New()
	cTagged := uuid.New()
	cTitled := uuid.New()
	resolved := []resolvedTag{{AnarlogID: "human-tagged", ContactID: cTagged}}
	titleMatched := []resolvedTitle{
		{Token: "Alice", NormalizedToken: "alice", ContactID: cTitled},
	}
	state, kind, id, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, nil, resolved, titleMatched)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Len(t, desired, 2)
	taggedRef := "anarlog:" + sessionID.String() + ":" + cTagged.String()
	titleRef := "anarlog:" + sessionID.String() + ":title:" + cTitled.String()
	gotRefs := []string{desired[0].SourceRef, desired[1].SourceRef}
	require.Contains(t, gotRefs, taggedRef)
	require.Contains(t, gotRefs, titleRef)
}

// TestDecideLinkage_OneCandidate_WalkinPresent_NoSupplement verifies
// that a tagged contact already in the event's matched_contact_ids
// does NOT produce a walk-in interaction.
func TestDecideLinkage_OneCandidate_WalkinPresent_NoSupplement(t *testing.T) {
	sessionID := uuid.New()
	cA := uuid.New()
	cand := repository.LinkageCandidate{
		Kind:               "event",
		ID:                 uuid.New(),
		OccurredAt:         accelerated.GetCurrentTime(),
		AttendeeContactIDs: []uuid.UUID{cA},
	}
	resolved := []resolvedTag{{AnarlogID: "human-a", ContactID: cA}}
	state, kind, id, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, []repository.LinkageCandidate{cand}, resolved, nil)
	require.Equal(t, repository.LinkageStateLinked, state)
	require.NotNil(t, kind)
	require.Equal(t, "event", *kind)
	require.NotNil(t, id)
	require.Equal(t, cand.ID, *id)
	require.Empty(t, desired, "tagged contact already in event attendees → no walk-in")
}

// TestDecideLinkage_OneCandidate_TaggedNotInAttendees_AddsWalkin
// fires the walk-in supplemental interaction for the missing contact.
func TestDecideLinkage_OneCandidate_TaggedNotInAttendees_AddsWalkin(t *testing.T) {
	sessionID := uuid.New()
	cA := uuid.New() // in attendees
	cB := uuid.New() // tagged but NOT in attendees → walk-in
	cand := repository.LinkageCandidate{
		Kind:               "event",
		ID:                 uuid.New(),
		AttendeeContactIDs: []uuid.UUID{cA},
	}
	resolved := []resolvedTag{{AnarlogID: "human-b", ContactID: cB}}
	state, kind, _, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, []repository.LinkageCandidate{cand}, resolved, nil)
	require.Equal(t, repository.LinkageStateLinked, state)
	require.Equal(t, "event", *kind)
	require.Len(t, desired, 1)
	expectedRef := "anarlog:" + sessionID.String() + ":walkin:" + cB.String()
	require.Equal(t, expectedRef, desired[0].SourceRef)
	require.Equal(t, cB, desired[0].ContactID)
}

// TestDecideLinkage_OneCandidate_TitleMatchesDoNotProduceInteractions
// verifies the spec invariant that title matches don't create
// interactions in the linked state.
func TestDecideLinkage_OneCandidate_TitleMatchesDoNotProduceInteractions(t *testing.T) {
	sessionID := uuid.New()
	cAttendee := uuid.New()
	cTitled := uuid.New()
	cand := repository.LinkageCandidate{
		Kind:               "event",
		ID:                 uuid.New(),
		AttendeeContactIDs: []uuid.UUID{cAttendee},
	}
	titleMatched := []resolvedTitle{
		{Token: "Alice", NormalizedToken: "alice", ContactID: cTitled},
	}
	state, _, _, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, []repository.LinkageCandidate{cand}, nil, titleMatched)
	require.Equal(t, repository.LinkageStateLinked, state)
	require.Empty(t, desired, "title matches must not produce interactions in linked state")
}

// TestDecideLinkage_MultipleCandidates_ConflictPending verifies that
// 2+ candidates with no implied-set signal (no tagged, no title-matched)
// land on conflict_pending and surface the per-candidate snapshot.
func TestDecideLinkage_MultipleCandidates_ConflictPending(t *testing.T) {
	sessionID := uuid.New()
	cands := []repository.LinkageCandidate{
		{Kind: "event", ID: uuid.New()},
		{Kind: "event", ID: uuid.New()},
	}
	state, kind, id, desired, snap := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, cands, nil, nil)
	require.Equal(t, repository.LinkageStateConflictPending, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Empty(t, desired)
	require.Len(t, snap, 2, "snapshot includes every candidate")
	for _, s := range snap {
		require.Equal(t, 0, s.OverlapCount, "empty implied set → overlap 0")
	}
}

// TestDecideLinkage_MultipleCandidates_TitleMatchesDoNotInteract
// confirms that when title matches don't overlap with any candidate's
// attendees, the result is still conflict_pending with no interactions.
func TestDecideLinkage_MultipleCandidates_TitleMatchesDoNotInteract(t *testing.T) {
	sessionID := uuid.New()
	cands := []repository.LinkageCandidate{
		{Kind: "event", ID: uuid.New()},
		{Kind: "event", ID: uuid.New()},
	}
	titled := uuid.New()
	titleMatched := []resolvedTitle{
		{Token: "Alice", NormalizedToken: "alice", ContactID: titled},
	}
	state, _, _, desired, _ := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, cands, nil, titleMatched)
	require.Equal(t, repository.LinkageStateConflictPending, state)
	require.Empty(t, desired)
}

// ----------------------------------------------------------------------------
// disambiguateCandidates Step 3 algorithm
// ----------------------------------------------------------------------------

// TestDisambiguateCandidates_EmptyImplied returns nil winner and a
// snapshot with all overlap=0 — no signal to disambiguate with.
func TestDisambiguateCandidates_EmptyImplied(t *testing.T) {
	cands := []repository.LinkageCandidate{
		{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{uuid.New()}},
		{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{uuid.New()}},
	}
	winner, snap := disambiguateCandidates(cands, map[uuid.UUID]struct{}{})
	require.Nil(t, winner)
	require.Len(t, snap, 2)
	for _, s := range snap {
		require.Equal(t, 0, s.OverlapCount)
	}
}

// TestDisambiguateCandidates_StrictWinnerUniqueOverlap picks the
// candidate whose attendees cover more of the implied set.
func TestDisambiguateCandidates_StrictWinnerUniqueOverlap(t *testing.T) {
	cA := uuid.New()
	cB := uuid.New()
	cC := uuid.New()
	cand1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA, cB}}
	cand2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cC}}
	cands := []repository.LinkageCandidate{cand1, cand2}
	implied := map[uuid.UUID]struct{}{cA: {}, cB: {}}
	winner, snap := disambiguateCandidates(cands, implied)
	require.NotNil(t, winner)
	require.Equal(t, cand1.ID, winner.ID)
	require.Equal(t, 2, snap[0].OverlapCount)
	require.Equal(t, 0, snap[1].OverlapCount)
}

// TestDisambiguateCandidates_TiedAtTop returns nil winner; the strictly-
// highest rule fails when two candidates share the top overlap.
func TestDisambiguateCandidates_TiedAtTop(t *testing.T) {
	cA := uuid.New()
	cand1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	cand2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	winner, snap := disambiguateCandidates([]repository.LinkageCandidate{cand1, cand2}, map[uuid.UUID]struct{}{cA: {}})
	require.Nil(t, winner)
	require.Len(t, snap, 2)
	require.Equal(t, 1, snap[0].OverlapCount)
	require.Equal(t, 1, snap[1].OverlapCount)
}

// TestDisambiguateCandidates_TiedAtTopWithThirdLess — top two tied,
// third strictly less. Still no winner because the top tier is tied.
func TestDisambiguateCandidates_TiedAtTopWithThirdLess(t *testing.T) {
	cA := uuid.New()
	cB := uuid.New()
	cC := uuid.New()
	c1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA, cB}}
	c2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA, cB}}
	c3 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cC}}
	winner, snap := disambiguateCandidates([]repository.LinkageCandidate{c1, c2, c3}, map[uuid.UUID]struct{}{cA: {}, cB: {}, cC: {}})
	require.Nil(t, winner)
	require.Equal(t, 2, snap[0].OverlapCount)
	require.Equal(t, 2, snap[1].OverlapCount)
	require.Equal(t, 1, snap[2].OverlapCount)
}

// TestDisambiguateCandidates_StrictWinnerAmongThree resolves correctly
// when overlaps are [3,1,1].
func TestDisambiguateCandidates_StrictWinnerAmongThree(t *testing.T) {
	cA := uuid.New()
	cB := uuid.New()
	cC := uuid.New()
	c1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA, cB, cC}}
	c2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	c3 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cB}}
	winner, _ := disambiguateCandidates([]repository.LinkageCandidate{c1, c2, c3}, map[uuid.UUID]struct{}{cA: {}, cB: {}, cC: {}})
	require.NotNil(t, winner)
	require.Equal(t, c1.ID, winner.ID)
}

// TestDisambiguateCandidates_PhoneCallPeerMatch — phone_call wins
// when its peer covers the only implied contact while the event has zero
// overlap.
func TestDisambiguateCandidates_PhoneCallPeerMatch(t *testing.T) {
	cA := uuid.New()
	evt := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New()}
	call := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall, ID: uuid.New(), PeerContactID: &cA}
	winner, _ := disambiguateCandidates([]repository.LinkageCandidate{evt, call}, map[uuid.UUID]struct{}{cA: {}})
	require.NotNil(t, winner)
	require.Equal(t, call.ID, winner.ID)
}

// TestDisambiguateCandidates_PhoneCallPeerNil — a phone_call candidate
// with PeerContactID=nil contributes zero overlap; the event with
// overlap=1 wins.
func TestDisambiguateCandidates_PhoneCallPeerNil(t *testing.T) {
	cA := uuid.New()
	evt := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	call := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall, ID: uuid.New(), PeerContactID: nil}
	winner, _ := disambiguateCandidates([]repository.LinkageCandidate{evt, call}, map[uuid.UUID]struct{}{cA: {}})
	require.NotNil(t, winner)
	require.Equal(t, evt.ID, winner.ID)
}

// TestDisambiguateCandidates_SnapshotDeterministicSort — same overlap,
// earlier occurred_at sorts first.
func TestDisambiguateCandidates_SnapshotDeterministicSort(t *testing.T) {
	cA := uuid.New()
	base := accelerated.GetCurrentTime()
	c1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), OccurredAt: base.Add(2 * time.Minute), AttendeeContactIDs: []uuid.UUID{cA}}
	c2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), OccurredAt: base, AttendeeContactIDs: []uuid.UUID{cA}}
	c3 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), OccurredAt: base.Add(1 * time.Minute), AttendeeContactIDs: []uuid.UUID{cA}}
	_, snap := disambiguateCandidates([]repository.LinkageCandidate{c1, c2, c3}, map[uuid.UUID]struct{}{cA: {}})
	require.Len(t, snap, 3)
	require.Equal(t, c2.ID, snap[0].ID)
	require.Equal(t, c3.ID, snap[1].ID)
	require.Equal(t, c1.ID, snap[2].ID)
}

// TestDisambiguateCandidates_SingleZeroOverlap — defensive: helper
// handles single-candidate input even though the gate is len>=2 in the
// caller, returning nil winner because the sole overlap is 0.
func TestDisambiguateCandidates_SingleZeroOverlap(t *testing.T) {
	c1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{uuid.New()}}
	winner, snap := disambiguateCandidates([]repository.LinkageCandidate{c1}, map[uuid.UUID]struct{}{})
	require.Nil(t, winner)
	require.Len(t, snap, 1)
	require.Equal(t, 0, snap[0].OverlapCount)
}

// TestDisambiguateCandidates_LargeMixedKindSet — 5 candidates spanning
// event + phone_call kinds with mixed implied membership; verify the
// exact winner is the one with the highest overlap.
func TestDisambiguateCandidates_LargeMixedKindSet(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	implied := map[uuid.UUID]struct{}{a: {}, b: {}, c: {}}

	winningEvent := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{a, b, c}}
	losingEvent := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{a, uuid.New()}}
	bystanderEvent := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{uuid.New()}}
	matchingCall := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall, ID: uuid.New(), PeerContactID: &b}
	unrelatedCall := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall, ID: uuid.New()}

	winner, snap := disambiguateCandidates(
		[]repository.LinkageCandidate{winningEvent, losingEvent, bystanderEvent, matchingCall, unrelatedCall},
		implied,
	)
	require.NotNil(t, winner)
	require.Equal(t, winningEvent.ID, winner.ID)
	require.Equal(t, 3, snap[0].OverlapCount, "winning event covers a,b,c")
}

// TestBuildImpliedSet covers the union semantics.
func TestBuildImpliedSet(t *testing.T) {
	cA := uuid.New()
	cB := uuid.New()
	cT := uuid.New()
	tagged := []resolvedTag{{AnarlogID: "x", ContactID: cA}, {AnarlogID: "y", ContactID: cB}}
	titles := []resolvedTitle{{Token: "T", NormalizedToken: "t", ContactID: cT}}
	got := buildImpliedSet(tagged, titles)
	require.Len(t, got, 3)
	_, hasA := got[cA]
	_, hasB := got[cB]
	_, hasT := got[cT]
	require.True(t, hasA)
	require.True(t, hasB)
	require.True(t, hasT)
}

// TestLinkageCandidateImpliedAttendeeSet covers the kind-aware
// attendee-set helper used by Step 5's walk-in computation.
func TestLinkageCandidateImpliedAttendeeSet(t *testing.T) {
	cA := uuid.New()
	cB := uuid.New()
	evt := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, AttendeeContactIDs: []uuid.UUID{cA, cB}}
	got := evt.ImpliedAttendeeSet()
	require.Len(t, got, 2)
	_, hasA := got[cA]
	_, hasB := got[cB]
	require.True(t, hasA)
	require.True(t, hasB)

	call := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall, PeerContactID: &cA}
	got = call.ImpliedAttendeeSet()
	require.Len(t, got, 1)
	_, hasA = got[cA]
	require.True(t, hasA)

	callNilPeer := repository.LinkageCandidate{Kind: repository.LinkedKindPhoneCall}
	got = callNilPeer.ImpliedAttendeeSet()
	require.Empty(t, got)
}

// TestDecideLinkage_Step3_StrictWinnerAutoLinks — a tagged participant
// breaks the tie and the algorithm auto-links to the strict winner,
// emitting the same Step 5 walk-in supplemental as if there had been
// one candidate from the start.
func TestDecideLinkage_Step3_StrictWinnerAutoLinks(t *testing.T) {
	sessionID := uuid.New()
	cA := uuid.New()
	cB := uuid.New()
	winning := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	losing := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cB}}
	resolved := []resolvedTag{{AnarlogID: "human-a", ContactID: cA}}

	state, kind, id, desired, snap := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, []repository.LinkageCandidate{winning, losing}, resolved, nil)
	require.Equal(t, repository.LinkageStateLinked, state)
	require.NotNil(t, kind)
	require.Equal(t, repository.LinkedKindEvent, *kind)
	require.NotNil(t, id)
	require.Equal(t, winning.ID, *id)
	require.Empty(t, desired, "tagged contact already in winning attendees → no walk-in")
	require.NotEmpty(t, snap, "snapshot returned for observability even on auto-link")
	require.Equal(t, 1, snap[0].OverlapCount, "winner's overlap surfaces for logging")
}

// TestDecideLinkage_Step3_TiedTopFallsThroughToConflict — when two
// candidates tie at the top, the conflict_pending state stands and the
// snapshot is returned for persistence.
func TestDecideLinkage_Step3_TiedTopFallsThroughToConflict(t *testing.T) {
	sessionID := uuid.New()
	cA := uuid.New()
	c1 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	c2 := repository.LinkageCandidate{Kind: repository.LinkedKindEvent, ID: uuid.New(), AttendeeContactIDs: []uuid.UUID{cA}}
	resolved := []resolvedTag{{AnarlogID: "human-a", ContactID: cA}}

	state, kind, id, desired, snap := decideLinkage(events.MeetingNoteRecordedPayload{}, sessionID, []repository.LinkageCandidate{c1, c2}, resolved, nil)
	require.Equal(t, repository.LinkageStateConflictPending, state)
	require.Nil(t, kind)
	require.Nil(t, id)
	require.Empty(t, desired)
	require.Len(t, snap, 2)
}
