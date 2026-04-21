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
// key helper without a live DB. The Record* helpers funnel through the
// shadow writer stub so tests inspect exactly what would be persisted.
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

// stubFollowUpShadowWriter captures what the consumer would persist.
type stubFollowUpShadowWriter struct {
	records []repository.FollowUpShadowObservation
	err     error
}

func (s *stubFollowUpShadowWriter) RecordConsumer(_ context.Context, _ pgx.Tx, obs repository.FollowUpShadowObservation) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, obs)
	return nil
}

func (s *stubFollowUpShadowWriter) FindMatchingDirect(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.FollowUpShadowObservation, error) {
	return nil, nil
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

func newUnitFollowUp(mode string) (*FollowUpManager, *stubFollowUpTaskReader, *stubInteractionResponseReader, *stubFollowUpShadowWriter) {
	tasks := &stubFollowUpTaskReader{err: db.ErrNotFound}
	inters := &stubInteractionResponseReader{}
	shadow := &stubFollowUpShadowWriter{}
	h := NewFollowUpManager(mode, tasks, inters, shadow, testWatchdog())
	return h, tasks, inters, shadow
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
	h, tasks, inters, shadow := newUnitFollowUp(FollowUpModeOff)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls, "mode=off must not query task repo")
	require.Equal(t, 0, inters.calls, "mode=off must not query interaction repo")
	require.Empty(t, shadow.records, "mode=off must not write shadow rows")
}

func TestFollowUpManager_ModeCutover_ReturnsError(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.Error(t, err, "cutover mode must fail loudly while cutover code is absent")
	require.Empty(t, shadow.records)
}

func TestFollowUpManager_NilEnvelope(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeShadow)
	err := h.HandleEvent(context.Background(), nonNilFakeTx(), nil)
	require.Error(t, err)
}

func TestFollowUpManager_NilTx(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeShadow)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")
	err := h.HandleEvent(context.Background(), nil, env)
	require.Error(t, err)
}

func TestFollowUpManager_V1PayloadRejected(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
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

	err = h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err, "V1 payload is logged but not returned as error")
	require.Empty(t, shadow.records)
}

// -----------------------------------------------------------------------------
// Direction dispatch.
// -----------------------------------------------------------------------------

func TestFollowUpManager_UnknownDirection_Skips(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	env := buildRecordedEnv(t, uuid.New(), "bogus", repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Empty(t, shadow.records)
}

// -----------------------------------------------------------------------------
// Outbound + guard branches.
// -----------------------------------------------------------------------------

func TestFollowUpManager_Outbound_NoCadence_SkipsWithoutReason(t *testing.T) {
	h, tasks, inters, shadow := newUnitFollowUp(FollowUpModeShadow)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls, "no-cadence skip must not run guard 3")
	require.Equal(t, 0, inters.calls, "no-cadence skip must not run guard 2")
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionSkip, shadow.records[0].Action)
	require.Empty(t, shadow.records[0].SkipReason, "no-cadence skip has empty skip_reason")
}

func TestFollowUpManager_Outbound_Backdated_NonManual_SkipsBackdated(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	// 90 days older than now, with weekly cadence (3-day watchdog): well
	// past the backdated cutoff.
	occurred := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionSkip, shadow.records[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonBackdated, shadow.records[0].SkipReason)
}

func TestFollowUpManager_Outbound_Backdated_Manual_BypassesGuard1(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	occurred := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceManual, occurred, "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionCreate, shadow.records[0].Action,
		"manual source must bypass the backdated guard and proceed to create")
}

func TestFollowUpManager_Outbound_OutOfOrder_Skips(t *testing.T) {
	h, _, inters, shadow := newUnitFollowUp(FollowUpModeShadow)
	inters.hasResp = true
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, inters.calls)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionSkip, shadow.records[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonOutOfOrder, shadow.records[0].SkipReason)
}

func TestFollowUpManager_Outbound_HasResponseErr_Propagates(t *testing.T) {
	h, _, inters, _ := newUnitFollowUp(FollowUpModeShadow)
	inters.err = errors.New("boom")
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.Error(t, err)
}

func TestFollowUpManager_Outbound_DuplicatePending_RefreshNotSkip(t *testing.T) {
	h, tasks, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	existing := uuid.New()
	tasks.pending = &repository.ContactTask{ID: existing, Kind: "follow_up", State: repository.ContactTaskStateManaged}
	tasks.err = nil
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionRefresh, shadow.records[0].Action,
		"duplicate-pending outbound records refresh, not skip — mirrors direct-path behavior")
	require.Empty(t, shadow.records[0].SkipReason)
	require.NotNil(t, shadow.records[0].WouldDeadline)
}

func TestFollowUpManager_Outbound_Fresh_RecordsCreate(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	obs := shadow.records[0]
	require.Equal(t, repository.FollowUpActionCreate, obs.Action)
	require.NotNil(t, obs.WouldIdempotencyKey)
	require.NotEmpty(t, *obs.WouldIdempotencyKey)
	require.NotNil(t, obs.WouldDeadline)
	require.False(t, obs.ConsumerCalledTodoist)
}

// -----------------------------------------------------------------------------
// Inbound / mutual dispatch.
// -----------------------------------------------------------------------------

func TestFollowUpManager_Inbound_HasPending_RecordsComplete(t *testing.T) {
	h, tasks, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	tasks.pending = &repository.ContactTask{ID: uuid.New(), Kind: "follow_up", State: repository.ContactTaskStateManaged}
	tasks.err = nil
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionInbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionComplete, shadow.records[0].Action)
}

func TestFollowUpManager_Mutual_NoPending_RecordsSkipNoReason(t *testing.T) {
	h, _, _, shadow := newUnitFollowUp(FollowUpModeShadow)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionMutual, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, shadow.records, 1)
	require.Equal(t, repository.FollowUpActionSkip, shadow.records[0].Action)
	require.Empty(t, shadow.records[0].SkipReason, "inbound/mutual no-pending skip is not guard-class")
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
	require.Equal(t, FollowUpModeShadow, FollowUpModeFromConfig(config.EventBusFollowUpModeShadow))
	require.Equal(t, FollowUpModeCutover, FollowUpModeFromConfig(config.EventBusFollowUpModeCutover))
	require.Equal(t, FollowUpModeOff, FollowUpModeFromConfig("garbage"), "unknown mode defaults to off")
}
