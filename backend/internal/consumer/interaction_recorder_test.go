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
// typed nil (the writer stub never dereferences it).
// -----------------------------------------------------------------------------

// stubWriter is the cutover-era replacement for the PR 5 stubContactRepo /
// stubInteractionRepo split. It emulates ContactService.RecordInteractionTx:
// dedup, contact-existence check, and interaction insert collapse into one
// entry point (plan Decision 4a). PR 7 extends the returns with the
// pre-cadence snapshot + cadence-at-emit value (plan Decision 2a) plus a
// shadow-drain closure (plan Decision 6).
type stubWriter struct {
	calls   int
	lastReq repository.RecordInteractionRequest
	// lastWithShadow captures the withShadow arg so tests can assert
	// the consumer threads it as true.
	lastWithShadow bool
	// Existing row forces isReplay=true; otherwise a fresh row is fabricated.
	existing *repository.Interaction
	// notFound simulates GetContactTx returning db.ErrNotFound.
	notFound bool
	// returnErr forces a non-dedup error from the writer.
	returnErr error
	// lastCreated records the fabricated row on fresh writes (nil on replay).
	lastCreated *repository.Interaction
	// postCommit is returned on fresh writes; caller may invoke it.
	postCommit func(context.Context)
	// prev + cadenceAtEmit are the PR 7 pre-cadence snapshot returns.
	// Tests set these to exercise V2 payload construction in HandleEvent.
	prev          *repository.ContactCadenceFields
	cadenceAtEmit *string
	// shadowDrain is the PR 7 cadence shadow drain closure. Tests set
	// this to assert the consumer invokes it with recordedEnv.ID.
	shadowDrain repository.CadenceShadowDrainFn
	// lastShadowDrainEventID captures the eventID passed to shadowDrain
	// (set by the closure when the consumer invokes it).
	lastShadowDrainEventID *uuid.UUID
}

func (s *stubWriter) RecordInteractionTx(
	_ context.Context, _ pgx.Tx, withShadow bool, req repository.RecordInteractionRequest,
) (*repository.RecordInteractionResult, error) {
	s.calls++
	s.lastReq = req
	s.lastWithShadow = withShadow
	if s.returnErr != nil {
		return nil, s.returnErr
	}
	if s.notFound {
		return nil, db.ErrNotFound
	}
	if s.existing != nil {
		return &repository.RecordInteractionResult{Interaction: s.existing, IsReplay: true}, nil
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
	// Wrap shadowDrain so we can observe the eventID passed at drain time.
	var drain repository.CadenceShadowDrainFn
	if s.shadowDrain != nil {
		base := s.shadowDrain
		drain = func(ctx context.Context, eventID uuid.UUID) {
			s.lastShadowDrainEventID = &eventID
			base(ctx, eventID)
		}
	}
	return &repository.RecordInteractionResult{
		Interaction:   inter,
		IsReplay:      false,
		PrevCadence:   s.prev,
		CadenceAtEmit: s.cadenceAtEmit,
		FollowUpFn:    s.postCommit,
		ShadowDrainFn: drain,
	}, nil
}

type stubTGRepo struct {
	calls           int
	markErr         error
	lastInteraction uuid.UUID
	lastMessageIDs  []uuid.UUID
}

func (s *stubTGRepo) MarkMessagesProcessedTx(_ context.Context, _ pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	s.calls++
	s.lastMessageIDs = messageIDs
	s.lastInteraction = interactionID
	return s.markErr
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

// newRecorderWithStubs constructs the consumer with fresh stubs. In
// cutover there is no mode parameter — the consumer runs unconditionally
// wherever it's wired (mode gating lives at the publisher + manual-
// handler wiring level per plan Decision 6).
func newRecorderWithStubs() (*InteractionRecorder, *stubWriter, *stubTGRepo, *stubBus, *stubCadence) {
	w := &stubWriter{}
	tg := &stubTGRepo{}
	b := &stubBus{}
	c := &stubCadence{}
	rec := NewInteractionRecorder(w, tg, b, c)
	return rec, w, tg, b, c
}

// stubCadence captures HandleEvent calls so tests can assert the
// inline-apply seam fired with the interaction.recorded envelope.
type stubCadence struct {
	calls       int
	lastEnvID   uuid.UUID
	returnError error
}

func (s *stubCadence) HandleEvent(_ context.Context, _ pgx.Tx, env *events.Envelope) error {
	s.calls++
	if env != nil {
		s.lastEnvID = env.ID
	}
	return s.returnError
}

// -----------------------------------------------------------------------------
// Per-kind new-write cases. Each asserts: RecordInteractionTx runs with the
// expected (source, source_ref, direction), interaction.recorded is published
// with SourceID = interaction.ID, and MarkMessagesProcessedTx fires only on
// message.* kinds (plan Decisions 4a + 10).
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_CutoverFreshWrite(t *testing.T) {
	cid := uuid.New()
	msgID := uuid.New()
	rec, w, tg, b, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
		MessageIDs:        []uuid.UUID{msgID},
	})

	interaction, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, interaction)

	require.Equal(t, 1, w.calls)
	require.Equal(t, repository.InteractionSourceTelegram, w.lastReq.Source)
	require.NotNil(t, w.lastReq.SourceRef)
	require.Equal(t, "tg:1:2:10", *w.lastReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionInbound, w.lastReq.Direction)

	require.Equal(t, 1, b.publishCalls)
	require.Equal(t, events.KindInteractionRecorded, b.lastEnv.Kind)
	require.Equal(t, w.lastCreated.ID.String(), b.lastEnv.SourceID)

	// MarkMessagesProcessedTx fires inside the same tx (plan Decision 10).
	require.Equal(t, 1, tg.calls, "message.* kinds mark telegram messages processed in-tx")
	require.Equal(t, []uuid.UUID{msgID}, tg.lastMessageIDs)
	require.Equal(t, w.lastCreated.ID, tg.lastInteraction)
}

func TestHandleEvent_MessageSent_CutoverFreshWrite_DefaultOutbound(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:11",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionOutbound, w.lastReq.Direction)
}

func TestHandleEvent_MessageReceived_PayloadDirectionMutual(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:20",
		Direction:         "mutual",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
}

func TestHandleEvent_MessageSent_PayloadDirectionMutual(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:21",
		Direction:         "mutual",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
}

func TestHandleEvent_CalendarAttended_CutoverFreshWrite_NoMarkProcessed(t *testing.T) {
	cid := uuid.New()
	rec, w, tg, b, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-1",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionSourceGCal, w.lastReq.Source)
	require.NotNil(t, w.lastReq.SourceRef)
	require.Equal(t, "gcal-evt-1", *w.lastReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
	require.Nil(t, w.lastReq.Description, "calendar payload without Title leaves description nil")
	require.Equal(t, 1, b.publishCalls)
	require.Zero(t, tg.calls, "calendar kind does not mark telegram messages processed")
}

// TestHandleEvent_CalendarAttended_CopiesTitleToDescription asserts that
// the payload's Title flows through to interaction.description so
// calendar interactions preserve their user-visible context post-cutover
// (Codex PR 6 P2 regression fix).
func TestHandleEvent_CalendarAttended_CopiesTitleToDescription(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	title := "Quarterly sync with Alice"
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-titled",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Title:      &title,
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, w.lastReq.Description)
	require.Equal(t, title, *w.lastReq.Description)
}

func TestHandleEvent_TaskCompleted_CutoverFreshWrite(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:     1,
		ContactID:   cid,
		TaskID:      "6fw9cQQ5JppCp7qX",
		TaskKind:    "cadence",
		CompletedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Direction:   "mutual",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionSourceTodoist, w.lastReq.Source)
	require.NotNil(t, w.lastReq.SourceRef)
	require.Equal(t, "6fw9cQQ5JppCp7qX", *w.lastReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
}

func TestHandleEvent_TaskCompleted_EmptyDirectionDefaultsMutual(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:     1,
		ContactID:   cid,
		TaskID:      "tk1",
		TaskKind:    "cadence",
		CompletedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
}

func TestHandleEvent_TaskOutreachDetected_CutoverFreshWrite(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindTaskOutreachDetected, events.TaskOutreachDetectedPayload{
		Version:    1,
		ContactID:  cid,
		TaskID:     "tk-outreach",
		DetectedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionSourceTodoist, w.lastReq.Source)
	require.Equal(t, repository.InteractionDirectionOutbound, w.lastReq.Direction)
}

func TestHandleEvent_InteractionManual_CutoverReturnsInteraction(t *testing.T) {
	cid := uuid.New()
	rec, w, _, b, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "mutual",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	interaction, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, interaction, "manual kind must return the interaction for the HTTP response")
	require.Equal(t, repository.InteractionSourceManual, w.lastReq.Source)
	require.Nil(t, w.lastReq.SourceRef)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
	require.Equal(t, 1, b.publishCalls)
}

func TestHandleEvent_InteractionManual_EmptyDirectionDefaultsMutual(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, w.lastReq.Direction)
}

// -----------------------------------------------------------------------------
// Replay cases. Writer returns isReplay=true → consumer skips interaction.recorded
// emit, returns nil postCommit, but mark-processed still fires for telegram
// kinds (plan Decision 10 — matches today's publisher's unconditional
// MarkMessagesProcessed call).
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_CutoverReplay(t *testing.T) {
	cid := uuid.New()
	msgID := uuid.New()
	existing := &repository.Interaction{
		ID:        uuid.New(),
		ContactID: cid,
		Source:    repository.InteractionSourceTelegram,
		Direction: repository.InteractionDirectionInbound,
	}
	rec, w, tg, b, _ := newRecorderWithStubs()
	w.existing = existing

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
		MessageIDs:        []uuid.UUID{msgID},
	})
	interaction, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, existing.ID, interaction.ID, "replay returns the existing row")
	require.Nil(t, postCommit, "replay returns nil postCommit (plan Decision 8)")
	require.Zero(t, b.publishCalls, "replay must not emit interaction.recorded (spec §3.4.1)")
	require.Equal(t, 1, tg.calls, "mark-processed runs on replay per plan Decision 10")
	require.Equal(t, existing.ID, tg.lastInteraction)
}

func TestHandleEvent_InteractionManual_Replay(t *testing.T) {
	cid := uuid.New()
	existing := &repository.Interaction{
		ID:        uuid.New(),
		ContactID: cid,
		Source:    repository.InteractionSourceManual,
		Direction: repository.InteractionDirectionMutual,
	}
	rec, w, _, b, _ := newRecorderWithStubs()
	w.existing = existing

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "mutual",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	interaction, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, existing.ID, interaction.ID)
	require.Nil(t, postCommit)
	require.Zero(t, b.publishCalls)
}

// -----------------------------------------------------------------------------
// Missing-contact cases. Writer propagates db.ErrNotFound; no publish fires.
// -----------------------------------------------------------------------------

func TestHandleEvent_MessageReceived_MissingContact(t *testing.T) {
	cid := uuid.New()
	rec, w, tg, b, _ := newRecorderWithStubs()
	w.notFound = true

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:10",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, tg.calls)
	require.Zero(t, b.publishCalls)
}

func TestHandleEvent_CalendarAttended_MissingContact(t *testing.T) {
	cid := uuid.New()
	rec, w, _, b, _ := newRecorderWithStubs()
	w.notFound = true

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-miss",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, b.publishCalls)
}

// -----------------------------------------------------------------------------
// Atomicity. Writer errors, mark-processed errors, and publish errors all
// propagate so the caller's BeginTxFunc rolls the tx back.
// -----------------------------------------------------------------------------

func TestHandleEvent_WriterFailure_SkipsPublishAndMarkProcessed(t *testing.T) {
	cid := uuid.New()
	rec, w, tg, b, _ := newRecorderWithStubs()
	w.returnErr = errors.New("simulated writer failure")

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-fail",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "record interaction tx")
	require.Zero(t, b.publishCalls)
	require.Zero(t, tg.calls)
}

func TestHandleEvent_MarkProcessedFailure_RollsBack(t *testing.T) {
	cid := uuid.New()
	msgID := uuid.New()
	rec, _, tg, b, _ := newRecorderWithStubs()
	tg.markErr = errors.New("simulated mark-processed failure")

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:mp-fail",
		MessageIDs:        []uuid.UUID{msgID},
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mark telegram messages processed")
	require.Zero(t, b.publishCalls, "publish must not run after mark-processed fails")
}

func TestHandleEvent_PublishFailure_ReturnsError(t *testing.T) {
	cid := uuid.New()
	rec, _, _, b, _ := newRecorderWithStubs()
	b.publishErr = errors.New("simulated publish failure")

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-pub-fail",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "publish interaction.recorded")
}

// -----------------------------------------------------------------------------
// postCommit propagation.
// -----------------------------------------------------------------------------

func TestHandleEvent_PostCommitBubblesUp(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	invoked := false
	w.postCommit = func(context.Context) { invoked = true }

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "outbound",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, postCommit, "fresh write must return non-nil postCommit when writer supplied one")
	require.False(t, invoked, "HandleEvent must not invoke postCommit itself (caller runs it after tx commit)")
	postCommit(context.Background())
	require.True(t, invoked)
}

// -----------------------------------------------------------------------------
// Defensive error paths.
// -----------------------------------------------------------------------------

func TestHandleEvent_NilEnvelope_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs()
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), nil)
	require.Error(t, err)
}

func TestHandleEvent_NilTx_Errors(t *testing.T) {
	cid := uuid.New()
	rec, _, _, _, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: cid, EventID: "x",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nil, env)
	require.Error(t, err)
}

func TestHandleEvent_UnknownKind_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs()
	env := &events.Envelope{
		Kind:       events.Kind("made.up"),
		Source:     "telegram",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extract made.up")
}

func TestHandleEvent_UnresolvedContactID_Errors(t *testing.T) {
	rec, w, _, b, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         nil, // publisher bug per plan Decision 4.
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:unresolved",
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "contact_id unresolved")
	require.Zero(t, w.calls)
	require.Zero(t, b.publishCalls)
}

// TestHandleEvent_RefBearingKind_EmptySourceRef_Errors verifies that
// ref-bearing kinds fail fast when their source_ref field is empty.
func TestHandleEvent_RefBearingKind_EmptySourceRef_Errors(t *testing.T) {
	cid := uuid.New()
	ctx := context.Background()

	tests := []struct {
		name    string
		env     func() *events.Envelope
		wantSub string
	}{
		{
			name: "calendar.attended_empty_event_id",
			env: func() *events.Envelope {
				return mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
					Version: 1, ContactID: cid, EventID: "",
					OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
				})
			},
			wantSub: "empty event_id",
		},
		{
			name: "task.completed_empty_task_id",
			env: func() *events.Envelope {
				return mustEnv(t, events.KindTaskCompleted, events.TaskCompletedPayload{
					Version: 1, ContactID: cid, TaskID: "", TaskKind: "cadence",
					CompletedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
				})
			},
			wantSub: "empty task_id",
		},
		{
			name: "task.outreach_detected_empty_task_id",
			env: func() *events.Envelope {
				return mustEnv(t, events.KindTaskOutreachDetected, events.TaskOutreachDetectedPayload{
					Version: 1, ContactID: cid, TaskID: "",
					DetectedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
				})
			},
			wantSub: "empty task_id",
		},
		{
			name: "message.received_empty_external_message_id",
			env: func() *events.Envelope {
				return mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
					Version: 1, ContactID: &cid, PeerRef: "tg:1:2",
					MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
					ExternalMessageID: "",
				})
			},
			wantSub: "empty external_message_id",
		},
		{
			name: "message.sent_empty_external_message_id",
			env: func() *events.Envelope {
				return mustEnv(t, events.KindMessageSent, events.MessageSentPayload{
					Version: 1, ContactID: &cid, PeerRef: "tg:1:2",
					MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
					ExternalMessageID: "",
				})
			},
			wantSub: "empty external_message_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, w, _, b, _ := newRecorderWithStubs()
			_, _, err := rec.HandleEvent(ctx, nonNilTx(), tt.env())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantSub)
			require.Zero(t, w.calls)
			require.Zero(t, b.publishCalls)
		})
	}
}

func TestHandleEvent_PayloadUnmarshalFailure_Errors(t *testing.T) {
	rec, _, _, _, _ := newRecorderWithStubs()
	env := &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindMessageReceived,
		Source:     "telegram",
		Payload:    json.RawMessage(`{not valid json`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// Interaction.recorded envelope fields.
// -----------------------------------------------------------------------------

func TestHandleEvent_RecordedEventSourceIDIsInteractionID(t *testing.T) {
	cid := uuid.New()
	rec, w, _, b, _ := newRecorderWithStubs()
	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    "gcal-evt-sid",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, w.lastCreated)
	require.Equal(t, w.lastCreated.ID.String(), b.lastEnv.SourceID,
		"interaction.recorded SourceID must equal interaction.ID (plan Decision 9)")
	require.Equal(t, env.Source, b.lastEnv.Source)

	// Payload decodes correctly.
	var decoded events.InteractionRecordedPayload
	require.NoError(t, events.Unmarshal(b.lastEnv, &decoded))
	require.Equal(t, cid, decoded.ContactID)
	require.Equal(t, w.lastCreated.ID, decoded.InteractionID)
	require.Equal(t, repository.InteractionDirectionMutual, decoded.Direction)
	require.Equal(t, repository.InteractionSourceGCal, decoded.Source)
	require.NotNil(t, decoded.SourceRef)
	require.Equal(t, "gcal-evt-sid", *decoded.SourceRef)
}
