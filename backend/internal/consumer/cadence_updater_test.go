package consumer

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Cadence_updater unit tests. Exercises applyTx, buildInteractionWrite,
// the branch matrix, mode gates, and claim dedupe without a live DB.
// The writes-to-contact side effects are covered by integration tests
// in backend/tests/.
// -----------------------------------------------------------------------------

// stubClaimer is the in-memory eventClaimer used by unit tests. Records
// calls and lets the test decide whether the claim succeeds.
type stubClaimer struct {
	calls         int
	lastEventID   uuid.UUID
	lastConsumer  string
	claimedResult bool
	returnErr     error
}

func (s *stubClaimer) TryClaimTx(_ context.Context, _ pgx.Tx, eventID uuid.UUID, consumer string) (bool, error) {
	s.calls++
	s.lastEventID = eventID
	s.lastConsumer = consumer
	if s.returnErr != nil {
		return false, s.returnErr
	}
	return s.claimedResult, nil
}

// stubContactReader returns a stored contact on GetContactTx; tests set
// the contact field directly to simulate the live row.
type stubContactReader struct {
	contact *repository.Contact
	err     error
}

func (s *stubContactReader) GetContactTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.Contact, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.contact, nil
}

// newUnitUpdater builds a CadenceUpdater with stub dependencies. queries
// is nil — applyTx would panic if actually called. Tests that exercise
// applyTx end-to-end live in the integration suite; these tests verify
// the pre-applyTx branching (mode gates, claim dedupe, build-request
// math) which never reaches the SQL layer.
func newUnitUpdater(mode string) (*CadenceUpdater, *stubClaimer, *stubContactReader) {
	claims := &stubClaimer{claimedResult: true}
	contacts := &stubContactReader{}
	h := NewCadenceUpdater(claims, contacts, nil, mode, false)
	return h, claims, contacts
}

// -----------------------------------------------------------------------------
// buildInteractionWrite — direction + manual branch matrix.
// Asserts apply flags, values, and branch selection without touching
// the DB. Exercises the full outbound/inbound/mutual × automated/manual
// matrix so a regression to direction rules surfaces at unit-test time.
// -----------------------------------------------------------------------------

func TestBuildInteractionWrite_Outbound_WritesOnlyLastOutreachAt(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occ := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		occ,
		repository.ContactCadenceFields{},
		"weekly",
	)

	require.Equal(t, repository.CadenceShadowBranchForward, req.Branch)
	require.False(t, req.ApplyLastContacted)
	require.False(t, req.ApplyLastInteractionAt, "outbound never touches last_interaction_at")
	require.True(t, req.ApplyLastOutreachAt)
	require.False(t, req.ApplyLastResponseAt)
	require.False(t, req.ApplyContactBy, "outbound never touches contact_by")
	require.NotNil(t, req.LastOutreachAt)
	require.True(t, req.LastOutreachAt.Equal(occ))
}

func TestBuildInteractionWrite_Inbound_WritesThreeFields(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occ := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionInbound,
		repository.InteractionSourceTelegram,
		occ,
		repository.ContactCadenceFields{},
		"weekly",
	)

	require.Equal(t, repository.CadenceShadowBranchForward, req.Branch)
	require.True(t, req.ApplyLastContacted)
	require.True(t, req.ApplyLastInteractionAt, "inbound bumps last_interaction_at alongside last_contacted")
	require.False(t, req.ApplyLastOutreachAt, "inbound does NOT bump last_outreach_at")
	require.True(t, req.ApplyLastResponseAt)
	require.True(t, req.ApplyContactBy)
	require.NotNil(t, req.ContactBy)
	require.NotNil(t, req.LastInteractionAt)
	require.True(t, req.LastInteractionAt.Equal(occ))
}

func TestBuildInteractionWrite_Mutual_WritesAllFour(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occ := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionMutual,
		repository.InteractionSourceTelegram,
		occ,
		repository.ContactCadenceFields{},
		"weekly",
	)

	require.Equal(t, repository.CadenceShadowBranchForward, req.Branch)
	require.True(t, req.ApplyLastContacted)
	require.True(t, req.ApplyLastInteractionAt, "mutual bumps last_interaction_at alongside last_contacted")
	require.True(t, req.ApplyLastOutreachAt)
	require.True(t, req.ApplyLastResponseAt)
	require.True(t, req.ApplyContactBy)
}

func TestBuildInteractionWrite_Manual_UsesUnconditionalBranch(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occ := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionMutual,
		repository.InteractionSourceManual,
		occ,
		repository.ContactCadenceFields{},
		"weekly",
	)

	require.Equal(t, repository.CadenceShadowBranchUnconditional, req.Branch,
		"manual-source interactions take the unconditional branch (spec §3.4.2)")
	require.True(t, req.ApplyContactBy)
}

func TestBuildInteractionWrite_NoCadence_SkipsContactBy(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occ := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionMutual,
		repository.InteractionSourceTelegram,
		occ,
		repository.ContactCadenceFields{},
		"",
	)

	require.True(t, req.ApplyLastContacted)
	require.False(t, req.ApplyContactBy, "contact_by gate requires cadence to be set")
	require.Nil(t, req.ContactBy)
}

func TestBuildInteractionWrite_ForwardGate_OlderIncoming(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	tPrev := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	tOlder := tPrev.Add(-7 * 24 * time.Hour)

	req := h.buildInteractionWrite(
		uuid.New(),
		repository.InteractionDirectionMutual,
		repository.InteractionSourceTelegram,
		tOlder,
		repository.ContactCadenceFields{LastContacted: &tPrev},
		"weekly",
	)

	require.Equal(t, repository.CadenceShadowBranchForward, req.Branch)
	// Apply flags stay true — the SQL's forward guard is what refuses the
	// regression. ApplyContactBy specifically falls to false because the
	// time-gate is violated.
	require.True(t, req.ApplyLastContacted)
	require.False(t, req.ApplyContactBy,
		"older incoming against existing prev must not recompute contact_by")
}

// -----------------------------------------------------------------------------
// HandleEvent — mode gate, payload validation, claim dedupe.
// -----------------------------------------------------------------------------

func TestHandleEvent_ModeOff_NoClaimNoWrite(t *testing.T) {
	h, claims, _ := newUnitUpdater(CadenceModeOff)
	env := mustRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, claims.calls, "off-mode must not attempt a claim")
}

func TestHandleEvent_V1Payload_Rejected(t *testing.T) {
	h, claims, _ := newUnitUpdater(CadenceModeCutover)
	env := mustRecordedEnv(t, events.InteractionRecordedPayload{
		Version:   1,
		ContactID: uuid.New(),
		Direction: repository.InteractionDirectionMutual,
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env),
		"V1 payload must be rejected cleanly without error (no river retry)")
	require.Zero(t, claims.calls, "V1 rejection fires before claim attempt")
}

func TestHandleEvent_V2MissingSnapshot_Rejected(t *testing.T) {
	h, claims, _ := newUnitUpdater(CadenceModeCutover)
	env := mustRecordedEnv(t, events.InteractionRecordedPayload{
		Version:   2,
		ContactID: uuid.New(),
		Direction: repository.InteractionDirectionMutual,
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, claims.calls)
}

func TestHandleEvent_AlreadyClaimed_NoOp(t *testing.T) {
	h, claims, _ := newUnitUpdater(CadenceModeCutover)
	claims.claimedResult = false

	env := mustRecordedEnv(t, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, 1, claims.calls)
	require.Equal(t, env.ID, claims.lastEventID)
	require.Equal(t, repository.EventConsumerCadenceUpdater, claims.lastConsumer)
}

// -----------------------------------------------------------------------------
// Mode=off guards on the direct-invoke APIs.
// -----------------------------------------------------------------------------

func TestBulkApply_ModeOff_NoOp(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeOff)
	require.NoError(t, h.BulkApply(context.Background(), nonNilTx(), uuid.New(), repository.ContactCadenceFields{}))
}

func TestApplyContactByOverride_ModeOff_NoOp(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeOff)
	require.NoError(t, h.ApplyContactByOverride(context.Background(), nonNilTx(), uuid.New(), nil))
}

// -----------------------------------------------------------------------------
// Helpers.
// -----------------------------------------------------------------------------

func mustRecordedEnv(t *testing.T, payload events.InteractionRecordedPayload) *events.Envelope {
	t.Helper()
	if payload.OccurredAt.IsZero() {
		payload.OccurredAt = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	}
	if payload.Source == "" {
		payload.Source = repository.InteractionSourceTelegram
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
