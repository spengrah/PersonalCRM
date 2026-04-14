package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Stubs. Unit tests exercise HandleEvent without a DB — the tx param is a
// typed nil (shadow observations and publishes go through stubs).
// -----------------------------------------------------------------------------

type stubContactRepo struct {
	getCalls   int
	notFound   bool
	lastID     uuid.UUID
	returnErr  error
	lastGetCtx context.Context
}

func (s *stubContactRepo) GetContactTx(ctx context.Context, _ pgx.Tx, id uuid.UUID) (*repository.Contact, error) {
	s.getCalls++
	s.lastID = id
	s.lastGetCtx = ctx
	if s.notFound {
		return nil, db.ErrNotFound
	}
	if s.returnErr != nil {
		return nil, s.returnErr
	}
	return &repository.Contact{ID: id, FullName: "Test"}, nil
}

type stubInteractionRepo struct {
	createCalls    int
	createErr      error
	lastCreated    *repository.Interaction
	existingByRef  *repository.Interaction
	existingWindow *repository.Interaction
	findRefErr     error
	findWindowErr  error
	lastCreateReq  repository.CreateInteractionRequest
}

func (s *stubInteractionRepo) CreateInteractionTx(_ context.Context, _ pgx.Tx, req repository.CreateInteractionRequest) (*repository.Interaction, error) {
	s.createCalls++
	s.lastCreateReq = req
	if s.createErr != nil {
		return nil, s.createErr
	}
	inter := &repository.Interaction{
		ID:         uuid.New(),
		ContactID:  req.ContactID,
		Source:     req.Source,
		SourceRef:  req.SourceRef,
		OccurredAt: req.OccurredAt,
		Direction:  req.Direction,
	}
	s.lastCreated = inter
	return inter, nil
}

func (s *stubInteractionRepo) FindBySourceRefTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ string, _ string) (*repository.Interaction, error) {
	if s.findRefErr != nil {
		return nil, s.findRefErr
	}
	if s.existingByRef != nil {
		return s.existingByRef, nil
	}
	return nil, db.ErrNotFound
}

func (s *stubInteractionRepo) FindInWindowTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ string, _ time.Time, _ time.Duration) (*repository.Interaction, error) {
	if s.findWindowErr != nil {
		return nil, s.findWindowErr
	}
	if s.existingWindow != nil {
		return s.existingWindow, nil
	}
	return nil, db.ErrNotFound
}

type stubBus struct {
	publishCalls int
	publishErr   error
	lastEnv      *events.Envelope
}

func (s *stubBus) PublishTx(_ context.Context, _ pgx.Tx, env *events.Envelope) error {
	s.publishCalls++
	s.lastEnv = env
	if s.publishErr != nil {
		return s.publishErr
	}
	env.ID = uuid.New()
	return nil
}

func (s *stubBus) GetEvent(_ context.Context, _ uuid.UUID) (*events.Envelope, error) {
	return nil, db.ErrNotFound
}

type stubShadowRepo struct {
	writes       []repository.ShadowObservation
	replays      []repository.ShadowObservation
	writeErr     error
	replayErr    error
	peerForMatch *repository.ShadowObservation
}

func (s *stubShadowRepo) RecordConsumerWrite(_ context.Context, _ pgx.Tx, obs repository.ShadowObservation) (*repository.ShadowObservation, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	obs.ID = uuid.New()
	s.writes = append(s.writes, obs)
	out := obs
	return &out, nil
}

func (s *stubShadowRepo) RecordConsumerReplay(_ context.Context, _ pgx.Tx, obs repository.ShadowObservation) (*repository.ShadowObservation, error) {
	if s.replayErr != nil {
		return nil, s.replayErr
	}
	obs.ID = uuid.New()
	obs.Replay = true
	s.replays = append(s.replays, obs)
	out := obs
	return &out, nil
}

func (s *stubShadowRepo) FindMatchingDirectWrite(_ context.Context, _ pgx.Tx, _ repository.ShadowObservation) (*repository.ShadowObservation, error) {
	return s.peerForMatch, nil
}

// nonNilTx returns a typed-nil pgx.Tx. Stubs never deref the value.
func nonNilTx() pgx.Tx {
	var t *txStub
	return t
}

type txStub struct{ pgx.Tx }

// -----------------------------------------------------------------------------
// Helpers for building envelopes with a valid payload for each kind.
// -----------------------------------------------------------------------------

func mustEnv(t *testing.T, kind events.Kind, payload any) *events.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     kindDefaultSource(kind),
		SourceID:   "stub-" + uuid.NewString(),
		Kind:       kind,
		Payload:    raw,
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
}

func kindDefaultSource(kind events.Kind) string {
	switch kind {
	case events.KindCalendarAttended, events.KindCalendarDeclined:
		return "gcal"
	case events.KindTaskCompleted, events.KindTaskOutreachDetected, events.KindTaskSkipped:
		return "todoist"
	case events.KindInteractionManual:
		return "manual"
	}
	return "telegram"
}

func newRecorderWithStubs(mode string) (*InteractionRecorder, *stubContactRepo, *stubInteractionRepo, *stubBus, *stubShadowRepo) {
	cr := &stubContactRepo{}
	ir := &stubInteractionRepo{}
	b := &stubBus{}
	sr := &stubShadowRepo{}
	rec := NewInteractionRecorder(mode, cr, ir, b, sr)
	return rec, cr, ir, b, sr
}

// -----------------------------------------------------------------------------
// Per-kind new-write cases. Each asserts: interaction insert runs with the
// expected (source, source_ref, direction), interaction.recorded is published
// with SourceID = interaction.ID, and a writer='consumer' replay=false
// observation is written in shadow mode.
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_NewWrite(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
	})

	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, 1, ir.createCalls)
	require.Equal(t, repository.InteractionSourceTelegram, ir.lastCreateReq.Source)
	require.NotNil(t, ir.lastCreateReq.SourceRef)
	require.Equal(t, "tg:1:2:10", *ir.lastCreateReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionInbound, ir.lastCreateReq.Direction)

	require.Equal(t, 1, b.publishCalls)
	require.Equal(t, events.KindInteractionRecorded, b.lastEnv.Kind)
	require.Equal(t, ir.lastCreated.ID.String(), b.lastEnv.SourceID)

	require.Len(t, sr.writes, 1)
	require.Equal(t, repository.InteractionDirectionInbound, sr.writes[0].Direction)
	require.False(t, sr.writes[0].Replay)
}

func TestHandleEvent_MessageSent_NewWrite_DefaultOutbound(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:11",
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionDirectionOutbound, ir.lastCreateReq.Direction)
}

func TestHandleEvent_MessageReceived_PayloadDirectionMutual(t *testing.T) {
	// Fresh-mutual telegram session: publisher sets Direction="mutual" in
	// the payload (plan Decision 6). Consumer must honor it.
	cid := uuid.New()
	rec, _, ir, _, sr := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:20",
		Direction:         "mutual",
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
	require.Len(t, sr.writes, 1)
	require.Equal(t, repository.InteractionDirectionMutual, sr.writes[0].Direction)
}

func TestHandleEvent_MessageSent_PayloadDirectionMutual(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:21",
		Direction:         "mutual",
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
}

func TestHandleEvent_CalendarAttended_NewWrite(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-1",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionSourceGCal, ir.lastCreateReq.Source)
	require.NotNil(t, ir.lastCreateReq.SourceRef)
	require.Equal(t, "gcal-evt-1", *ir.lastCreateReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
	require.Equal(t, 1, b.publishCalls)
	require.Len(t, sr.writes, 1)
}

func TestHandleEvent_TaskCompleted_NewWrite(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:     1,
		ContactID:   cid,
		TaskID:      "6fw9cQQ5JppCp7qX",
		TaskKind:    "cadence",
		CompletedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Direction:   "mutual",
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionSourceTodoist, ir.lastCreateReq.Source)
	require.NotNil(t, ir.lastCreateReq.SourceRef)
	require.Equal(t, "6fw9cQQ5JppCp7qX", *ir.lastCreateReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
}

func TestHandleEvent_TaskCompleted_EmptyDirectionDefaultsMutual(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:     1,
		ContactID:   cid,
		TaskID:      "tk1",
		TaskKind:    "cadence",
		CompletedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		// Direction empty — plan Decision 3 default.
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
}

func TestHandleEvent_TaskOutreachDetected_NewWrite(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindTaskOutreachDetected, events.TaskOutreachDetectedPayload{
		Version:    1,
		ContactID:  cid,
		TaskID:     "tk-outreach",
		DetectedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionSourceTodoist, ir.lastCreateReq.Source)
	require.Equal(t, repository.InteractionDirectionOutbound, ir.lastCreateReq.Direction)
}

func TestHandleEvent_InteractionManual_NewWrite(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "mutual",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionSourceManual, ir.lastCreateReq.Source)
	require.Nil(t, ir.lastCreateReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
	require.Equal(t, 1, b.publishCalls)
	require.Len(t, sr.writes, 1)
	require.Nil(t, sr.writes[0].SourceRef)
}

func TestHandleEvent_InteractionManual_EmptyDirectionDefaultsMutual(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Equal(t, repository.InteractionDirectionMutual, ir.lastCreateReq.Direction)
}

// -----------------------------------------------------------------------------
// Replay cases. FindBySourceRef / FindInWindow return an existing row →
// consumer writes a replay=true observation, does NOT emit interaction.recorded,
// does NOT insert a new interaction row.
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_Replay(t *testing.T) {
	cid := uuid.New()
	existing := &repository.Interaction{
		ID:        uuid.New(),
		ContactID: cid,
		Source:    repository.InteractionSourceTelegram,
		Direction: repository.InteractionDirectionInbound,
	}
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	ir.existingByRef = existing

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, ir.createCalls, "replay must not create a new interaction row")
	require.Zero(t, b.publishCalls, "replay must not emit interaction.recorded")
	require.Len(t, sr.replays, 1)
	require.True(t, sr.replays[0].Replay)
	require.Empty(t, sr.writes)
}

func TestHandleEvent_InteractionManual_Replay(t *testing.T) {
	cid := uuid.New()
	existing := &repository.Interaction{
		ID:        uuid.New(),
		ContactID: cid,
		Source:    repository.InteractionSourceManual,
		Direction: repository.InteractionDirectionMutual,
	}
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	ir.existingWindow = existing

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "mutual",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Zero(t, ir.createCalls)
	require.Zero(t, b.publishCalls)
	require.Len(t, sr.replays, 1)
}

// -----------------------------------------------------------------------------
// Missing-contact cases. GetContactTx returns db.ErrNotFound → propagated
// wrapped; no interaction insert, no publish, no shadow obs.
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_MissingContact(t *testing.T) {
	cid := uuid.New()
	rec, cr, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	cr.notFound = true

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
	})
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, ir.createCalls)
	require.Zero(t, b.publishCalls)
	require.Empty(t, sr.writes)
	require.Empty(t, sr.replays)
}

func TestHandleEvent_CalendarAttended_MissingContact(t *testing.T) {
	cid := uuid.New()
	rec, cr, ir, b, _ := newRecorderWithStubs(InteractionModeShadow)
	cr.notFound = true

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-miss",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, ir.createCalls)
	require.Zero(t, b.publishCalls)
}

// -----------------------------------------------------------------------------
// Atomicity. If interaction insert fails, the publish must not fire; if
// publish fails after insert, the caller sees the error and the tx rolls back.
// -----------------------------------------------------------------------------

func TestHandleEvent_InsertFailure_SkipsPublishAndObservation(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, b, sr := newRecorderWithStubs(InteractionModeShadow)
	ir.createErr = errors.New("simulated insert failure")

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-fail",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "create interaction")
	require.Zero(t, b.publishCalls)
	require.Empty(t, sr.writes)
}

func TestHandleEvent_PublishFailure_ReturnsError(t *testing.T) {
	cid := uuid.New()
	rec, _, _, b, sr := newRecorderWithStubs(InteractionModeShadow)
	b.publishErr = errors.New("simulated publish failure")

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-pub-fail",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "publish interaction.recorded")
	// Shadow observation for fresh-write never fires because publish errored first.
	require.Empty(t, sr.writes)
}

// -----------------------------------------------------------------------------
// Mode gate. InteractionModeOff skips shadow observations entirely.
// -----------------------------------------------------------------------------

func TestHandleEvent_OffMode_SkipsShadowObservations(t *testing.T) {
	cid := uuid.New()
	rec, _, _, _, sr := newRecorderWithStubs(InteractionModeOff)
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-off",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.Empty(t, sr.writes)
	require.Empty(t, sr.replays)
}

// -----------------------------------------------------------------------------
// Defensive error paths.
// -----------------------------------------------------------------------------

func TestHandleEvent_NilEnvelope_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs(InteractionModeShadow)
	require.Error(t, rec.HandleEvent(context.Background(), nonNilTx(), nil))
}

func TestHandleEvent_NilTx_Errors(t *testing.T) {
	cid := uuid.New()
	rec, _, _, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: cid, EventID: "x", OccurredAt: time.Now(),
	})
	require.Error(t, rec.HandleEvent(context.Background(), nil, env))
}

func TestHandleEvent_UnknownKind_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := &events.Envelope{
		Kind:       events.Kind("made.up"),
		Source:     "telegram",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Now(),
	}
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extract made.up")
}

func TestHandleEvent_UnresolvedContactID_Errors(t *testing.T) {
	rec, _, ir, b, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         nil, // publisher bug per plan Decision 4.
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:unresolved",
	})
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "contact_id unresolved")
	require.Zero(t, ir.createCalls)
	require.Zero(t, b.publishCalls)
}

func TestHandleEvent_PayloadUnmarshalFailure_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs(InteractionModeShadow)
	env := &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindMessageReceived,
		Source:     "telegram",
		Payload:    json.RawMessage(`{not valid json`),
		ObservedAt: time.Now(),
	}
	err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// Interaction.recorded envelope fields.
// -----------------------------------------------------------------------------

func TestHandleEvent_RecordedEventSourceIDIsInteractionID(t *testing.T) {
	cid := uuid.New()
	rec, _, ir, b, _ := newRecorderWithStubs(InteractionModeShadow)
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-sid",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, rec.HandleEvent(context.Background(), nonNilTx(), env))
	require.NotNil(t, ir.lastCreated)
	require.Equal(t, ir.lastCreated.ID.String(), b.lastEnv.SourceID,
		"interaction.recorded SourceID must equal interaction.ID (plan Decision 9)")
	require.Equal(t, env.Source, b.lastEnv.Source)

	// Payload decodes correctly.
	var decoded events.InteractionRecordedPayload
	require.NoError(t, events.Unmarshal(b.lastEnv, &decoded))
	require.Equal(t, cid, decoded.ContactID)
	require.Equal(t, ir.lastCreated.ID, decoded.InteractionID)
	require.Equal(t, repository.InteractionDirectionMutual, decoded.Direction)
	require.Equal(t, repository.InteractionSourceGCal, decoded.Source)
	require.NotNil(t, decoded.SourceRef)
	require.Equal(t, "gcal-evt-sid", *decoded.SourceRef)
}
