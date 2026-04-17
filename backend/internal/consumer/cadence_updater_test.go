package consumer

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

// -----------------------------------------------------------------------------
// Test stubs for CadenceUpdater.HandleEvent. The worker tx is a typed nil
// (no stub dereferences it). The shadow-repo stub captures the last insert
// for assertions.
// -----------------------------------------------------------------------------

type stubCadenceShadowRepo struct {
	recordConsumerCalls int
	recordConsumerErr   error
	lastConsumerObs     *repository.CadenceShadowObservation

	directObs       *repository.CadenceShadowObservation
	findDirectErr   error
	findDirectCalls int
}

func (s *stubCadenceShadowRepo) RecordConsumer(_ context.Context, _ pgx.Tx, obs repository.CadenceShadowObservation) error {
	s.recordConsumerCalls++
	if s.recordConsumerErr != nil {
		return s.recordConsumerErr
	}
	obsCopy := obs
	s.lastConsumerObs = &obsCopy
	return nil
}

func (s *stubCadenceShadowRepo) FindMatchingDirect(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.CadenceShadowObservation, error) {
	s.findDirectCalls++
	if s.findDirectErr != nil {
		return nil, s.findDirectErr
	}
	return s.directObs, nil
}

func newCadenceUpdaterWithStubs(mode string) (*CadenceUpdater, *stubCadenceShadowRepo, *stubBus) {
	shadowRepo := &stubCadenceShadowRepo{}
	bus := &stubBus{}
	h := NewCadenceUpdater(shadowRepo, bus, mode)
	return h, shadowRepo, bus
}

// mustInteractionRecordedEnv builds an interaction.recorded envelope with
// the given V2 payload fields. The envelope gets a random ID; OccurredAt
// defaults to April 10, 2026 12:00 UTC unless the payload supplies one.
func mustInteractionRecordedEnv(t *testing.T, payload events.InteractionRecordedPayload) *events.Envelope {
	t.Helper()
	if payload.OccurredAt.IsZero() {
		payload.OccurredAt = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	}
	if payload.Source == "" {
		payload.Source = "telegram"
	}
	if payload.InteractionID == uuid.Nil {
		payload.InteractionID = uuid.New()
	}
	raw, err := events.Marshal(events.KindInteractionRecorded, payload)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     payload.Source,
		SourceID:   payload.InteractionID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    raw,
		ObservedAt: payload.OccurredAt,
	}
}

func strPtr(s string) *string { return &s }

// -----------------------------------------------------------------------------
// CadenceApplyFlagsByDirection matrix — direction-rule sanity.
// -----------------------------------------------------------------------------

func TestCadenceApplyFlagsByDirection(t *testing.T) {
	cases := []struct {
		direction                      string
		wantLC, wantLO, wantLR, wantCB bool
	}{
		{"outbound", false, true, false, false},
		// Inbound matches today's UpdateContactResponseFields — does NOT
		// bump last_outreach_at (plan Decision 3 + Risk 11).
		{"inbound", true, false, true, true},
		{"mutual", true, true, true, true},
		{"", false, false, false, false},
		{"garbage", false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.direction, func(t *testing.T) {
			lc, lo, lr, cb := repository.CadenceApplyFlagsByDirection(c.direction)
			require.Equal(t, c.wantLC, lc, "applyLastContacted")
			require.Equal(t, c.wantLO, lo, "applyLastOutreachAt")
			require.Equal(t, c.wantLR, lr, "applyLastResponseAt")
			require.Equal(t, c.wantCB, cb, "applyContactBy (direction gate)")
		})
	}
}

// -----------------------------------------------------------------------------
// ShouldApplyContactBy matrix.
// -----------------------------------------------------------------------------

func TestShouldApplyContactBy(t *testing.T) {
	t0 := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	tLater := t0.Add(24 * time.Hour)
	tEarlier := t0.Add(-24 * time.Hour)

	require.False(t, repository.ShouldApplyContactBy(nil, t0, false, false), "no cadence → false")
	require.True(t, repository.ShouldApplyContactBy(nil, t0, true, true), "manual + cadence → true")
	require.True(t, repository.ShouldApplyContactBy(nil, t0, false, true), "nil prev + cadence → true")
	require.True(t, repository.ShouldApplyContactBy(&t0, tLater, false, true), "later incoming → true")
	require.False(t, repository.ShouldApplyContactBy(&t0, t0, false, true), "equal incoming → false (strict >)")
	require.False(t, repository.ShouldApplyContactBy(&t0, tEarlier, false, true), "earlier incoming → false")
	require.True(t, repository.ShouldApplyContactBy(&t0, tEarlier, true, true), "manual overrides forward guard")
}

// -----------------------------------------------------------------------------
// ForwardMax sanity — strict > semantics (plan Decision 4).
// -----------------------------------------------------------------------------

func TestForwardMax_StrictGreaterThan(t *testing.T) {
	t0 := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	require.Equal(t, t0, repository.ForwardMax(nil, t0), "nil prev → incoming wins")
	require.Equal(t, t0.Add(time.Hour), repository.ForwardMax(&t0, t0.Add(time.Hour)), "later incoming wins")
	require.Equal(t, t0, repository.ForwardMax(&t0, t0), "equal incoming does NOT advance (strict >)")
	earlier := t0.Add(-time.Hour)
	require.Equal(t, t0, repository.ForwardMax(&t0, earlier), "earlier incoming → prev wins")
}

// -----------------------------------------------------------------------------
// HandleEvent — mode gating.
// -----------------------------------------------------------------------------

func TestHandleEvent_ModeOff_ShortCircuits(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeOff)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, shadow.recordConsumerCalls, "off mode: no consumer observation")
	require.Zero(t, shadow.findDirectCalls, "off mode: no direct lookup")
}

// -----------------------------------------------------------------------------
// HandleEvent — payload validation.
// -----------------------------------------------------------------------------

func TestHandleEvent_PayloadVersion1_LogsErrorReturnsNil(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:   1,
		ContactID: uuid.New(),
		Direction: "mutual",
	})
	// V1 payload → ERROR-log + nil return, no observation.
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, shadow.recordConsumerCalls, "V1 payload: no observation written")
}

func TestHandleEvent_PayloadV2NilSnapshot_LogsErrorReturnsNil(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		PrevCadenceSnapshot: nil,
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, shadow.recordConsumerCalls, "nil snapshot: no observation written")
}

func TestHandleEvent_PayloadUnmarshalError_ReturnsError(t *testing.T) {
	h, _, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := &events.Envelope{
		ID:         uuid.New(),
		Source:     "telegram",
		Kind:       events.KindInteractionRecorded,
		Payload:    json.RawMessage(`not valid json`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := h.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal")
}

// -----------------------------------------------------------------------------
// HandleEvent — direction branches. Each case fills in a V2 payload and
// asserts the shadow observation row matches the expected apply flags +
// computed next_* values.
// -----------------------------------------------------------------------------

func TestHandleEvent_Outbound_OnlyOutreachApplied(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prevOutreach := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	occurred := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "outbound",
		OccurredAt: occurred,
		Source:     "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastOutreachAt: &prevOutreach,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.NotNil(t, shadow.lastConsumerObs)
	obs := shadow.lastConsumerObs
	require.False(t, obs.ApplyLastContacted)
	require.True(t, obs.ApplyLastOutreachAt)
	require.False(t, obs.ApplyLastResponseAt)
	require.False(t, obs.ApplyContactBy, "outbound does not touch contact_by")
	require.Equal(t, repository.CadenceShadowBranchForward, obs.Branch)
	require.Nil(t, obs.NextLastContacted, "apply-flag-false column stays nil")
	require.NotNil(t, obs.NextLastOutreachAt)
	require.Equal(t, occurred, *obs.NextLastOutreachAt, "forward: incoming > prev → incoming wins")
}

func TestHandleEvent_Inbound_NoLastOutreachBumpMirrorsDirect(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prevContacted := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	occurred := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "inbound",
		OccurredAt: occurred,
		Source:     "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted: &prevContacted,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.NotNil(t, shadow.lastConsumerObs)
	obs := shadow.lastConsumerObs
	require.True(t, obs.ApplyLastContacted)
	require.False(t, obs.ApplyLastOutreachAt, "inbound mirrors direct: no last_outreach_at bump")
	require.True(t, obs.ApplyLastResponseAt)
	require.True(t, obs.ApplyContactBy, "cadence present, incoming later than prev → apply")
	require.Nil(t, obs.NextLastOutreachAt, "apply-false column stays nil")
}

func TestHandleEvent_Mutual_AllFourApplied(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		Source:              "gcal",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.True(t, obs.ApplyLastContacted)
	require.True(t, obs.ApplyLastOutreachAt)
	require.True(t, obs.ApplyLastResponseAt)
	require.True(t, obs.ApplyContactBy, "nil prev + cadence → apply contact_by")
}

// -----------------------------------------------------------------------------
// HandleEvent — forward-only guard.
// -----------------------------------------------------------------------------

func TestHandleEvent_ForwardOnly_IncomingOlder_NextEqualsPrev(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prev := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	incoming := prev.Add(-7 * 24 * time.Hour) // older
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "mutual",
		OccurredAt: incoming,
		Source:     "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted:  &prev,
			LastOutreachAt: &prev,
			LastResponseAt: &prev,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.Equal(t, repository.CadenceShadowBranchForward, obs.Branch)
	require.NotNil(t, obs.NextLastContacted)
	require.True(t, obs.NextLastContacted.Equal(prev), "forward: older incoming → prev wins")
	require.True(t, obs.NextLastOutreachAt.Equal(prev))
	require.True(t, obs.NextLastResponseAt.Equal(prev))
	// contact_by: prev.LastContacted > incoming.OccurredAt → applyContactBy=false.
	require.False(t, obs.ApplyContactBy, "older incoming: no contact_by advance")
	require.Nil(t, obs.NextContactBy)
}

func TestHandleEvent_ForwardOnly_IncomingNewer_NextEqualsIncoming(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prev := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	incoming := prev.Add(7 * 24 * time.Hour)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "mutual",
		OccurredAt: incoming,
		Source:     "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted:  &prev,
			LastOutreachAt: &prev,
			LastResponseAt: &prev,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.True(t, obs.NextLastContacted.Equal(incoming))
	require.True(t, obs.NextLastOutreachAt.Equal(incoming))
	require.True(t, obs.NextLastResponseAt.Equal(incoming))
}

func TestHandleEvent_NullPrev_IncomingWins(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	occurred := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		OccurredAt:          occurred,
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.True(t, obs.NextLastContacted.Equal(occurred))
	require.True(t, obs.NextLastOutreachAt.Equal(occurred))
	require.True(t, obs.NextLastResponseAt.Equal(occurred))
}

// -----------------------------------------------------------------------------
// HandleEvent — manual-source unconditional branch.
// -----------------------------------------------------------------------------

func TestHandleEvent_ManualSource_UnconditionalBranch(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prev := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	// Older incoming — manual should still replace unconditionally.
	incoming := prev.Add(-7 * 24 * time.Hour)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "mutual",
		OccurredAt: incoming,
		Source:     "manual",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted:  &prev,
			LastOutreachAt: &prev,
			LastResponseAt: &prev,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.Equal(t, repository.CadenceShadowBranchUnconditional, obs.Branch)
	require.True(t, obs.NextLastContacted.Equal(incoming), "manual: incoming wins even when older")
	require.True(t, obs.NextLastOutreachAt.Equal(incoming))
	require.True(t, obs.NextLastResponseAt.Equal(incoming))
	// manual always triggers apply_contact_by when cadence is set.
	require.True(t, obs.ApplyContactBy)
}

// -----------------------------------------------------------------------------
// HandleEvent — no cadence. PR 7 treats PrevCadenceValue=nil as "no cadence
// at emit time" and records the observation WITHOUT attempting to derive
// contact_by. Round-1 Codex fix: no live re-read fallback, which removes
// the cadence_edited_midstream race class at its source.
// -----------------------------------------------------------------------------

func TestHandleEvent_NoCadenceAtEmit_ContactByApplyFalse(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "inbound",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    nil, // no cadence at emit time
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, 1, shadow.recordConsumerCalls, "observation written even with no cadence")
	obs := shadow.lastConsumerObs
	require.False(t, obs.ApplyContactBy, "no cadence → apply_contact_by false")
	require.Nil(t, obs.NextContactBy)
	// Other columns still populated per direction rules.
	require.True(t, obs.ApplyLastContacted)
	require.True(t, obs.ApplyLastResponseAt)
	require.False(t, obs.ApplyLastOutreachAt)
}

// -----------------------------------------------------------------------------
// HandleEvent — unknown direction.
// -----------------------------------------------------------------------------

func TestHandleEvent_UnknownDirection_ApplyFlagsAllFalse(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "unknown-dir",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	// Envelope validates at ValidatePayload time only for the envelope
	// shape — consumer is the one enforcing direction CHECK behavior.
	// Unknown direction → no CHECK violation at this level; returns nil
	// observation via RecordConsumer (schema's direction CHECK will fire
	// at SQL time). In this test, stub repo doesn't enforce — assert the
	// apply flags are all false in the observation payload.
	//
	// NOTE: the integration test confirms the real DB rejects the insert
	// due to the CHECK constraint; this unit test just guards the in-
	// memory mapping.
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	obs := shadow.lastConsumerObs
	require.NotNil(t, obs)
	require.False(t, obs.ApplyLastContacted)
	require.False(t, obs.ApplyLastOutreachAt)
	require.False(t, obs.ApplyLastResponseAt)
	require.False(t, obs.ApplyContactBy)
}

// -----------------------------------------------------------------------------
// HandleEvent — inline divergence logger.
// -----------------------------------------------------------------------------

func TestHandleEvent_InlineDivergenceLogger_NoFireWhenMatch(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	prev := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	incoming := prev.Add(7 * 24 * time.Hour)
	// Pre-seed a matching direct observation.
	shadow.directObs = &repository.CadenceShadowObservation{
		EventID:            uuid.New(),
		Branch:             repository.CadenceShadowBranchForward,
		NextLastContacted:  &incoming,
		NextLastOutreachAt: &incoming,
		NextLastResponseAt: &incoming,
	}
	// Payload with cadence so consumer takes the happy path (no fallback).
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:    2,
		ContactID:  uuid.New(),
		Direction:  "mutual",
		OccurredAt: incoming,
		Source:     "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted:  &prev,
			LastOutreachAt: &prev,
			LastResponseAt: &prev,
		},
		PrevCadenceValue: strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, 1, shadow.findDirectCalls, "inline divergence lookup invoked on happy path")
}

func TestHandleEvent_InlineDivergenceLogger_NoFireWhenDirectMissing(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	// directObs == nil → normal condition (direct post-commit closure
	// hasn't fired yet). Consumer logs DEBUG and returns nil.
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, 1, shadow.findDirectCalls)
	// No test-logger adapter here; just assert no error returned.
}

func TestHandleEvent_InlineDivergenceLookupError_LoggedNotReturned(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	shadow.findDirectErr = errors.New("lookup failed")
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	// Lookup error must not fail the job — the consumer's observation is
	// already persisted; the inline logger is advisory.
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
}

// -----------------------------------------------------------------------------
// HandleEvent — RecordConsumer error surfaces (river retries).
// -----------------------------------------------------------------------------

func TestHandleEvent_RecordConsumerError_ReturnsError(t *testing.T) {
	h, shadow, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	shadow.recordConsumerErr = errors.New("insert failed")
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	err := h.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "record consumer")
}

// -----------------------------------------------------------------------------
// HandleEvent — nil guards.
// -----------------------------------------------------------------------------

func TestHandleEvent_NilEnvelope_Error(t *testing.T) {
	h, _, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	err := h.HandleEvent(context.Background(), nonNilTx(), nil)
	require.Error(t, err)
}

func TestHandleEvent_NilTx_Error(t *testing.T) {
	h, _, _ := newCadenceUpdaterWithStubs(CadenceModeShadow)
	env := mustInteractionRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           "mutual",
		Source:              "telegram",
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    strPtr("weekly"),
	})
	err := h.HandleEvent(context.Background(), nil, env)
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// CadenceModeFromConfig — narrows config.EventBusCadenceMode* into the
// consumer-side constants. Unknown defaults to shadow with a log.
// -----------------------------------------------------------------------------

func TestCadenceModeFromConfig(t *testing.T) {
	require.Equal(t, CadenceModeOff, CadenceModeFromConfig("off"))
	require.Equal(t, CadenceModeShadow, CadenceModeFromConfig("shadow"))
	require.Equal(t, CadenceModeCutover, CadenceModeFromConfig("cutover"))
	require.Equal(t, CadenceModeShadow, CadenceModeFromConfig("garbage"), "unknown falls back to shadow")
}
