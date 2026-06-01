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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Stubs. Unit tests exercise HandleEvent without a DB — the tx param is a
// typed nil (the writer stub never dereferences it).
// -----------------------------------------------------------------------------

// stubWriter emulates ContactService.RecordInteractionTx: dedup,
// contact-existence check, and interaction insert collapse into one
// entry point. The pre-cadence snapshot + cadence-at-emit returns
// populate the V2 InteractionRecordedPayload that HandleEvent emits.
type stubWriter struct {
	calls   int
	lastReq repository.RecordInteractionRequest
	// lastPublishesEvent captures the publishesEvent arg so tests can assert
	// the consumer threads it as true.
	lastPublishesEvent bool
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
	// prev + cadenceAtEmit are the pre-cadence snapshot returns. Tests
	// set these to exercise V2 payload construction in HandleEvent.
	prev          *repository.ContactCadenceFields
	cadenceAtEmit *string
}

func (s *stubWriter) RecordInteractionTx(
	_ context.Context, _ pgx.Tx, publishesEvent bool, req repository.RecordInteractionRequest,
) (*repository.RecordInteractionResult, error) {
	s.calls++
	s.lastReq = req
	s.lastPublishesEvent = publishesEvent
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
	return &repository.RecordInteractionResult{
		Interaction:   inter,
		IsReplay:      false,
		PrevCadence:   s.prev,
		CadenceAtEmit: s.cadenceAtEmit,
		FollowUpFn:    s.postCommit,
	}, nil
}

type stubTGRepo struct {
	calls           int
	markErr         error
	lastInteraction uuid.UUID
	lastMessageIDs  []uuid.UUID
	lastSource      string
	lastSessionRef  string
	// forceAffected overrides the default len(messageIDs) return when
	// non-nil. Used to simulate the boundary-shift race where the
	// SQL predicate matched zero rows.
	forceAffected *int64
}

func (s *stubTGRepo) MarkProcessedTx(_ context.Context, _ pgx.Tx, source string, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	s.calls++
	s.lastSource = source
	s.lastMessageIDs = messageIDs
	s.lastInteraction = interactionID
	s.lastSessionRef = sessionRef
	if s.markErr != nil {
		return 0, s.markErr
	}
	if s.forceAffected != nil {
		return *s.forceAffected, nil
	}
	return int64(len(messageIDs)), nil
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

// newRecorderWithStubs constructs the consumer with fresh stubs. The
// consumer has no mode parameter — it runs unconditionally wherever
// it's wired; mode gating lives at the publisher + manual-handler
// wiring level.
func newRecorderWithStubs() (*InteractionRecorder, *stubWriter, *stubTGRepo, *stubBus, *stubCadence) {
	w := &stubWriter{}
	tg := &stubTGRepo{}
	b := &stubBus{}
	c := &stubCadence{}
	// calendarLock is nil here: existing tests use placeholder (non-UUID)
	// EventIDs and don't exercise the attended-vs-decline race, so the lock
	// check is skipped. The skip test constructs its own recorder with a
	// stubCalendarLocker + a UUID EventID.
	rec := NewInteractionRecorder(w, tg, b, c, nil, nil)
	return rec, w, tg, b, c
}

// stubCalendarLocker stubs the calendarEventLocker dependency. `exists`
// controls whether the backing calendar_event is reported present; tests
// for the attended-vs-decline race set exists=false to assert the recorder
// skips the insert.
type stubCalendarLocker struct {
	exists  bool
	calls   int
	lastID  uuid.UUID
	returnE error
}

func (s *stubCalendarLocker) LockExistsByIDTx(_ context.Context, _ pgx.Tx, id uuid.UUID) (bool, error) {
	s.calls++
	s.lastID = id
	if s.returnE != nil {
		return false, s.returnE
	}
	return s.exists, nil
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
// Per-kind new-write cases. Each asserts: RecordInteractionTx runs with
// the expected (source, source_ref, direction), interaction.recorded is
// published with SourceID = interaction.ID, and MarkMessagesProcessedTx
// fires only on message.* kinds.
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

	// MarkMessagesProcessedTx fires inside the same tx as the
	// interaction insert so the FK write is atomic with the row it
	// references.
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

// TestHandleEvent_CalendarAttended_SkipsInsertWhenEventDeleted is the
// attended-after-delete unit guard: when the backing calendar_event was
// already deleted (a decline won the race), the locking read reports the row
// gone and the recorder skips the interaction insert entirely — no false
// "attended" interaction is written.
func TestHandleEvent_CalendarAttended_SkipsInsertWhenEventDeleted(t *testing.T) {
	cid := uuid.New()
	eventID := uuid.New()
	w := &stubWriter{}
	tg := &stubTGRepo{}
	b := &stubBus{}
	c := &stubCadence{}
	locker := &stubCalendarLocker{exists: false}
	rec := NewInteractionRecorder(w, tg, b, c, nil, locker)

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    eventID.String(),
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	interaction, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Nil(t, interaction, "no interaction written when backing event deleted")
	require.Nil(t, postCommit)
	require.Equal(t, 1, locker.calls, "lock check ran once")
	require.Equal(t, eventID, locker.lastID)
	require.Zero(t, w.calls, "writer must not be invoked when the event is gone")
	require.Zero(t, b.publishCalls, "no interaction.recorded published when skipped")
}

// TestHandleEvent_CalendarAttended_InsertsWhenEventPresent confirms the
// happy path of the attended-after-delete guard: when the backing
// calendar_event is present (lock acquired), the recorder proceeds with the
// insert.
func TestHandleEvent_CalendarAttended_InsertsWhenEventPresent(t *testing.T) {
	cid := uuid.New()
	eventID := uuid.New()
	w := &stubWriter{}
	tg := &stubTGRepo{}
	b := &stubBus{}
	c := &stubCadence{}
	locker := &stubCalendarLocker{exists: true}
	rec := NewInteractionRecorder(w, tg, b, c, nil, locker)

	env := mustEnv(t, events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  cid,
		EventID:    eventID.String(),
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, locker.calls)
	require.Equal(t, 1, w.calls, "writer invoked when the event is present")
}

// TestHandleEvent_CalendarAttended_CopiesTitleToDescription asserts that
// the payload's Title flows through to interaction.description so
// calendar interactions preserve their user-visible context.
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
// Replay cases. Writer returns isReplay=true → consumer skips
// interaction.recorded emit, returns nil postCommit, but mark-processed
// still fires for telegram kinds — matches the pre-cutover publisher's
// unconditional MarkMessagesProcessed call.
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
	require.Nil(t, postCommit, "replay returns nil postCommit")
	require.Zero(t, b.publishCalls, "replay must not emit interaction.recorded (spec §3.4.1)")
	require.Equal(t, 1, tg.calls, "mark-processed runs on replay")
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
	require.Contains(t, err.Error(), "mark staging messages processed")
	require.Zero(t, b.publishCalls, "publish must not run after mark-processed fails")
}

// TestHandleEvent_MarkProcessedZeroRows_FreshWrite_RollsBack exercises
// the boundary-shift race defense: on a fresh write (IsReplay=false),
// zero rows affected on MarkProcessedTx means another session won the
// race for these rows; the consumer must error so the whole tx rolls
// back, preventing a phantom-duplicate interaction.
func TestHandleEvent_MarkProcessedZeroRows_FreshWrite_RollsBack(t *testing.T) {
	cid := uuid.New()
	msgID := uuid.New()
	rec, _, tg, b, _ := newRecorderWithStubs()
	// Simulate boundary-shift: predicate filtered everything out.
	zero := int64(0)
	tg.forceAffected = &zero

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:zero-fresh",
		MessageIDs:        []uuid.UUID{msgID},
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "matched zero rows")
	require.Zero(t, b.publishCalls, "interaction.recorded must not fire when staging mark matched zero rows on fresh write")
}

// TestHandleEvent_MarkProcessedZeroRows_Replay_Tolerated covers the
// replay path: when RecordInteractionTx returns IsReplay=true, the
// rows were already linked to the existing interaction on the original
// attempt; zero rows affected is expected and the consumer must NOT
// roll back (interaction.recorded is already skipped via the
// res.IsReplay short-circuit further below).
func TestHandleEvent_MarkProcessedZeroRows_Replay_Tolerated(t *testing.T) {
	cid := uuid.New()
	msgID := uuid.New()
	existing := &repository.Interaction{
		ID:        uuid.New(),
		ContactID: cid,
		Source:    repository.InteractionSourceTelegram,
	}
	rec, w, tg, b, _ := newRecorderWithStubs()
	w.existing = existing // → IsReplay=true
	zero := int64(0)
	tg.forceAffected = &zero // replay → 0 rows affected because processed_at != NULL

	env := mustEnv(t, events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &cid,
		PeerRef:           "tg:1:2",
		MessageAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		ExternalMessageID: "tg:1:2:zero-replay",
		MessageIDs:        []uuid.UUID{msgID},
	})
	interaction, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err, "replay with zero affected must NOT error")
	require.NotNil(t, interaction)
	assert.Equal(t, existing.ID, interaction.ID)
	assert.Nil(t, postCommit, "replay returns nil postCommit so re-delivery doesn't re-fire side effects")
	require.Zero(t, b.publishCalls, "replay skips interaction.recorded emit")
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

// TestHandleEvent_FollowUpDispatcher_InlineInvoked asserts that the
// configured follow-up dispatcher's HandleEvent is called inline in
// the same tx as the cadence dispatch, with the interaction.recorded
// envelope.
func TestHandleEvent_FollowUpDispatcher_InlineInvoked(t *testing.T) {
	cid := uuid.New()
	rec, _, _, b, _ := newRecorderWithStubs()
	followUp := &stubFollowUpDispatcher{}
	rec.followUp = followUp

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "outbound",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, _, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, followUp.calls, "follow-up dispatcher must be invoked inline for fresh writes")
	require.NotNil(t, b.lastEnv)
	require.Equal(t, b.lastEnv.ID, followUp.lastEventID,
		"follow-up dispatcher must receive the interaction.recorded envelope id")
}

// TestHandleEvent_FollowUpDispatcher_PostCommitFolded asserts that a
// non-nil post-commit closure returned by the follow-up dispatcher is
// invoked when the recorder's returned post-commit fires.
func TestHandleEvent_FollowUpDispatcher_PostCommitFolded(t *testing.T) {
	cid := uuid.New()
	rec, _, _, _, _ := newRecorderWithStubs()
	var postFired bool
	followUp := &stubFollowUpDispatcher{postCommit: func(context.Context) { postFired = true }}
	rec.followUp = followUp

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "outbound",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.NotNil(t, postCommit, "non-nil follow-up post-commit must surface in recorder post-commit")
	require.False(t, postFired, "post-commit must not run inside HandleEvent")
	postCommit(context.Background())
	require.True(t, postFired)
}

// TestHandleEvent_NoFollowUpOrPostCommit_NilReturned asserts that
// without either a non-bus FollowUpFn or a bus-path follow-up post-
// commit, the recorder returns nil post-commit (no empty wrapper).
func TestHandleEvent_NoFollowUpOrPostCommit_NilReturned(t *testing.T) {
	cid := uuid.New()
	rec, w, _, _, _ := newRecorderWithStubs()
	w.postCommit = nil
	rec.followUp = nil

	env := mustEnv(t, events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  cid,
		Direction:  "outbound",
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	_, postCommit, err := rec.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Nil(t, postCommit, "no follow-up dispatcher + no post-commit must return nil post-commit")
}

// stubFollowUpDispatcher captures HandleEvent invocations and returns
// the pre-configured post-commit closure.
type stubFollowUpDispatcher struct {
	calls       int
	lastEventID uuid.UUID
	postCommit  func(context.Context)
	err         error
}

func (s *stubFollowUpDispatcher) HandleEvent(_ context.Context, _ pgx.Tx, env *events.Envelope) (func(context.Context), error) {
	s.calls++
	if env != nil {
		s.lastEventID = env.ID
	}
	return s.postCommit, s.err
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
		ContactID:         nil, // simulates a publisher that failed to resolve ContactID before PublishTx.
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
		"interaction.recorded SourceID must equal interaction.ID so the event table's partial unique index dedupes retries")
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
