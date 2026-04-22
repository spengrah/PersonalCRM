package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// FollowUpManager unit tests. Exercises direction dispatch, three
// guards, mode gates, payload-version rejection, and the idempotency-
// key helper without a live DB. The DecisionObserver hook captures the
// manager's terminal decision for each branch so tests assert on the
// full classification (action, skip reason, would-be deadline,
// idempotency key) without a DB round trip.
// -----------------------------------------------------------------------------

// stubFollowUpTaskReader records which contact FindPendingFollowUpTx was
// called for and returns the configured pending / err pair.
type stubFollowUpTaskReader struct {
	pending *repository.ContactTask
	err     error
	calls   int
}

func (s *stubFollowUpTaskReader) FindPendingFollowUpTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.ContactTask, error) {
	s.calls++
	return s.pending, s.err
}

// stubInteractionResponseReader reports whether a later response exists.
type stubInteractionResponseReader struct {
	hasResp bool
	err     error
	calls   int
}

func (s *stubInteractionResponseReader) HasResponseAfterTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ time.Time) (bool, error) {
	s.calls++
	return s.hasResp, s.err
}

// stubEventClaimer unconditionally grants the claim. Production claim
// behavior is covered by integration tests — unit tests here care only
// that the manager reaches the decision branch.
type stubEventClaimer struct {
	calls int
}

func (s *stubEventClaimer) TryClaimTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ string) (bool, error) {
	s.calls++
	return true, nil
}

// stubFollowUpContactReader returns a test-fixture contact with an
// optional cadence string.
type stubFollowUpContactReader struct {
	cadence *string
	err     error
}

func (s *stubFollowUpContactReader) GetContactTx(_ context.Context, _ pgx.Tx, id uuid.UUID) (*repository.Contact, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &repository.Contact{ID: id, FullName: "Test Contact", Cadence: s.cadence}, nil
}

// fakeTx is a typed-nil pgx.Tx sufficient for branching that never
// dereferences the value.
type fakeTx struct{ pgx.Tx }

func nonNilFakeTx() pgx.Tx {
	var t *fakeTx
	return t
}

func testWatchdog() config.WatchdogConfig {
	return config.WatchdogConfig{
		WeeklyDays:    3,
		BiweeklyDays:  5,
		MonthlyDays:   7,
		QuarterlyDays: 14,
		BiannualDays:  21,
		AnnualDays:    21,
	}
}

// newUnitFollowUp builds a cutover-mode manager with a stub claimer
// (always grants) and an installed DecisionObserver. Returns the
// observed-decisions slice pointer so tests assert on classifications.
// Cutover-only deps that aren't reached by skip-branch tests (taskWriter,
// riverInserter, pool, settings, clientFactory) are nil; tests that
// reach those paths use newCutoverFollowUp below.
func newUnitFollowUp(mode string) (*FollowUpManager, *stubFollowUpTaskReader, *stubInteractionResponseReader, *[]Decision) {
	tasks := &stubFollowUpTaskReader{err: db.ErrNotFound}
	inters := &stubInteractionResponseReader{}
	claims := &stubEventClaimer{}
	contacts := &stubFollowUpContactReader{}
	var observed []Decision
	h := NewFollowUpManager(mode, claims, contacts, tasks, nil, inters, nil, nil, nil, nil, "", testWatchdog())
	h.SetDecisionObserver(func(d Decision) { observed = append(observed, d) })
	return h, tasks, inters, &observed
}

// buildRecordedEnv constructs a V2 interaction.recorded envelope for
// the given (contact, direction, occurred_at, source) tuple. cadence
// may be empty to test the no-cadence skip path.
func buildRecordedEnv(t *testing.T, contactID uuid.UUID, direction, source string, occurredAt time.Time, cadenceStr string) *events.Envelope {
	t.Helper()
	payload := events.InteractionRecordedPayload{
		Version:       2,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     direction,
		OccurredAt:    occurredAt,
		Source:        source,
	}
	if cadenceStr != "" {
		payload.PrevCadenceValue = &cadenceStr
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindInteractionRecorded,
		Source:     source,
		SourceID:   "",
		Payload:    raw,
		ObservedAt: occurredAt,
	}
}

// -----------------------------------------------------------------------------
// Mode gate + payload-version tests.
// -----------------------------------------------------------------------------

func TestFollowUpManager_ModeOff_NoWrites(t *testing.T) {
	h, tasks, inters, observed := newUnitFollowUp(FollowUpModeOff)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls, "mode=off must not query task repo")
	require.Equal(t, 0, inters.calls, "mode=off must not query interaction repo")
	require.Empty(t, *observed, "mode=off must not emit decisions")
}

func TestFollowUpManager_ModeCutover_RequiresClaimRepo(t *testing.T) {
	// Cutover without a wired claim repository is a programming error —
	// the manager errors out at the claim step so a misconfigured
	// deployment doesn't silently skip dedupe.
	tasks := &stubFollowUpTaskReader{err: db.ErrNotFound}
	inters := &stubInteractionResponseReader{}
	h := NewFollowUpManager(FollowUpModeCutover, nil, nil, tasks, nil, inters, nil, nil, nil, nil, "", testWatchdog())

	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")
	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "claim")
}

func TestFollowUpManager_NilEnvelope(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeCutover)
	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), nil)
	require.Error(t, err)
}

func TestFollowUpManager_NilTx(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")
	_, err := h.HandleEvent(context.Background(), nil, env)
	require.Error(t, err)
}

func TestFollowUpManager_V1PayloadRejected(t *testing.T) {
	h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
	payload := events.InteractionRecordedPayload{
		Version:       1,
		ContactID:     uuid.New(),
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionOutbound,
		OccurredAt:    accelerated.GetCurrentTime(),
		Source:        repository.InteractionSourceTelegram,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindInteractionRecorded,
		Source:     repository.InteractionSourceTelegram,
		Payload:    raw,
		ObservedAt: accelerated.GetCurrentTime(),
	}

	_, err = h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err, "V1 payload is logged but not returned as error")
	require.Empty(t, *observed)
}

// -----------------------------------------------------------------------------
// Direction dispatch.
// -----------------------------------------------------------------------------

func TestFollowUpManager_UnknownDirection_Skips(t *testing.T) {
	h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), "bogus", repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Empty(t, *observed)
}

// -----------------------------------------------------------------------------
// Outbound + guard branches. These tests exercise the skip-path
// classification only (guards 1, 2, 0), so they do not reach the
// Todoist-settings / taskWriter path — a simpler manager with nil
// writer/settings suffices.
// -----------------------------------------------------------------------------

func TestFollowUpManager_Outbound_NoCadence_SkipsWithoutReason(t *testing.T) {
	h, tasks, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls, "no-cadence skip must not run guard 3")
	require.Equal(t, 0, inters.calls, "no-cadence skip must not run guard 2")
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Empty(t, (*observed)[0].SkipReason, "no-cadence skip has empty skip_reason")
}

func TestFollowUpManager_Outbound_Backdated_NonManual_SkipsBackdated(t *testing.T) {
	h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
	// 90 days older than now, with weekly cadence (3-day watchdog): well
	// past the backdated cutoff.
	occurred := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonBackdated, (*observed)[0].SkipReason)
}

func TestFollowUpManager_Outbound_OutOfOrder_Skips(t *testing.T) {
	h, _, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	inters.hasResp = true
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, inters.calls)
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonOutOfOrder, (*observed)[0].SkipReason)
}

func TestFollowUpManager_Outbound_HasResponseErr_Propagates(t *testing.T) {
	h, _, inters, _ := newUnitFollowUp(FollowUpModeCutover)
	inters.err = errors.New("boom")
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// Inbound / mutual dispatch. Only the no-pending skip path is reachable
// without a taskWriter; the complete-with-pending path requires the
// full integration-test harness (UpdateContactTaskStateTx + river
// inserter). Integration tests in backend/tests cover that branch.
// -----------------------------------------------------------------------------

func TestFollowUpManager_Mutual_NoPending_RecordsSkipNoReason(t *testing.T) {
	h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionMutual, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	_, err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Empty(t, (*observed)[0].SkipReason, "inbound/mutual no-pending skip is not guard-class")
}

// -----------------------------------------------------------------------------
// Idempotency key helper.
// -----------------------------------------------------------------------------

func TestBuildFollowUpIdempotencyKey_Stability(t *testing.T) {
	cid := uuid.New()
	when := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	k1 := buildFollowUpIdempotencyKey(cid, when)
	k2 := buildFollowUpIdempotencyKey(cid, when)
	require.Equal(t, k1, k2, "same inputs must produce identical key")
	require.Len(t, k1, 64, "sha256 hex digest is 64 chars")
}

func TestBuildFollowUpIdempotencyKey_Distinctness(t *testing.T) {
	when := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	c1 := uuid.New()
	c2 := uuid.New()
	require.NotEqual(t, buildFollowUpIdempotencyKey(c1, when), buildFollowUpIdempotencyKey(c2, when),
		"different contacts must produce different keys")
	require.NotEqual(t,
		buildFollowUpIdempotencyKey(c1, when),
		buildFollowUpIdempotencyKey(c1, when.Add(time.Second)),
		"different timestamps must produce different keys")
}

// -----------------------------------------------------------------------------
// watchdogDaysForCadenceStr helper.
// -----------------------------------------------------------------------------

func TestWatchdogDaysForCadenceStr(t *testing.T) {
	cfg := testWatchdog()
	cases := []struct {
		cadence string
		want    int
	}{
		{"weekly", 3},
		{"biweekly", 5},
		{"monthly", 7},
		{"quarterly", 14},
		{"biannual", 21},
		{"annual", 21},
		{"", 0},
		{"gibberish", 0},
	}
	for _, tc := range cases {
		t.Run(tc.cadence, func(t *testing.T) {
			require.Equal(t, tc.want, watchdogDaysForCadenceStr(tc.cadence, cfg))
		})
	}
}

// -----------------------------------------------------------------------------
// FollowUpModeFromConfig mapping.
// -----------------------------------------------------------------------------

func TestFollowUpModeFromConfig(t *testing.T) {
	require.Equal(t, FollowUpModeOff, FollowUpModeFromConfig(config.EventBusFollowUpModeOff))
	require.Equal(t, FollowUpModeCutover, FollowUpModeFromConfig(config.EventBusFollowUpModeCutover))
	require.Equal(t, FollowUpModeOff, FollowUpModeFromConfig("garbage"), "unknown mode defaults to off")
}
