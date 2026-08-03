package consumer

import (
	"context"
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
// Stubs for the email consumer's narrow interfaces. A shared *callSeq records
// the global call order across stubs so we can assert the advisory lock is
// taken BEFORE the content read / find (Decision 8).
// -----------------------------------------------------------------------------

type callSeq struct{ order []string }

func (c *callSeq) record(name string) { c.order = append(c.order, name) }

// stubCommsLocator stubs commsLocator. getResult (when non-nil) is returned
// by GetMessageTx; otherwise getErr (default: a fabricated row). markAffected
// defaults to 1.
type stubCommsLocator struct {
	seq *callSeq

	getResult *repository.CommsMessage
	getErr    error

	getCalls    int
	lastGetKey  [2]string // source, externalID
	lastGetCID  uuid.UUID
	returnedRow *repository.CommsMessage // the row handed back by GetMessageTx
	markCalls   int
	lastMarkIDs []uuid.UUID
	lastMarkInt uuid.UUID
	lastMarkRef string
	markAffect  int64
	markErr     error
}

func (s *stubCommsLocator) GetMessageTx(_ context.Context, _ pgx.Tx, source, externalID string, contactID uuid.UUID) (*repository.CommsMessage, error) {
	s.getCalls++
	if s.seq != nil {
		s.seq.record("get")
	}
	s.lastGetKey = [2]string{source, externalID}
	s.lastGetCID = contactID
	if s.getErr != nil {
		return nil, s.getErr
	}
	row := s.getResult
	if row == nil {
		row = &repository.CommsMessage{ID: uuid.New(), Source: source, ExternalID: externalID, MatchedContactID: &contactID}
	}
	s.returnedRow = row
	return row, nil
}

func (s *stubCommsLocator) MarkProcessedTx(_ context.Context, _ pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	s.markCalls++
	if s.seq != nil {
		s.seq.record("mark")
	}
	s.lastMarkIDs = messageIDs
	s.lastMarkInt = interactionID
	s.lastMarkRef = sessionRef
	if s.markErr != nil {
		return 0, s.markErr
	}
	return s.markAffect, nil
}

// stubEmailFinder stubs emailInteractionFinder. found (when non-nil) is
// returned by FindBySourceRefTx; otherwise findErr (default db.ErrNotFound).
type stubEmailFinder struct {
	seq *callSeq

	found   *repository.Interaction
	findErr error
	lockErr error

	lockCalls   int
	lastLockRef string
	findCalls   int
	lastFindKey [2]string // source, sourceRef
	lastFindCID uuid.UUID
}

func (s *stubEmailFinder) AcquireSourceRefLockTx(_ context.Context, _ pgx.Tx, sourceRef string) error {
	s.lockCalls++
	if s.seq != nil {
		s.seq.record("lock")
	}
	s.lastLockRef = sourceRef
	return s.lockErr
}

func (s *stubEmailFinder) FindBySourceRefTx(_ context.Context, _ pgx.Tx, contactID uuid.UUID, source, sourceRef string) (*repository.Interaction, error) {
	s.findCalls++
	if s.seq != nil {
		s.seq.record("find")
	}
	s.lastFindKey = [2]string{source, sourceRef}
	s.lastFindCID = contactID
	if s.found != nil {
		return s.found, nil
	}
	if s.findErr != nil {
		return nil, s.findErr
	}
	return nil, db.ErrNotFound
}

// stubEmailAggregator stubs emailAggregator, recording which method ran +
// the args (forward-only ts assertions read lastExtendTS / lastPromoteTS).
type stubEmailAggregator struct {
	seq *callSeq

	promoteCalls  int
	lastPromoteID uuid.UUID
	lastPromoteTS time.Time
	promoteErr    error

	extendCalls    int
	lastExtendID   uuid.UUID
	lastExtendTS   time.Time
	lastExtendDir  string
	lastExtendDesc *string
	extendErr      error
}

func (s *stubEmailAggregator) PromoteInteractionToMutualTx(_ context.Context, _ pgx.Tx, interactionID, _ uuid.UUID, replyAt time.Time) error {
	s.promoteCalls++
	if s.seq != nil {
		s.seq.record("promote")
	}
	s.lastPromoteID = interactionID
	s.lastPromoteTS = replyAt
	return s.promoteErr
}

func (s *stubEmailAggregator) ExtendInteractionTx(_ context.Context, _ pgx.Tx, interactionID, _ uuid.UUID, direction string, occurredAt time.Time, description *string) error {
	s.extendCalls++
	if s.seq != nil {
		s.seq.record("extend")
	}
	s.lastExtendID = interactionID
	s.lastExtendTS = occurredAt
	s.lastExtendDir = direction
	s.lastExtendDesc = description
	return s.extendErr
}

// -----------------------------------------------------------------------------
// Test fixtures.
// -----------------------------------------------------------------------------

type emailStubs struct {
	writer     *stubWriter
	comms      *stubCommsLocator
	finder     *stubEmailFinder
	aggregator *stubEmailAggregator
	bus        *stubBus
	cadence    *stubCadence
	followUp   *stubFollowUpDispatcher
	seq        *callSeq
}

func newEmailConsumerWithStubs() (*EmailInteractionConsumer, *emailStubs) {
	seq := &callSeq{}
	s := &emailStubs{
		writer:     &stubWriter{},
		comms:      &stubCommsLocator{seq: seq, markAffect: 1},
		finder:     &stubEmailFinder{seq: seq},
		aggregator: &stubEmailAggregator{seq: seq},
		bus:        &stubBus{},
		cadence:    &stubCadence{},
		followUp:   &stubFollowUpDispatcher{},
		seq:        seq,
	}
	c := NewEmailInteractionConsumer(s.writer, s.comms, s.finder, s.aggregator, s.bus, s.cadence, s.followUp)
	return c, s
}

var (
	emailDay  = "2026-04-10"
	emailSent = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
)

func mustEmailEnv(t *testing.T, kind events.Kind, p events.EmailEventPayload) *events.Envelope {
	t.Helper()
	raw, err := events.Marshal(kind, p)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceEmail,
		SourceID:   p.ExternalID + ":" + p.ContactID.String(),
		Kind:       kind,
		Payload:    raw,
		ObservedAt: p.SentAt,
	}
}

func basePayload(cid uuid.UUID, direction string) events.EmailEventPayload {
	subject := "Re: lunch"
	return events.EmailEventPayload{
		Version:    1,
		ContactID:  cid,
		ExternalID: "<msg-1@example.test>",
		ThreadID:   "thread-1",
		LocalDay:   emailDay,
		SentAt:     emailSent,
		Direction:  direction,
		Subject:    &subject,
	}
}

func expectedSourceRef(cid uuid.UUID, thread, day string) string {
	return cid.String() + ":" + thread + ":" + day
}

// -----------------------------------------------------------------------------
// Create branch.
// -----------------------------------------------------------------------------

func TestEmailConsumer_Create_Inbound_PublishesAndApplies(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	p := basePayload(cid, repository.InteractionDirectionInbound)
	env := mustEmailEnv(t, events.KindEmailReceived, p)

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	// Create path: writer ran with publishesEvent=true, source=email, the
	// computed source_ref, occurred_at=SentAt, direction passed through.
	require.Equal(t, 1, s.writer.calls)
	require.True(t, s.writer.lastPublishesEvent)
	require.Equal(t, repository.InteractionSourceEmail, s.writer.lastReq.Source)
	require.NotNil(t, s.writer.lastReq.SourceRef)
	require.Equal(t, expectedSourceRef(cid, "thread-1", emailDay), *s.writer.lastReq.SourceRef)
	require.Equal(t, emailSent, s.writer.lastReq.OccurredAt)
	require.Equal(t, repository.InteractionDirectionInbound, s.writer.lastReq.Direction)
	require.Equal(t, p.Subject, s.writer.lastReq.Description)

	// interaction.recorded published once + cadence + follow-up applied inline.
	require.Equal(t, 1, s.bus.publishCalls)
	require.Equal(t, events.KindInteractionRecorded, s.bus.lastEnv.Kind)
	require.Equal(t, repository.InteractionSourceEmail, s.bus.lastEnv.Source)
	require.Equal(t, s.writer.lastCreated.ID.String(), s.bus.lastEnv.SourceID)
	require.Equal(t, 1, s.cadence.calls)
	require.Equal(t, 1, s.followUp.calls)

	// Found-branch primitives NOT touched on create.
	require.Zero(t, s.aggregator.extendCalls)
	require.Zero(t, s.aggregator.promoteCalls)

	// Content row linked to the created interaction.
	require.Equal(t, 1, s.comms.markCalls)
	require.Equal(t, []uuid.UUID{s.comms.returnedRow.ID}, s.comms.lastMarkIDs)
	require.Equal(t, s.writer.lastCreated.ID, s.comms.lastMarkInt)
	require.Equal(t, expectedSourceRef(cid, "thread-1", emailDay), s.comms.lastMarkRef)
}

// -----------------------------------------------------------------------------
// Found branch — same direction → extend; different direction → promote.
// -----------------------------------------------------------------------------

func TestEmailConsumer_Found_SameDirection_Extends(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	s.finder.found = &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: emailSent.Add(-time.Hour), Direction: repository.InteractionDirectionInbound,
	}

	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))
	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Zero(t, s.writer.calls, "found branch never calls RecordInteractionTx")
	require.Zero(t, s.bus.publishCalls, "found branch publishes nothing")
	require.Equal(t, 1, s.aggregator.extendCalls)
	require.Zero(t, s.aggregator.promoteCalls)
	require.Equal(t, s.finder.found.ID, s.aggregator.lastExtendID)
	require.Equal(t, repository.InteractionDirectionInbound, s.aggregator.lastExtendDir)
	// SentAt is later than stored → ts advances to SentAt.
	require.Equal(t, emailSent, s.aggregator.lastExtendTS)
	require.Equal(t, 1, s.comms.markCalls)
	require.Equal(t, s.finder.found.ID, s.comms.lastMarkInt)
}

func TestEmailConsumer_Found_DifferentDirection_Promotes(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	s.finder.found = &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: emailSent.Add(-time.Hour), Direction: repository.InteractionDirectionInbound,
	}

	// Inbound stored, now an outbound event → promote to mutual.
	env := mustEmailEnv(t, events.KindEmailSent, basePayload(cid, repository.InteractionDirectionOutbound))
	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.aggregator.promoteCalls)
	require.Zero(t, s.aggregator.extendCalls)
	require.Equal(t, s.finder.found.ID, s.aggregator.lastPromoteID)
	require.Equal(t, emailSent, s.aggregator.lastPromoteTS)
	require.Zero(t, s.bus.publishCalls)
	require.Equal(t, 1, s.comms.markCalls)
}

// -----------------------------------------------------------------------------
// Forward-only timestamp guard (Decision 4).
// -----------------------------------------------------------------------------

func TestEmailConsumer_ForwardOnly_EarlierSentAt_HoldsStoredTS(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	stored := emailSent // T2
	s.finder.found = &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: stored, Direction: repository.InteractionDirectionInbound,
	}

	// Out-of-order backfill: earlier SentAt (T1 < T2), same direction.
	p := basePayload(cid, repository.InteractionDirectionInbound)
	p.SentAt = stored.Add(-2 * time.Hour)
	p.ExternalID = "<msg-earlier@example.test>"
	env := mustEmailEnv(t, events.KindEmailReceived, p)

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.aggregator.extendCalls)
	require.Equal(t, stored, s.aggregator.lastExtendTS, "occurred_at must NOT move backward")
}

func TestEmailConsumer_ForwardOnly_EarlierMixedDirection_PromotesButHoldsTS(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	stored := emailSent
	s.finder.found = &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: stored, Direction: repository.InteractionDirectionInbound,
	}

	// Earlier outbound backfill → promote but hold ts (direction flips, ts held).
	p := basePayload(cid, repository.InteractionDirectionOutbound)
	p.SentAt = stored.Add(-2 * time.Hour)
	env := mustEmailEnv(t, events.KindEmailSent, p)

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.aggregator.promoteCalls)
	require.Equal(t, stored, s.aggregator.lastPromoteTS, "promote holds stored ts when SentAt is earlier")
}

func TestEmailConsumer_ForwardOnly_EqualSentAt_HoldsStoredTS(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	s.finder.found = &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: emailSent, Direction: repository.InteractionDirectionInbound,
	}
	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.aggregator.extendCalls)
	require.Equal(t, emailSent, s.aggregator.lastExtendTS, "equal SentAt holds the stored value")
}

// -----------------------------------------------------------------------------
// source_ref construction + lookup keys (Decision 5).
// -----------------------------------------------------------------------------

func TestEmailConsumer_SourceRef_BuiltFromLocalDay(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	p := basePayload(cid, repository.InteractionDirectionInbound)
	// SentAt is a different calendar day than LocalDay to prove LocalDay
	// (not SentAt) drives the source_ref.
	p.SentAt = time.Date(2026, 4, 11, 1, 0, 0, 0, time.UTC)
	env := mustEmailEnv(t, events.KindEmailReceived, p)

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	want := expectedSourceRef(cid, "thread-1", emailDay)
	require.Equal(t, want, s.finder.lastLockRef, "lock key = source_ref from LocalDay")
	require.Equal(t, [2]string{repository.InteractionSourceEmail, want}, s.finder.lastFindKey)
	require.Equal(t, cid, s.finder.lastFindCID)
	require.NotNil(t, s.writer.lastReq.SourceRef)
	require.Equal(t, want, *s.writer.lastReq.SourceRef)
}

func TestEmailConsumer_SourceRef_EmptyThread(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	p := basePayload(cid, repository.InteractionDirectionInbound)
	p.ThreadID = ""
	env := mustEmailEnv(t, events.KindEmailReceived, p)

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)
	require.Equal(t, expectedSourceRef(cid, "", emailDay), s.finder.lastLockRef)
}

// -----------------------------------------------------------------------------
// Advisory lock ordering (Decision 8) + content read (Decision 2).
// -----------------------------------------------------------------------------

func TestEmailConsumer_LockTakenBeforeReadAndFind(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.finder.lockCalls)
	require.GreaterOrEqual(t, len(s.seq.order), 3)
	require.Equal(t, "lock", s.seq.order[0], "advisory lock must be acquired first")
	// get + find both come after the lock.
	lockIdx, getIdx, findIdx := indexOf(s.seq.order, "lock"), indexOf(s.seq.order, "get"), indexOf(s.seq.order, "find")
	require.Less(t, lockIdx, getIdx, "lock before content read")
	require.Less(t, lockIdx, findIdx, "lock before find")
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func TestEmailConsumer_GetMessageNotFound_Errors(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	s.comms.getErr = db.ErrNotFound
	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err, "ErrNotFound on content read is error-and-retry, not benign-skip")
	require.Zero(t, s.writer.calls, "no interaction write when content row is missing")
	require.Zero(t, s.aggregator.extendCalls)
	require.Zero(t, s.aggregator.promoteCalls)
	require.Zero(t, s.comms.markCalls)
	// Lock still taken (before the read) — proves the read happens inside the lock.
	require.Equal(t, 1, s.finder.lockCalls)
}

// -----------------------------------------------------------------------------
// res.IsReplay create-branch fall-through (Decision 1 P1 fix).
// -----------------------------------------------------------------------------

func TestEmailConsumer_CreateBranch_IsReplay_FallsThroughToFound(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	// Outer find returns not-found (so we enter the create branch), but the
	// writer reports IsReplay=true with an existing row (a concurrent same-key
	// create slipped past). Must fall through to extend/promote, NOT no-op.
	existing := &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: emailSent.Add(-time.Hour), Direction: repository.InteractionDirectionInbound,
	}
	s.writer.existing = existing

	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))
	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.writer.calls, "RecordInteractionTx was attempted")
	require.Zero(t, s.bus.publishCalls, "no second interaction.recorded on the replay fall-through")
	require.Equal(t, 1, s.aggregator.extendCalls, "fall-through applies the extend (same direction)")
	require.Equal(t, existing.ID, s.aggregator.lastExtendID)
	require.Equal(t, 1, s.comms.markCalls, "content row still linked")
	require.Equal(t, existing.ID, s.comms.lastMarkInt)
}

func TestEmailConsumer_CreateBranch_IsReplay_MixedDirection_Promotes(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	existing := &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceEmail,
		OccurredAt: emailSent.Add(-time.Hour), Direction: repository.InteractionDirectionInbound,
	}
	s.writer.existing = existing

	env := mustEmailEnv(t, events.KindEmailSent, basePayload(cid, repository.InteractionDirectionOutbound))
	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.NoError(t, err)

	require.Equal(t, 1, s.aggregator.promoteCalls, "replay fall-through promotes on direction mismatch")
	require.Zero(t, s.bus.publishCalls)
}

// -----------------------------------------------------------------------------
// Publisher-bug guards (Decision 5).
// -----------------------------------------------------------------------------

func TestEmailConsumer_PublisherBugGuards_Error(t *testing.T) {
	cid := uuid.New()
	cases := []struct {
		name   string
		mutate func(*events.EmailEventPayload)
		kind   events.Kind
	}{
		{"nil contact_id", func(p *events.EmailEventPayload) { p.ContactID = uuid.Nil }, events.KindEmailReceived},
		{"empty external_id", func(p *events.EmailEventPayload) { p.ExternalID = "" }, events.KindEmailReceived},
		{"empty direction", func(p *events.EmailEventPayload) { p.Direction = "" }, events.KindEmailReceived},
		{"invalid direction", func(p *events.EmailEventPayload) { p.Direction = "mutual" }, events.KindEmailReceived},
		{"zero sent_at", func(p *events.EmailEventPayload) { p.SentAt = time.Time{} }, events.KindEmailReceived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := newEmailConsumerWithStubs()
			p := basePayload(cid, repository.InteractionDirectionInbound)
			tc.mutate(&p)
			env := mustEmailEnv(t, tc.kind, p)

			err := c.HandleEvent(context.Background(), nonNilTx(), env)
			require.Error(t, err)
			require.Zero(t, s.finder.lockCalls, "no lock taken on a malformed payload")
			require.Zero(t, s.comms.getCalls)
			require.Zero(t, s.writer.calls)
		})
	}
}

func TestEmailConsumer_NilEnvelope_Errors(t *testing.T) {
	c, _ := newEmailConsumerWithStubs()
	err := c.HandleEvent(context.Background(), nonNilTx(), nil)
	require.Error(t, err)
}

func TestEmailConsumer_NilTx_Errors(t *testing.T) {
	cid := uuid.New()
	c, _ := newEmailConsumerWithStubs()
	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))
	err := c.HandleEvent(context.Background(), nil, env)
	require.Error(t, err)
}

func TestEmailConsumer_WrongKind_Errors(t *testing.T) {
	c, _ := newEmailConsumerWithStubs()
	// Hand the consumer a non-email envelope; it must reject before any work.
	env := &events.Envelope{ID: uuid.New(), Kind: events.KindMessageReceived, Source: "telegram"}
	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
}

// lockErr propagates as an error.
func TestEmailConsumer_LockError_Propagates(t *testing.T) {
	cid := uuid.New()
	c, s := newEmailConsumerWithStubs()
	s.finder.lockErr = errors.New("lock failed")
	env := mustEmailEnv(t, events.KindEmailReceived, basePayload(cid, repository.InteractionDirectionInbound))

	err := c.HandleEvent(context.Background(), nonNilTx(), env)
	require.Error(t, err)
	require.Zero(t, s.comms.getCalls, "no read after a failed lock")
}
