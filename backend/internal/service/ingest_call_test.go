package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// decideCallInteraction — pure decision-table verification (T1–T5 + T13).
// Covers every row of the content-delivered cadence table (spec
// §`phone_calls` source; settled S1, S2, S3).
// ---------------------------------------------------------------------------

func TestDecideCallInteraction_AnsweredInbound(t *testing.T) {
	// T1: inbound answered → interaction created, direction inbound.
	answered := true
	create, dir, desc := decideCallInteraction(false, &answered, false, 42, "voice")
	require.True(t, create)
	require.Equal(t, "inbound", dir)
	require.Contains(t, desc, "voice")
	require.Contains(t, desc, "42 sec")
}

func TestDecideCallInteraction_VoicemailInbound(t *testing.T) {
	// T2: inbound, not answered, has voicemail → interaction created.
	answered := false
	create, dir, desc := decideCallInteraction(false, &answered, true, 15, "voice")
	require.True(t, create)
	require.Equal(t, "inbound", dir)
	require.Contains(t, desc, "voicemail")
	require.Contains(t, desc, "15 sec")
}

func TestDecideCallInteraction_MissedInbound_NoVoicemail(t *testing.T) {
	// T3 + T13: inbound missed, no voicemail → NO interaction (the
	// staging row IS created; the event-log row IS published; only the
	// interaction row is skipped per content-delivered cadence).
	answered := false
	create, dir, desc := decideCallInteraction(false, &answered, false, 0, "voice")
	require.False(t, create)
	require.Empty(t, dir)
	require.Empty(t, desc)
}

func TestDecideCallInteraction_MissedInbound_AnsweredNil(t *testing.T) {
	// Same as T3 but answered=nil (payload had it omitted). No
	// voicemail → no interaction.
	create, _, _ := decideCallInteraction(false, nil, false, 0, "voice")
	require.False(t, create)
}

func TestDecideCallInteraction_ConnectedOutbound(t *testing.T) {
	// T4: outbound connected (duration > 0) → interaction created,
	// direction outbound, answered IGNORED on outbound.
	answered := true
	create, dir, desc := decideCallInteraction(true, &answered, false, 60, "voice")
	require.True(t, create)
	require.Equal(t, "outbound", dir)
	require.Contains(t, desc, "voice")
	require.Contains(t, desc, "60 sec")
}

func TestDecideCallInteraction_MissedOutbound(t *testing.T) {
	// T5: outbound missed (duration = 0) → interaction created, the
	// "attempted to reach" signal (S3). Description marks it "missed".
	create, dir, desc := decideCallInteraction(true, nil, false, 0, "voice")
	require.True(t, create)
	require.Equal(t, "outbound", dir)
	require.Contains(t, desc, "missed")
}

func TestDecideCallInteraction_FaceTimeAudio_RendersLabel(t *testing.T) {
	answered := true
	_, _, desc := decideCallInteraction(false, &answered, false, 30, "facetime_audio")
	require.Contains(t, desc, "FaceTime audio")
}

func TestDecideCallInteraction_FaceTimeVideo_RendersLabel(t *testing.T) {
	answered := true
	_, _, desc := decideCallInteraction(false, &answered, false, 30, "facetime_video")
	require.Contains(t, desc, "FaceTime video")
}

// ---------------------------------------------------------------------------
// verifyCallInvariants — payload-cross-field invariants.
// ---------------------------------------------------------------------------

func validCallEnv(t *testing.T, kind events.Kind, hostID uuid.UUID, callUniqueID string) *events.Envelope {
	t.Helper()
	answered := true
	direction := "inbound"
	if kind == events.KindCallSent {
		direction = "outbound"
	}
	p := events.CallPayload{
		Version:         1,
		HostID:          hostID,
		Source:          "phone_calls",
		CallUniqueID:    callUniqueID,
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         "voice",
		Direction:       direction,
		Answered:        &answered,
		HasVoicemail:    false,
		DurationSeconds: 42,
		StartedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(kind, p)
	require.NoError(t, err)
	return &events.Envelope{
		Source:     "phone_calls",
		SourceID:   callUniqueID,
		Kind:       kind,
		Payload:    raw,
		ObservedAt: p.StartedAt,
	}
}

func TestVerifyCallInvariants_HappyPath_Received(t *testing.T) {
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "uniq-1")
	rej := verifyCallInvariants(env, host)
	require.Nil(t, rej)
}

func TestVerifyCallInvariants_HappyPath_Sent(t *testing.T) {
	host := uuid.New()
	env := validCallEnv(t, events.KindCallSent, host, "uniq-2")
	rej := verifyCallInvariants(env, host)
	require.Nil(t, rej)
}

func TestVerifyCallInvariants_HostIDMismatch(t *testing.T) {
	host := uuid.New()
	otherHost := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "uniq-3")
	rej := verifyCallInvariants(env, otherHost)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "host_id")
}

func TestVerifyCallInvariants_RejectsUnknownSource(t *testing.T) {
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "uniq-4")
	env.Source = "skype" // not in allowedCallSources
	rej := verifyCallInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "source")
}

func TestVerifyCallInvariants_RejectsSourceIDMismatch(t *testing.T) {
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "uniq-5")
	env.SourceID = "different-id"
	rej := verifyCallInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "source_id")
}

func TestVerifyCallInvariants_RejectsPeerNormalizedMismatch(t *testing.T) {
	// T17: payload peer_normalized doesn't match Pi's re-canonicalization.
	host := uuid.New()
	answered := true
	p := events.CallPayload{
		Version:         1,
		HostID:          host,
		Source:          "phone_calls",
		CallUniqueID:    "uniq-mismatch",
		PeerHandle:      "+1 (555) 123-4567", // becomes "+15551234567"
		PeerNormalized:  "+19998887777",      // wrong
		Service:         "voice",
		Direction:       "inbound",
		Answered:        &answered,
		DurationSeconds: 1,
		StartedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(events.KindCallReceived, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "phone_calls", SourceID: "uniq-mismatch", Kind: events.KindCallReceived, Payload: raw}
	rej := verifyCallInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	require.Contains(t, rej.Message, "peer_normalized")
}

func TestVerifyCallInvariants_RejectsUnnormalizablePeer(t *testing.T) {
	// "+abc" is detected as a phone (starts with +) but
	// NormalizePhoneE164 returns empty (no digits) → reject with
	// IDENTITY_MATCH_FAILED.
	host := uuid.New()
	p := events.CallPayload{
		Version:         1,
		HostID:          host,
		Source:          "phone_calls",
		CallUniqueID:    "uniq-unnorm",
		PeerHandle:      "+abc",
		PeerNormalized:  "+abc",
		Service:         "voice",
		Direction:       "inbound",
		HasVoicemail:    false,
		DurationSeconds: 1,
		StartedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(events.KindCallReceived, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "phone_calls", SourceID: "uniq-unnorm", Kind: events.KindCallReceived, Payload: raw}
	rej := verifyCallInvariants(env, host)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectIdentityMatchFailed, rej.Code)
}

// ---------------------------------------------------------------------------
// isCallKind / isHostOnlyKind allowlist tests (T8, T9 — partial).
// ---------------------------------------------------------------------------

func TestIsCallKind(t *testing.T) {
	require.True(t, isCallKind(events.KindCallReceived))
	require.True(t, isCallKind(events.KindCallSent))
	require.False(t, isCallKind(events.KindRawMessageReceived))
	require.False(t, isCallKind(events.KindExternalContactUpserted))
}

func TestIsHostOnlyKind_IncludesCalls(t *testing.T) {
	require.True(t, isHostOnlyKind(events.KindCallReceived))
	require.True(t, isHostOnlyKind(events.KindCallSent))
}

// ---------------------------------------------------------------------------
// handleCall — stub-driven decision-branch coverage.
//
// These tests exercise the actual handleCall function using stubs for
// every collaborator (identity, phoneCalls, contactRecorder, bus,
// cadence, followUp). They cover: missed-no-voicemail SKIPS the
// interaction emit; qualified rows DO publish + cadence; identity-match
// failure rejects; staging-upsert failure rejects; outbound forces
// answered=NULL on staging.
// ---------------------------------------------------------------------------

// stubPhoneCallWriter records UpsertCallTx / MarkProcessedTx inputs and
// returns configurable responses. The pgx.Tx is ignored — the stub
// doesn't touch the DB.
type stubPhoneCallWriter struct {
	upsertParams      []repository.UpsertPhoneCallParams
	upsertResp        *repository.PhoneCall
	upsertErr         error
	markProcessedArgs []repository.MarkProcessedParams
	markProcessedErr  error
}

func (s *stubPhoneCallWriter) UpsertCallTx(_ context.Context, _ pgx.Tx, params repository.UpsertPhoneCallParams) (*repository.PhoneCall, error) {
	s.upsertParams = append(s.upsertParams, params)
	if s.upsertErr != nil {
		return nil, s.upsertErr
	}
	if s.upsertResp != nil {
		return s.upsertResp, nil
	}
	// Default response: synthesize a row that echoes the input so the
	// handler can use the returned ID in MarkProcessed.
	return &repository.PhoneCall{
		ID:             uuid.New(),
		CallUniqueID:   params.CallUniqueID,
		PeerHandle:     params.PeerHandle,
		PeerNormalized: params.PeerNormalized,
		Service:        params.Service,
		Direction:      params.Direction,
		Answered:       params.Answered,
		HasVoicemail:   params.HasVoicemail,
	}, nil
}

func (s *stubPhoneCallWriter) MarkProcessedTx(_ context.Context, _ pgx.Tx, params repository.MarkProcessedParams) error {
	s.markProcessedArgs = append(s.markProcessedArgs, params)
	return s.markProcessedErr
}

// stubContactRecorder records RecordInteractionTx calls.
type stubContactRecorder struct {
	calls    []repository.RecordInteractionRequest
	response *repository.RecordInteractionResult
	err      error
}

func (s *stubContactRecorder) RecordInteractionTx(_ context.Context, _ pgx.Tx, _ bool, req repository.RecordInteractionRequest) (*repository.RecordInteractionResult, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.response != nil {
		return s.response, nil
	}
	return &repository.RecordInteractionResult{
		Interaction: &repository.Interaction{
			ID:         uuid.New(),
			ContactID:  req.ContactID,
			Source:     req.Source,
			SourceRef:  req.SourceRef,
			OccurredAt: req.OccurredAt,
		},
		IsReplay: false,
	}, nil
}

// stubCadenceApplier records HandleEvent calls.
type stubCadenceApplier struct {
	calls int
	err   error
}

func (s *stubCadenceApplier) HandleEvent(_ context.Context, _ pgx.Tx, _ *events.Envelope) error {
	s.calls++
	return s.err
}

// stubFollowUpApplier records HandleEvent calls and returns a
// configurable post-commit closure.
type stubFollowUpApplier struct {
	calls            int
	err              error
	returnPostCommit func(context.Context)
}

func (s *stubFollowUpApplier) HandleEvent(_ context.Context, _ pgx.Tx, _ *events.Envelope) (func(context.Context), error) {
	s.calls++
	return s.returnPostCommit, s.err
}

// stubEventBusForCall is a no-op bus stub that records publishes. The
// IngestService's bus field is a concrete *events.Bus pointer; to keep
// these stub-driven tests possible, the tests must skip the publish path
// by configuring the recorder to return a replay (IsReplay=true) — that
// path short-circuits before bus.PublishTx is called.
//
// For non-replay tests, we use a real bus over a stub DB pool. That's
// architecturally heavier, so the publish-path tests live in the
// integration suite under backend/tests/api/ instead. Here we cover the
// branches handleCall can reach WITHOUT touching the bus.

// callTestHarness bundles stubs + IngestService for a handleCall test.
type callTestHarness struct {
	svc             *IngestService
	phoneCalls      *stubPhoneCallWriter
	contactRecorder *stubContactRecorder
	cadence         *stubCadenceApplier
	followUp        *stubFollowUpApplier
	identity        *stubIdentityMatcher
}

func newCallTestHarness(matchedContactID *uuid.UUID) *callTestHarness {
	pcWriter := &stubPhoneCallWriter{}
	recorder := &stubContactRecorder{}
	cadence := &stubCadenceApplier{}
	followUp := &stubFollowUpApplier{}
	ident := &stubIdentityMatcher{
		result: &MatchResult{ContactID: matchedContactID},
	}
	svc := &IngestService{
		identity:        ident,
		phoneCalls:      pcWriter,
		contactRecorder: recorder,
		cadence:         cadence,
		followUp:        followUp,
		// bus intentionally nil — replay-path tests don't hit it; other
		// branches' tests either configure recorder.response.IsReplay=true
		// (replay short-circuit) or configure phoneCalls.upsertErr to
		// reject before reaching the bus.
	}
	return &callTestHarness{
		svc:             svc,
		phoneCalls:      pcWriter,
		contactRecorder: recorder,
		cadence:         cadence,
		followUp:        followUp,
		identity:        ident,
	}
}

// TestHandleCall_MissedInboundNoVoicemail_SkipsInteractionEmit (T3 +
// expanded): a missed inbound with no voicemail upserts the staging
// row, marks it processed with interaction_id=NIL, and does NOT call
// the contact recorder or the bus. The staging row IS still durable
// for the future timeline UI (R2).
func TestHandleCall_MissedInboundNoVoicemail_SkipsInteractionEmit(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)

	host := uuid.New()
	answered := false
	p := events.CallPayload{
		Version:        1,
		HostID:         host,
		Source:         "phone_calls",
		CallUniqueID:   "missed-1",
		PeerHandle:     "+15551234567",
		PeerNormalized: "+15551234567",
		Service:        "voice",
		Direction:      "inbound",
		Answered:       &answered,
		HasVoicemail:   false,
		StartedAt:      time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(events.KindCallReceived, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "phone_calls", SourceID: "missed-1", Kind: events.KindCallReceived, Payload: raw}

	postCommit, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.Nil(t, rej)
	require.Nil(t, postCommit)

	require.Len(t, h.phoneCalls.upsertParams, 1, "staging row must be upserted")
	require.Len(t, h.phoneCalls.markProcessedArgs, 1, "row must be marked processed even when no interaction is created")
	require.Nil(t, h.phoneCalls.markProcessedArgs[0].InteractionID, "missed-no-voicemail row keeps interaction_id NULL")
	require.Empty(t, h.contactRecorder.calls, "contact recorder must NOT be called for missed-no-voicemail")
}

// TestHandleCall_Outbound_ForcesAnsweredNullOnStaging (T11): outbound
// rows ignore the daemon's `answered` value and write NULL on the
// staging row. R5 + spec line 421.
func TestHandleCall_Outbound_ForcesAnsweredNullOnStaging(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)
	// Configure recorder to short-circuit to replay so we don't hit
	// the bus.
	existing := &repository.Interaction{
		ID:         uuid.New(),
		ContactID:  contactID,
		Source:     "phone_calls",
		OccurredAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	h.contactRecorder.response = &repository.RecordInteractionResult{
		Interaction: existing,
		IsReplay:    true,
	}

	host := uuid.New()
	// Daemon mis-sets answered=true on outbound — Pi must force NULL.
	bogusAnswered := true
	p := events.CallPayload{
		Version:         1,
		HostID:          host,
		Source:          "phone_calls",
		CallUniqueID:    "outbound-1",
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         "voice",
		Direction:       "outbound",
		Answered:        &bogusAnswered, // wrongly set
		HasVoicemail:    true,           // also wrong for outbound — Pi forces false
		DurationSeconds: 30,
		StartedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(events.KindCallSent, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "phone_calls", SourceID: "outbound-1", Kind: events.KindCallSent, Payload: raw}

	_, rej := h.svc.handleCall(context.Background(), nil, env, host, true)
	require.Nil(t, rej)

	require.Len(t, h.phoneCalls.upsertParams, 1)
	got := h.phoneCalls.upsertParams[0]
	require.Equal(t, "outbound", got.Direction)
	require.Nil(t, got.Answered, "outbound must force answered=NULL on staging (R5)")
	require.False(t, got.HasVoicemail, "outbound must force has_voicemail=FALSE on staging (R4)")
}

// TestHandleCall_Replay_ShortCircuitsBusEmit (T6): a re-push of the
// same call hits the recorder's replay path (IsReplay=true); the
// handler links the staging row to the existing interaction and
// returns without calling the bus or cadence.
func TestHandleCall_Replay_ShortCircuitsBusEmit(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)
	existingInteractionID := uuid.New()
	h.contactRecorder.response = &repository.RecordInteractionResult{
		Interaction: &repository.Interaction{
			ID:         existingInteractionID,
			ContactID:  contactID,
			Source:     "phone_calls",
			OccurredAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		},
		IsReplay: true,
	}

	host := uuid.New()
	answered := true
	p := events.CallPayload{
		Version:         1,
		HostID:          host,
		Source:          "phone_calls",
		CallUniqueID:    "replay-1",
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         "voice",
		Direction:       "inbound",
		Answered:        &answered,
		HasVoicemail:    false,
		DurationSeconds: 42,
		StartedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	raw, err := events.Marshal(events.KindCallReceived, p)
	require.NoError(t, err)
	env := &events.Envelope{Source: "phone_calls", SourceID: "replay-1", Kind: events.KindCallReceived, Payload: raw}

	postCommit, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.Nil(t, rej)
	require.Nil(t, postCommit)

	require.Len(t, h.contactRecorder.calls, 1, "recorder still called to fetch existing interaction")
	require.Equal(t, 0, h.cadence.calls, "cadence NOT applied on replay")
	require.Equal(t, 0, h.followUp.calls, "followUp NOT applied on replay")
	require.Len(t, h.phoneCalls.markProcessedArgs, 1)
	require.NotNil(t, h.phoneCalls.markProcessedArgs[0].InteractionID)
	require.Equal(t, existingInteractionID, *h.phoneCalls.markProcessedArgs[0].InteractionID,
		"staging row's interaction_id must point at the replay's existing interaction")
}

// TestHandleCall_IdentityMatchFailed_Rejects (T10): an un-normalizable
// peer (or one with no matching contact) rejects with
// IDENTITY_MATCH_FAILED so the daemon retries.
func TestHandleCall_IdentityMatchFailed_Rejects(t *testing.T) {
	// matchedContactID = nil → no contact matched
	h := newCallTestHarness(nil)
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "no-match-1")
	_, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectIdentityMatchFailed, rej.Code)
}

// TestHandleCall_IdentityMatchError_Rejects: the IdentityService returns
// an error → rejection bubbles up.
func TestHandleCall_IdentityMatchError_Rejects(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)
	h.identity.err = errors.New("transient db error")
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "ident-err-1")
	_, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectIdentityMatchFailed, rej.Code)
}

// TestHandleCall_StagingUpsertFailed_Rejects: phoneCalls.UpsertCallTx
// errors → rejection bubbles up with STAGING_UPSERT_FAILED.
func TestHandleCall_StagingUpsertFailed_Rejects(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)
	h.phoneCalls.upsertErr = errors.New("FK violation")
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "upsert-err-1")
	_, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectStagingUpsertFailed, rej.Code)
}

// TestHandleCall_MissingDependencies_Rejects: the wiring guard rejects
// when the service is misconfigured (defence in depth).
func TestHandleCall_MissingDependencies_Rejects(t *testing.T) {
	host := uuid.New()
	env := validCallEnv(t, events.KindCallReceived, host, "missing-deps")

	t.Run("no_identity", func(t *testing.T) {
		svc := &IngestService{phoneCalls: &stubPhoneCallWriter{}, contactRecorder: &stubContactRecorder{}}
		_, rej := svc.handleCall(context.Background(), nil, env, host, false)
		require.NotNil(t, rej)
		require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	})

	t.Run("no_phoneCalls", func(t *testing.T) {
		svc := &IngestService{identity: &stubIdentityMatcher{result: &MatchResult{}}, contactRecorder: &stubContactRecorder{}}
		_, rej := svc.handleCall(context.Background(), nil, env, host, false)
		require.NotNil(t, rej)
		require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	})

	t.Run("no_contactRecorder", func(t *testing.T) {
		svc := &IngestService{identity: &stubIdentityMatcher{result: &MatchResult{}}, phoneCalls: &stubPhoneCallWriter{}}
		_, rej := svc.handleCall(context.Background(), nil, env, host, false)
		require.NotNil(t, rej)
		require.Equal(t, ingestRejectPayloadInvariant, rej.Code)
	})
}

// TestHandleCall_PayloadDecodeError: malformed payload bytes reach the
// in-function json.Unmarshal (the IngestBatch path runs the validator
// first, but the per-handler decode is a defence-in-depth seam).
func TestHandleCall_PayloadDecodeError(t *testing.T) {
	contactID := uuid.New()
	h := newCallTestHarness(&contactID)
	host := uuid.New()
	env := &events.Envelope{
		Source:   "phone_calls",
		SourceID: "decode-err",
		Kind:     events.KindCallReceived,
		Payload:  json.RawMessage("{not json"),
	}
	_, rej := h.svc.handleCall(context.Background(), nil, env, host, false)
	require.NotNil(t, rej)
	require.Equal(t, ingestRejectPayloadInvalid, rej.Code)
}
