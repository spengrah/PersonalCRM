package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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

// newUnitFollowUp builds a cutover-mode manager with all required
// dependencies wired to minimal stubs. `validateCutoverDeps` now
// refuses to run HandleEvent with any nil collaborator, so tests that
// exercise skip branches still need a complete dependency set — even
// though the skip paths never reach the taskWriter / settings
// branches. Returns the observed-decisions slice pointer so tests
// assert on classifications.
func newUnitFollowUp(mode string) (*FollowUpManager, *stubFollowUpTaskReader, *stubInteractionResponseReader, *[]Decision) {
	tasks := &stubFollowUpTaskReader{err: db.ErrNotFound}
	inters := &stubInteractionResponseReader{}
	claims := &stubEventClaimer{}
	contacts := &stubFollowUpContactReader{}
	writer := &stubFollowUpTaskWriter{}
	inserter := &stubRiverInserter{}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "p", LabelName: "l", IntegrationInstanceID: "i"}, "token", nil
	}
	var observed []Decision
	h := NewFollowUpManager(mode, claims, contacts, tasks, writer, inters, inserter, settings, "http://localhost", testWatchdog())
	h.SetDecisionObserver(func(d Decision) { observed = append(observed, d) })
	return h, tasks, inters, &observed
}

// buildRecordedEnv constructs a V2 interaction.recorded envelope for
// the given (contact, direction, occurred_at, source) tuple. cadence
// may be empty to test the no-cadence skip path.
func buildRecordedEnv(t *testing.T, contactID uuid.UUID, direction, source string, occurredAt time.Time, cadenceStr string) *events.Envelope {
	t.Helper()
	return buildRecordedEnvVersion(t, 2, contactID, direction, source, occurredAt, cadenceStr, false)
}

// buildRecordedEnvVersion constructs an interaction.recorded envelope at
// an explicit payload version with optional SuppressFollowUp. Used by
// the V3 / SuppressFollowUp tests; V2 callers go through buildRecordedEnv.
func buildRecordedEnvVersion(
	t *testing.T,
	version int,
	contactID uuid.UUID,
	direction, source string,
	occurredAt time.Time,
	cadenceStr string,
	suppressFollowUp bool,
) *events.Envelope {
	t.Helper()
	payload := events.InteractionRecordedPayload{
		Version:          version,
		ContactID:        contactID,
		InteractionID:    uuid.New(),
		Direction:        direction,
		OccurredAt:       occurredAt,
		Source:           source,
		SuppressFollowUp: suppressFollowUp,
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

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls, "mode=off must not query task repo")
	require.Equal(t, 0, inters.calls, "mode=off must not query interaction repo")
	require.Empty(t, *observed, "mode=off must not emit decisions")
}

func TestFollowUpManager_ModeCutover_RequiresCollaborators(t *testing.T) {
	// Cutover without required collaborators is a programming error.
	// `validateCutoverDeps` must fail closed with a descriptive error
	// rather than panicking deep in a create / refresh / complete branch.
	// The first nil dependency reported determines the error message —
	// we cover each required dep's check in sequence by wiring all of
	// them to nil except the ones we've already validated.
	// Helper captures the full dependency set so each sub-test can null
	// out a single dep while leaving the rest populated. Returning a
	// builder closure rather than a manager keeps each sub-test's null
	// substitution local.
	fullDeps := func() (eventClaimer, followUpContactReader, contactTaskReader, followUpTaskWriter, interactionResponseReader, RiverInserter, TodoistSettingsFunc) {
		return &stubEventClaimer{}, &stubFollowUpContactReader{}, &stubFollowUpTaskReader{}, &stubFollowUpTaskWriter{}, &stubInteractionResponseReader{}, &stubRiverInserter{}, settingsOKStub()
	}
	cases := []struct {
		name     string
		build    func() *FollowUpManager
		contains string
	}{
		{
			name: "nil_claims",
			build: func() *FollowUpManager {
				_, contacts, taskRepo, taskWriter, inters, inserter, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, nil, contacts, taskRepo, taskWriter, inters, inserter, settings, "", testWatchdog())
			},
			contains: "claim repository",
		},
		{
			name: "nil_contacts",
			build: func() *FollowUpManager {
				claims, _, taskRepo, taskWriter, inters, inserter, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, nil, taskRepo, taskWriter, inters, inserter, settings, "", testWatchdog())
			},
			contains: "contact reader",
		},
		{
			name: "nil_task_repo",
			build: func() *FollowUpManager {
				claims, contacts, _, taskWriter, inters, inserter, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, contacts, nil, taskWriter, inters, inserter, settings, "", testWatchdog())
			},
			contains: "contact-task reader",
		},
		{
			name: "nil_task_writer",
			build: func() *FollowUpManager {
				claims, contacts, taskRepo, _, inters, inserter, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, contacts, taskRepo, nil, inters, inserter, settings, "", testWatchdog())
			},
			contains: "contact-task writer",
		},
		{
			name: "nil_interaction_repo",
			build: func() *FollowUpManager {
				claims, contacts, taskRepo, taskWriter, _, inserter, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, contacts, taskRepo, taskWriter, nil, inserter, settings, "", testWatchdog())
			},
			contains: "interaction reader",
		},
		{
			name: "nil_river_inserter",
			build: func() *FollowUpManager {
				claims, contacts, taskRepo, taskWriter, inters, _, settings := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, contacts, taskRepo, taskWriter, inters, nil, settings, "", testWatchdog())
			},
			contains: "river inserter",
		},
		{
			name: "nil_settings",
			build: func() *FollowUpManager {
				claims, contacts, taskRepo, taskWriter, inters, inserter, _ := fullDeps()
				return NewFollowUpManager(FollowUpModeCutover, claims, contacts, taskRepo, taskWriter, inters, inserter, nil, "", testWatchdog())
			},
			contains: "todoist settings",
		},
	}
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.build()
			err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.contains)
		})
	}
}

func settingsOKStub() TodoistSettingsFunc {
	return func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "p", LabelName: "l", IntegrationInstanceID: "i"}, "token", nil
	}
}

func TestFollowUpManager_NilEnvelope(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeCutover)
	err := h.HandleEvent(context.Background(), nonNilFakeTx(), nil)
	require.Error(t, err)
}

func TestFollowUpManager_NilTx(t *testing.T) {
	h, _, _, _ := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")
	err := h.HandleEvent(context.Background(), nil, env)
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

	err = h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err, "V1 payload is logged but not returned as error")
	require.Empty(t, *observed)
}

// TestFollowUpManager_AcceptsV2AndV3 confirms both V2 and V3 payloads
// dispatch past the version gate. V3 carries SuppressFollowUp; V2
// retains backward compatibility for in-flight events written by an
// older binary. We use the no-cadence skip branch (empty cadence
// string) because it's the earliest skip beyond the SuppressFollowUp
// short-circuit and the version gate, so the observed decision proves
// the dispatcher accepted the version.
func TestFollowUpManager_AcceptsV2AndV3(t *testing.T) {
	for _, version := range []int{2, 3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
			env := buildRecordedEnvVersion(t, version, uuid.New(),
				repository.InteractionDirectionOutbound,
				repository.InteractionSourceTelegram,
				accelerated.GetCurrentTime(),
				"" /* no cadence -> skips at guard 0 */, false)

			err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
			require.NoError(t, err)
			require.Len(t, *observed, 1,
				"V%d payload must produce exactly one decision", version)
			require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action,
				"V%d payload must dispatch past the version gate (no-cadence skip)", version)
			// Crucially: the SuppressFollowUp skip-reason MUST NOT fire
			// for these payloads — SuppressFollowUp=false on both.
			require.Empty(t, (*observed)[0].SkipReason,
				"V%d no-cadence skip must NOT carry the suppressed reason", version)
		})
	}
}

// TestFollowUpManager_SuppressFollowUp_TrueEarlyReturns asserts the
// load-bearing SuppressFollowUp polarity for the kind=send path. When
// true, the manager must short-circuit BEFORE the cadence/backdate/
// out-of-order/has-response guards fire and emit a skip decision with
// the suppressed reason. Polarity is "zero=do-not-suppress" so a V1/V2
// payload (where the field decodes to false) still spawns follow-ups.
func TestFollowUpManager_SuppressFollowUp_TrueEarlyReturns(t *testing.T) {
	h, tasks, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnvVersion(t, 3, uuid.New(),
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTodoist,
		accelerated.GetCurrentTime(), "weekly", true /* suppress */)

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 0, tasks.calls,
		"SuppressFollowUp=true must short-circuit before task lookup")
	require.Equal(t, 0, inters.calls,
		"SuppressFollowUp=true must short-circuit before interaction lookup")
	require.Len(t, *observed, 1, "must emit exactly one skip decision")
	d := (*observed)[0]
	require.Equal(t, repository.FollowUpActionSkip, d.Action)
	require.Equal(t, repository.FollowUpSkipReasonSuppressed, d.SkipReason)
}

// TestFollowUpManager_SuppressFollowUp_FalseDispatches confirms the
// inverse: SuppressFollowUp=false (the polarity-safe default) does NOT
// short-circuit on the suppressed reason. We use the out-of-order skip
// branch (a later inbound response exists) to land in a known
// post-suppression decision without exercising the create path.
func TestFollowUpManager_SuppressFollowUp_FalseDispatches(t *testing.T) {
	h, _, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	inters.hasResp = true // out-of-order skip branch
	env := buildRecordedEnvVersion(t, 3, uuid.New(),
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTodoist,
		accelerated.GetCurrentTime(), "weekly", false /* do not suppress */)

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, inters.calls,
		"SuppressFollowUp=false must reach the out-of-order guard")
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonOutOfOrder, (*observed)[0].SkipReason,
		"skip reason must be out-of-order, not suppressed, when SuppressFollowUp=false")
}

// TestFollowUpManager_SuppressFollowUp_TelegramPathIgnored confirms
// SuppressFollowUp is task.completed-only — telegram outbound payloads
// (where the field is always zero) still progress past the suppress
// gate so a telegram outbound message followed by no inbound reply
// still spawns a follow-up.
func TestFollowUpManager_SuppressFollowUp_TelegramPathIgnored(t *testing.T) {
	h, _, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	inters.hasResp = true // land in a post-suppress skip branch
	env := buildRecordedEnvVersion(t, 3, uuid.New(),
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime(), "weekly", false)

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, *observed, 1)
	require.NotEqual(t, repository.FollowUpSkipReasonSuppressed, (*observed)[0].SkipReason,
		"telegram path must never carry suppressed skip reason")
}

// -----------------------------------------------------------------------------
// Direction dispatch.
// -----------------------------------------------------------------------------

func TestFollowUpManager_UnknownDirection_Skips(t *testing.T) {
	h, _, _, observed := newUnitFollowUp(FollowUpModeCutover)
	env := buildRecordedEnv(t, uuid.New(), "bogus", repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
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

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
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

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)
	require.Len(t, *observed, 1)
	require.Equal(t, repository.FollowUpActionSkip, (*observed)[0].Action)
	require.Equal(t, repository.FollowUpSkipReasonBackdated, (*observed)[0].SkipReason)
}

func TestFollowUpManager_Outbound_OutOfOrder_Skips(t *testing.T) {
	h, _, inters, observed := newUnitFollowUp(FollowUpModeCutover)
	inters.hasResp = true
	env := buildRecordedEnv(t, uuid.New(), repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), "weekly")

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
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

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
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

	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
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

// -----------------------------------------------------------------------------
// Refresh-path coverage. The refresh branch advances the local due_date
// and enqueues an update_deadline op in the same tx (all remote effects
// leave via op jobs — no post-commit Todoist call). It emits exactly one
// Decision at the in-tx decision point.
// -----------------------------------------------------------------------------

// stubFollowUpTaskWriter records metadata updates and returns a
// ContactTask from the configured pending-lookup. Other methods panic
// to catch unintended code-path activation.
type stubFollowUpTaskWriter struct {
	lastMetadataID       uuid.UUID
	lastMetadata         map[string]any
	updatedTask          *repository.ContactTask
	metadataErr          error
	stateTransitionCalls int
}

func (s *stubFollowUpTaskWriter) CreateContactTaskTx(context.Context, pgx.Tx, repository.CreateContactTaskRequest, *string) (*repository.ContactTask, error) {
	panic("stubFollowUpTaskWriter.CreateContactTaskTx: not exercised in refresh-path tests")
}
func (s *stubFollowUpTaskWriter) UpdateContactTaskStateTx(context.Context, pgx.Tx, uuid.UUID, repository.ContactTaskState) (*repository.ContactTask, error) {
	s.stateTransitionCalls++
	return s.updatedTask, nil
}
func (s *stubFollowUpTaskWriter) UpdateContactTaskMetadataTx(_ context.Context, _ pgx.Tx, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error) {
	s.lastMetadataID = id
	s.lastMetadata = metadata
	if s.metadataErr != nil {
		return nil, s.metadataErr
	}
	return s.updatedTask, nil
}
func (s *stubFollowUpTaskWriter) UpdateContactTaskExternalIDTx(context.Context, pgx.Tx, uuid.UUID, string) (*repository.ContactTask, error) {
	panic("stubFollowUpTaskWriter.UpdateContactTaskExternalIDTx: not exercised in refresh-path tests")
}
func (s *stubFollowUpTaskWriter) SetContactTaskExternalIDOnlyTx(context.Context, pgx.Tx, uuid.UUID, string) error {
	panic("stubFollowUpTaskWriter.SetContactTaskExternalIDOnlyTx: not exercised in refresh-path tests")
}
func (s *stubFollowUpTaskWriter) GetContactTaskByIdempotencyKeyTx(context.Context, pgx.Tx, uuid.UUID, string) (*repository.ContactTask, error) {
	return nil, db.ErrNotFound
}
func (s *stubFollowUpTaskWriter) GetContactTaskTx(context.Context, pgx.Tx, uuid.UUID) (*repository.ContactTask, error) {
	return s.updatedTask, nil
}

// TestFollowUpManager_Refresh_UpdatesMetadataAndEnqueuesUpdateOp drives
// the refresh branch and asserts the new op-based contract:
//
//  1. The local metadata due_date is advanced to the new deadline in tx.
//  2. An update_deadline op for the row is enqueued in the SAME tx (no
//     post-commit Todoist call, no carried deadline).
//  3. Exactly one Decision is emitted (Action=Refresh) — the second
//     post-Todoist emission is gone with the closure.
func TestFollowUpManager_Refresh_UpdatesMetadataAndEnqueuesUpdateOp(t *testing.T) {
	contactID := uuid.New()
	taskID := uuid.New()
	cadenceStr := "weekly"

	tasks := &stubFollowUpTaskReader{
		pending: &repository.ContactTask{
			ID:             taskID,
			ContactID:      contactID,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleFollowUpLoop,
			State:          repository.ContactTaskStateManaged,
			ExternalTaskID: "ext-remote-123",
			Metadata:       map[string]any{"due_date": "2026-01-01"},
		},
	}
	inters := &stubInteractionResponseReader{}
	claims := &stubEventClaimer{}
	contacts := &stubFollowUpContactReader{cadence: &cadenceStr}
	writer := &stubFollowUpTaskWriter{updatedTask: tasks.pending}
	inserter := &recordingInserter{}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "p", LabelName: "l", IntegrationInstanceID: "i"}, "token", nil
	}

	var observed []Decision
	h := NewFollowUpManager(FollowUpModeCutover, claims, contacts, tasks, writer, inters, inserter, settings, "http://localhost", testWatchdog())
	h.SetDecisionObserver(func(d Decision) { observed = append(observed, d) })

	env := buildRecordedEnv(t, contactID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, accelerated.GetCurrentTime(), cadenceStr)
	err := h.HandleEvent(context.Background(), nonNilFakeTx(), env)
	require.NoError(t, err)

	// Metadata due_date advanced in tx.
	require.Equal(t, taskID, writer.lastMetadataID)
	require.NotEmpty(t, writer.lastMetadata["due_date"])

	// Exactly one update_deadline op enqueued for this row, in-tx.
	require.Len(t, inserter.args, 1, "refresh must enqueue exactly one op")
	op, ok := inserter.args[0].(consumerjobs.TodoistTaskOpArgs)
	require.True(t, ok, "enqueued arg must be a TodoistTaskOpArgs")
	require.Equal(t, consumerjobs.TaskOpUpdateDeadline, op.Op)
	require.Equal(t, taskID, op.ContactTaskID)

	// Exactly one Decision emitted (the post-Todoist second emit is gone).
	require.Len(t, observed, 1, "refresh must emit exactly one decision")
	d0 := observed[0]
	require.Equal(t, repository.FollowUpActionRefresh, d0.Action)
	require.Empty(t, d0.SkipReason)
	require.NotNil(t, d0.WouldDeadline, "refresh must report the would-be deadline")
	require.NotNil(t, d0.ContactTaskID)
	require.Equal(t, taskID, *d0.ContactTaskID)
}

// stubRiverInserter satisfies the manager constructor for tests that
// don't assert on enqueued jobs. Returns a nil result.
type stubRiverInserter struct{}

func (*stubRiverInserter) InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, nil
}
