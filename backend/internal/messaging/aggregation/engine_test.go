package aggregation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- shared test types -------------------------------------------------

// telegramShapedAdapter mirrors the production telegramAdapter exactly so
// shared-package tests can lock in the wire format "tg:<chatID>:<extID>"
// without importing the telegram package (which would create a cycle).
type telegramShapedAdapter struct{}

func (telegramShapedAdapter) SourceName() string {
	return repository.InteractionSourceTelegram
}
func (telegramShapedAdapter) SourceRef(chatID, firstExternalID string) string {
	return "tg:" + chatID + ":" + firstExternalID
}
func (telegramShapedAdapter) SourceRefPrefix(chatID string) string {
	return "tg:" + chatID + ":%"
}
func (telegramShapedAdapter) PeerRef(chatID string) string {
	return "tg:" + chatID
}
func (telegramShapedAdapter) Description(direction string, msgCount int) string {
	label := "exchange"
	switch direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("Telegram %s (%d messages)", label, msgCount)
}

// fakeAdapter is a minimal SourceAdapter used to prove the engine is
// source-neutral (non-telegram naming).
type fakeAdapter struct {
	name string
}

func (a fakeAdapter) SourceName() string { return a.name }
func (a fakeAdapter) SourceRef(chatID, firstExternalID string) string {
	return a.name + ":" + chatID + ":" + firstExternalID
}
func (a fakeAdapter) SourceRefPrefix(chatID string) string { return a.name + ":" + chatID + ":%" }
func (a fakeAdapter) PeerRef(chatID string) string         { return a.name + ":" + chatID }
func (a fakeAdapter) Description(direction string, msgCount int) string {
	return fmt.Sprintf("%s %s (%d messages)", a.name, direction, msgCount)
}

// fakeStore captures the MessageStore protocol in-memory.
type fakeStore struct {
	unprocessedContactIDs []uuid.UUID
	byContact             map[uuid.UUID][]Message
	byContactAndChat      map[string][]Message // key: contactID.String()+"|"+chatID
	byReplyTarget         map[string]Message   // key: chatID+"|"+replyTargetID
	markProcessedCalls    []markProcessedCall
	markProcessedTxCalls  []markProcessedTxCall
	clearStaleClaimCalls  []clearStaleClaimCall
	claimRowsTxCalls      []claimRowsTxCall

	// listUnprocessedByContactErr controls injection points for negative tests.
	listUnprocessedByContactErr error

	// claimRowsTxErr / claimRowsTxFn allow tests to inject claim failure
	// or partial-claim semantics. When both are nil, the default is
	// full-claim success.
	claimRowsTxErr error
	claimRowsTxFn  func(requested []uuid.UUID) ([]uuid.UUID, error)
}

type markProcessedCall struct {
	messageIDs    []uuid.UUID
	interactionID uuid.UUID
}

type markProcessedTxCall struct {
	messageIDs    []uuid.UUID
	interactionID uuid.UUID
	sessionRef    string
}

type clearStaleClaimCall struct {
	messageIDs         []uuid.UUID
	expectedSessionRef string
}

type claimRowsTxCall struct {
	messageIDs []uuid.UUID
	sessionRef string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byContact:        map[uuid.UUID][]Message{},
		byContactAndChat: map[string][]Message{},
		byReplyTarget:    map[string]Message{},
	}
}

func (s *fakeStore) ListUnprocessedContactIDs(_ context.Context) ([]uuid.UUID, error) {
	return s.unprocessedContactIDs, nil
}

func (s *fakeStore) ListUnprocessedByContact(_ context.Context, contactID uuid.UUID) ([]Message, error) {
	if s.listUnprocessedByContactErr != nil {
		return nil, s.listUnprocessedByContactErr
	}
	return s.byContact[contactID], nil
}

func (s *fakeStore) ListUnprocessedByContactAndChat(_ context.Context, contactID uuid.UUID, chatID string) ([]Message, error) {
	return s.byContactAndChat[contactID.String()+"|"+chatID], nil
}

func (s *fakeStore) GetMessageByReplyTarget(_ context.Context, _ uuid.UUID, chatID, replyTargetID string) (Message, bool, error) {
	msg, ok := s.byReplyTarget[chatID+"|"+replyTargetID]
	return msg, ok, nil
}

func (s *fakeStore) MarkProcessed(_ context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	s.markProcessedCalls = append(s.markProcessedCalls, markProcessedCall{
		messageIDs:    append([]uuid.UUID(nil), messageIDs...),
		interactionID: interactionID,
	})
	return nil
}

// ClaimRowsTx default: full claim success (returns the requested IDs).
// Tests can override by replacing the field below or wrapping the store.
// The sessionRef argument is captured so tests can assert the engine
// derived the contact-scoped claim key correctly.
func (s *fakeStore) ClaimRowsTx(_ context.Context, _ pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	s.claimRowsTxCalls = append(s.claimRowsTxCalls, claimRowsTxCall{
		messageIDs: append([]uuid.UUID(nil), messageIDs...),
		sessionRef: sessionRef,
	})
	if s.claimRowsTxErr != nil {
		return nil, s.claimRowsTxErr
	}
	if s.claimRowsTxFn != nil {
		return s.claimRowsTxFn(messageIDs)
	}
	return append([]uuid.UUID(nil), messageIDs...), nil
}

// MarkProcessedTx default: capture and return nil. The session-ref
// parameter is captured so tests can assert the recorder threaded
// env.SourceID through correctly.
func (s *fakeStore) MarkProcessedTx(_ context.Context, _ pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) error {
	s.markProcessedTxCalls = append(s.markProcessedTxCalls, markProcessedTxCall{
		messageIDs:    append([]uuid.UUID(nil), messageIDs...),
		interactionID: interactionID,
		sessionRef:    sessionRef,
	})
	return nil
}

// ClearStaleClaimTx default: capture and return nil.
func (s *fakeStore) ClearStaleClaimTx(_ context.Context, _ pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	s.clearStaleClaimCalls = append(s.clearStaleClaimCalls, clearStaleClaimCall{
		messageIDs:         append([]uuid.UUID(nil), messageIDs...),
		expectedSessionRef: expectedSessionRef,
	})
	return nil
}

// fakeFinder dispenses pre-seeded interactions and tracks calls.
type fakeFinder struct {
	mu                          sync.Mutex
	findRecentBySourceAndDirRet *repository.Interaction
	findRecentBySourceAndDirErr error
	findRecentBySourceAndDirArg findRecentBySourceAndDirCall

	findRecentOutboundRet *repository.Interaction
	findRecentOutboundErr error

	getInteractionRet *repository.Interaction
	getInteractionErr error
}

type findRecentBySourceAndDirCall struct {
	contactID       uuid.UUID
	source          string
	direction       string
	sourceRefPrefix string
}

func (f *fakeFinder) FindRecentBySourceAndDirection(
	_ context.Context,
	contactID uuid.UUID,
	source, direction, sourceRefPrefix string,
	_, _ time.Time,
) (*repository.Interaction, error) {
	f.mu.Lock()
	f.findRecentBySourceAndDirArg = findRecentBySourceAndDirCall{
		contactID:       contactID,
		source:          source,
		direction:       direction,
		sourceRefPrefix: sourceRefPrefix,
	}
	f.mu.Unlock()
	if f.findRecentBySourceAndDirErr != nil {
		return nil, f.findRecentBySourceAndDirErr
	}
	return f.findRecentBySourceAndDirRet, nil
}

func (f *fakeFinder) FindRecentOutboundBySource(
	_ context.Context,
	_ uuid.UUID,
	_, _ string,
	_, _ time.Time,
) (*repository.Interaction, error) {
	if f.findRecentOutboundErr != nil {
		return nil, f.findRecentOutboundErr
	}
	return f.findRecentOutboundRet, nil
}

func (f *fakeFinder) GetInteraction(_ context.Context, _ uuid.UUID) (*repository.Interaction, error) {
	if f.getInteractionErr != nil {
		return nil, f.getInteractionErr
	}
	return f.getInteractionRet, nil
}

type fakePromoter struct {
	mu    sync.Mutex
	calls []promoteCall
}

type promoteCall struct {
	interactionID uuid.UUID
	contactID     uuid.UUID
	replyAt       time.Time
}

func (p *fakePromoter) PromoteInteractionToMutual(_ context.Context, interactionID, contactID uuid.UUID, replyAt time.Time) error {
	p.mu.Lock()
	p.calls = append(p.calls, promoteCall{interactionID, contactID, replyAt})
	p.mu.Unlock()
	return nil
}

type fakeExtender struct {
	mu    sync.Mutex
	calls []extendCall
}

type extendCall struct {
	interactionID uuid.UUID
	contactID     uuid.UUID
	direction     string
	occurredAt    time.Time
	description   string
}

func (e *fakeExtender) ExtendInteraction(_ context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error {
	e.mu.Lock()
	desc := ""
	if description != nil {
		desc = *description
	}
	e.calls = append(e.calls, extendCall{interactionID, contactID, direction, occurredAt, desc})
	e.mu.Unlock()
	return nil
}

type fakePublisher struct {
	mu           sync.Mutex
	envelopes    []events.Envelope
	publishErr   error
	publishTxErr error
}

func (p *fakePublisher) Publish(_ context.Context, env *events.Envelope) error {
	if p.publishErr != nil {
		return p.publishErr
	}
	p.mu.Lock()
	p.envelopes = append(p.envelopes, *env)
	p.mu.Unlock()
	return nil
}

func (p *fakePublisher) PublishTx(_ context.Context, _ pgx.Tx, env *events.Envelope) error {
	if p.publishTxErr != nil {
		return p.publishTxErr
	}
	p.mu.Lock()
	p.envelopes = append(p.envelopes, *env)
	p.mu.Unlock()
	return nil
}

// helper: make a Message with the telegram-shaped fields used by the
// migrated tests (chatID as decimal string, externalID as decimal
// string, no reply target unless overridden).
func makeMsg(extID int32, chatID int64, outgoing bool, sentAt time.Time) Message {
	return Message{
		ID:         uuid.New(),
		ChatID:     strconv.FormatInt(chatID, 10),
		ExternalID: strconv.Itoa(int(extID)),
		IsOutgoing: outgoing,
		SentAt:     sentAt,
	}
}

// --- migrated burst/session tests --------------------------------------

func TestGroupIntoBursts_SingleOutbound(t *testing.T) {
	e := &Engine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []Message{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, true, base.Add(10*time.Minute)),
		makeMsg(3, 100, true, base.Add(30*time.Minute)),
	}

	bursts := e.groupIntoBursts(msgs, "100")
	require.Len(t, bursts, 1)
	assert.Equal(t, repository.InteractionDirectionOutbound, bursts[0].direction)
	assert.Len(t, bursts[0].messages, 3)
}

func TestGroupIntoBursts_SingleInbound(t *testing.T) {
	e := &Engine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []Message{
		makeMsg(1, 100, false, base),
		makeMsg(2, 100, false, base.Add(5*time.Minute)),
	}

	bursts := e.groupIntoBursts(msgs, "100")
	require.Len(t, bursts, 1)
	assert.Equal(t, repository.InteractionDirectionInbound, bursts[0].direction)
}

func TestGroupIntoBursts_SplitByGap(t *testing.T) {
	e := &Engine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []Message{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, true, base.Add(3*time.Hour)),
	}

	bursts := e.groupIntoBursts(msgs, "100")
	require.Len(t, bursts, 2)
}

func TestGroupIntoBursts_DirectionChangeSplits(t *testing.T) {
	e := &Engine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []Message{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, false, base.Add(5*time.Minute)),
	}

	bursts := e.groupIntoBursts(msgs, "100")
	require.Len(t, bursts, 2)
	assert.Equal(t, repository.InteractionDirectionOutbound, bursts[0].direction)
	assert.Equal(t, repository.InteractionDirectionInbound, bursts[1].direction)
}

func TestResolveSessions_ReplyBridgeWithin48h(t *testing.T) {
	e := &Engine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages: []Message{
				makeMsg(1, 100, true, base),
				makeMsg(2, 100, true, base.Add(5*time.Minute)),
			},
			chatID: "100",
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages: []Message{
				makeMsg(3, 100, false, base.Add(1*time.Hour)),
			},
			chatID: "100",
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 1)
	assert.Equal(t, repository.InteractionDirectionMutual, sessions[0].direction)
	assert.Len(t, sessions[0].messages, 3)
}

func TestResolveSessions_ReplyBridgeExpired(t *testing.T) {
	e := &Engine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []Message{makeMsg(1, 100, true, base)},
			chatID:    "100",
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages:  []Message{makeMsg(2, 100, false, base.Add(49*time.Hour))},
			chatID:    "100",
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2)
}

func TestResolveSessions_ExplicitReplyBridges(t *testing.T) {
	e := &Engine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	replyTo := "1" // ExternalID of msg 1 (outbound burst's first message)
	inbound := makeMsg(2, 100, false, base.Add(72*time.Hour))
	inbound.ReplyTargetID = &replyTo

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []Message{makeMsg(1, 100, true, base)},
			chatID:    "100",
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages:  []Message{inbound},
			chatID:    "100",
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 1)
	assert.Equal(t, repository.InteractionDirectionMutual, sessions[0].direction)
}

func TestSessionKey_Stability_TelegramShape(t *testing.T) {
	adapter := telegramShapedAdapter{}
	sess := msgSession{chatID: "12345", firstExternalID: "50001"}
	assert.Equal(t, "tg:12345:50001", sess.sourceRef(adapter))
}

func TestResolveSessions_ChatScoped(t *testing.T) {
	e := &Engine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []Message{makeMsg(1, 100, true, base)},
			chatID:    "100",
		},
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []Message{makeMsg(2, 200, true, base.Add(5*time.Minute))},
			chatID:    "200",
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2)
}

func TestResolveSessions_CrossChatNoBridge(t *testing.T) {
	e := &Engine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []Message{makeMsg(1, 100, true, base)},
			chatID:    "100",
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages:  []Message{makeMsg(2, 200, false, base.Add(1*time.Hour))},
			chatID:    "200",
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2)
	assert.Equal(t, repository.InteractionDirectionOutbound, sessions[0].direction)
	assert.Equal(t, repository.InteractionDirectionInbound, sessions[1].direction)
}

func TestPartitionByChat(t *testing.T) {
	now := accelerated.GetCurrentTime()
	msgs := []Message{
		makeMsg(1, 100, true, now),
		makeMsg(2, 200, true, now),
		makeMsg(3, 100, false, now),
	}

	result := partitionByChat(msgs)
	assert.Len(t, result["100"], 2)
	assert.Len(t, result["200"], 1)
}

func TestMsgDirection(t *testing.T) {
	assert.Equal(t, repository.InteractionDirectionOutbound, msgDirection(Message{IsOutgoing: true}))
	assert.Equal(t, repository.InteractionDirectionInbound, msgDirection(Message{IsOutgoing: false}))
}

// --- engine wiring tests with fake adapters ----------------------------

// TestEngine_FakeSource_AggregateForContactBatch_CreatesEvents asserts
// the engine emits a message.received envelope with adapter-formatted
// fields when the inbound batch has no existing interaction to bridge
// against.
func TestEngine_FakeSource_AggregateForContactBatch_CreatesEvents(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	msgs := []Message{
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m2", IsOutgoing: false, SentAt: base.Add(5 * time.Minute)},
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m3", IsOutgoing: false, SentAt: base.Add(10 * time.Minute)},
	}
	store.byContact[contactID] = msgs

	publisher := &fakePublisher{}
	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)

	require.NoError(t, engine.AggregateForContactBatch(ctx, contactID))

	require.Len(t, publisher.envelopes, 1)
	env := publisher.envelopes[0]
	assert.Equal(t, "fakesrc", env.Source)
	assert.Equal(t, "fakesrc:chatA:m1:"+contactID.String(), env.SourceID)
	assert.Equal(t, events.KindMessageReceived, env.Kind)
}

// TestEngine_FakeSource_AggregateForContact_TimeBridgePromotesToMutual
// asserts the time-based reply bridge fires when an inbound session
// arrives within the reply-bridge window after an outbound interaction.
func TestEngine_FakeSource_AggregateForContact_TimeBridgePromotesToMutual(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()
	priorOutboundID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	inbound := Message{ID: uuid.New(), ChatID: "chatA", ExternalID: "i1", IsOutgoing: false, SentAt: base}
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	finder := &fakeFinder{
		findRecentOutboundRet: &repository.Interaction{
			ID:        priorOutboundID,
			Source:    "fakesrc",
			Direction: repository.InteractionDirectionOutbound,
		},
		// same-direction inbound coalescing finds nothing, so the
		// extend path doesn't run and the create-path is dead code here.
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}
	promoter := &fakePromoter{}
	extender := &fakeExtender{}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, promoter, extender, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, promoter.calls, 1)
	assert.Equal(t, priorOutboundID, promoter.calls[0].interactionID)
	assert.Empty(t, extender.calls)
	require.Len(t, store.markProcessedCalls, 1)
	assert.Equal(t, priorOutboundID, store.markProcessedCalls[0].interactionID)
}

// TestEngine_FakeSource_AggregateForContact_ExtendPathExtendsSameDirection
// asserts an inbound session with a matching prior inbound interaction
// extends rather than creates.
func TestEngine_FakeSource_AggregateForContact_ExtendPathExtendsSameDirection(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()
	priorInboundID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	inbound := Message{ID: uuid.New(), ChatID: "chatA", ExternalID: "i1", IsOutgoing: false, SentAt: base}
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	finder := &fakeFinder{
		findRecentOutboundErr: db.ErrNotFound, // no outbound to bridge to
		findRecentBySourceAndDirRet: &repository.Interaction{
			ID:        priorInboundID,
			Source:    "fakesrc",
			Direction: repository.InteractionDirectionInbound,
		},
	}
	promoter := &fakePromoter{}
	extender := &fakeExtender{}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, promoter, extender, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, extender.calls, 1)
	assert.Equal(t, priorInboundID, extender.calls[0].interactionID)
	assert.Equal(t, "fakesrc inbound (1 messages)", extender.calls[0].description)
	assert.Empty(t, promoter.calls)
	require.Len(t, store.markProcessedCalls, 1)
	assert.Equal(t, priorInboundID, store.markProcessedCalls[0].interactionID)
	// No event published: extend path consumes the message.
	assert.Empty(t, publisher.envelopes)
}

// TestEngine_FakeSource_ExplicitReplyBridge covers the cross-batch
// explicit reply path: an inbound message has ReplyTargetID pointing to
// a prior outbound message (resolved via GetMessageByReplyTarget),
// whose InteractionID is the existing outbound interaction. The
// engine must call GetInteraction, verify the source/direction, and
// promote.
func TestEngine_FakeSource_ExplicitReplyBridge(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()
	priorOutboundID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()

	replyTarget := "outboundExt"
	inbound := Message{
		ID:            uuid.New(),
		ChatID:        "chatA",
		ExternalID:    "i1",
		IsOutgoing:    false,
		SentAt:        base.Add(100 * time.Hour), // outside reply-bridge window
		ReplyTargetID: &replyTarget,
	}
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	// Referenced outbound message: outgoing + has an InteractionID
	referenced := Message{
		ID:            uuid.New(),
		ChatID:        "chatA",
		ExternalID:    replyTarget,
		IsOutgoing:    true,
		SentAt:        base,
		InteractionID: &priorOutboundID,
	}
	store.byReplyTarget["chatA|"+replyTarget] = referenced

	finder := &fakeFinder{
		// Time-based bridge fails (no outbound in window).
		findRecentOutboundErr: db.ErrNotFound,
		// GetInteraction returns the existing outbound row, owned by the
		// same contact we're aggregating for (the cross-contact guard
		// requires existing.ContactID == contactID).
		getInteractionRet: &repository.Interaction{
			ID:        priorOutboundID,
			ContactID: contactID,
			Source:    "fakesrc",
			Direction: repository.InteractionDirectionOutbound,
		},
	}
	promoter := &fakePromoter{}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, promoter, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, promoter.calls, 1)
	assert.Equal(t, priorOutboundID, promoter.calls[0].interactionID)
	require.Len(t, store.markProcessedCalls, 1)
	assert.Equal(t, priorOutboundID, store.markProcessedCalls[0].interactionID)
	assert.Empty(t, publisher.envelopes)
}

// TestEngine_FakeSource_ExplicitReplyBridge_RejectsOtherContact locks the
// cross-contact guard: if the referenced reply-target resolves to an
// interaction owned by a DIFFERENT contact (as can happen for per-contact
// content stores where a shared address fans out), the engine must NOT promote
// it. Instead it falls through to the create path (publishes a new event).
func TestEngine_FakeSource_ExplicitReplyBridge_RejectsOtherContact(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()
	otherContactID := uuid.New()
	priorOutboundID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()

	replyTarget := "outboundExt"
	inbound := Message{
		ID:            uuid.New(),
		ChatID:        "chatA",
		ExternalID:    "i1",
		IsOutgoing:    false,
		SentAt:        base.Add(100 * time.Hour), // outside time-bridge window
		ReplyTargetID: &replyTarget,
	}
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	referenced := Message{
		ID:            uuid.New(),
		ChatID:        "chatA",
		ExternalID:    replyTarget,
		IsOutgoing:    true,
		SentAt:        base,
		InteractionID: &priorOutboundID,
	}
	store.byReplyTarget["chatA|"+replyTarget] = referenced

	finder := &fakeFinder{
		// Both time-bridge lookups miss so the only path that could promote
		// is the explicit-reply bridge — which must be rejected here.
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
		// The referenced interaction is owned by ANOTHER contact.
		getInteractionRet: &repository.Interaction{
			ID:        priorOutboundID,
			ContactID: otherContactID,
			Source:    "fakesrc",
			Direction: repository.InteractionDirectionOutbound,
		},
	}
	promoter := &fakePromoter{}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, promoter, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	// No promotion of the other contact's interaction.
	assert.Empty(t, promoter.calls, "must NOT promote an interaction owned by a different contact")
	// Falls through to the create path: a fresh event is published.
	require.Len(t, publisher.envelopes, 1, "rejecting the cross-contact bridge falls through to the create path")
}

// TestEngine_FakeSource_NoMatch_PublishesCreateEvent asserts the
// create-path runs when finder + reply target both miss.
func TestEngine_FakeSource_NoMatch_PublishesCreateEvent(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	inbound := Message{ID: uuid.New(), ChatID: "chatA", ExternalID: "i1", IsOutgoing: false, SentAt: base}
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, publisher.envelopes, 1)
	assert.Equal(t, events.KindMessageReceived, publisher.envelopes[0].Kind)
	assert.Equal(t, "fakesrc", publisher.envelopes[0].Source)
	assert.Equal(t, "fakesrc:chatA:i1:"+contactID.String(), publisher.envelopes[0].SourceID)
	// No staging-row mark — the consumer's tx does that in the create path.
	assert.Empty(t, store.markProcessedCalls)
}

// TestEngine_FakeSource_NilPublisher_Errors covers the publisher==nil
// guard inside createInteractionForSession. Calling the private method
// directly (same-package access) keeps the assertion focused — the
// public AggregateForContact swallows this error and returns nil to
// match the pre-refactor behaviour.
func TestEngine_FakeSource_NilPublisher_Errors(t *testing.T) {
	ctx := context.Background()
	contactID := uuid.New()
	sess := msgSession{
		direction:       repository.InteractionDirectionInbound,
		chatID:          "chatA",
		firstExternalID: "i1",
		messages: []Message{
			{ID: uuid.New(), ChatID: "chatA", ExternalID: "i1", IsOutgoing: false, SentAt: accelerated.GetCurrentTime()},
		},
	}

	engine := NewEngine(fakeAdapter{name: "fakesrc"}, newFakeStore(), &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, nil, 2, 48, nil, nil, nil)
	err := engine.createInteractionForSession(ctx, contactID, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publisher")
}

// TestEngine_TelegramShape_SourceRefMatchesLegacyFormat locks the wire
// format "tg:<chatID>:<firstExternalID>" via a telegram-shaped adapter
// in the shared package — proves the adapter contract reproduces the
// pre-refactor source_ref byte-for-byte.
func TestEngine_TelegramShape_SourceRefMatchesLegacyFormat(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := telegramShapedAdapter{}
	store := newFakeStore()
	// chatID and ExternalID mirror what telegram's row-mapping helper
	// produces: strconv.FormatInt(int64) and strconv.Itoa(int32).
	msgs := []Message{
		{ID: uuid.New(), ChatID: "12345", ExternalID: "50001", IsOutgoing: false, SentAt: base},
	}
	store.byContact[contactID] = msgs

	publisher := &fakePublisher{}
	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)

	require.NoError(t, engine.AggregateForContactBatch(ctx, contactID))
	require.Len(t, publisher.envelopes, 1)
	assert.Equal(t, "tg:12345:50001:"+contactID.String(), publisher.envelopes[0].SourceID)
	assert.Equal(t, repository.InteractionSourceTelegram, publisher.envelopes[0].Source)
}

// TestEngine_FakeSource_FinderReceivesAdapterPrefix asserts the
// SourceRefPrefix passed to the finder is exactly what
// adapter.SourceRefPrefix(chatID) produces. Regression guard for the
// "engine bakes its own format" failure mode.
func TestEngine_FakeSource_FinderReceivesAdapterPrefix(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	inbound := Message{ID: uuid.New(), ChatID: "chatA", ExternalID: "i1", IsOutgoing: false, SentAt: base}
	// Use the inbound→same-direction-extend branch so the engine calls
	// FindRecentBySourceAndDirection (which records the prefix).
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{inbound}

	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, &fakePublisher{}, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	finder.mu.Lock()
	got := finder.findRecentBySourceAndDirArg
	finder.mu.Unlock()
	assert.Equal(t, "fakesrc", got.source)
	assert.Equal(t, "fakesrc:chatA:%", got.sourceRefPrefix)
	assert.Equal(t, repository.InteractionDirectionInbound, got.direction)
}

// TestEngine_FakeSource_AggregateAll_SwallowsPerContactErrors covers
// the AggregateAll batch-mode error-swallowing semantic: a contact
// whose ListUnprocessedByContact fails does NOT abort the loop.
func TestEngine_FakeSource_AggregateAll_SwallowsPerContactErrors(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	failingContactID := uuid.New()
	goodContactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	store.unprocessedContactIDs = []uuid.UUID{failingContactID, goodContactID}
	// failingContactID has no entry in byContact, but we want a real
	// error (not just empty result). Use a separate flag.
	store.listUnprocessedByContactErr = nil // start fresh
	store.byContact[goodContactID] = []Message{
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
	}

	// Swap in a store that errors for failingContactID only.
	erroringStore := &perContactErroringStore{
		inner: store,
		errs: map[uuid.UUID]error{
			failingContactID: errors.New("simulated per-contact failure"),
		},
	}

	publisher := &fakePublisher{}
	engine := NewEngine(adapter, erroringStore, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateAll(ctx))
	// goodContactID's batch produced one published event despite the
	// failingContactID error.
	require.Len(t, publisher.envelopes, 1)
}

type perContactErroringStore struct {
	inner *fakeStore
	errs  map[uuid.UUID]error
}

func (s *perContactErroringStore) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return s.inner.ListUnprocessedContactIDs(ctx)
}
func (s *perContactErroringStore) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]Message, error) {
	if err, ok := s.errs[contactID]; ok {
		return nil, err
	}
	return s.inner.ListUnprocessedByContact(ctx, contactID)
}
func (s *perContactErroringStore) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]Message, error) {
	return s.inner.ListUnprocessedByContactAndChat(ctx, contactID, chatID)
}
func (s *perContactErroringStore) GetMessageByReplyTarget(ctx context.Context, contactID uuid.UUID, chatID, replyTargetID string) (Message, bool, error) {
	return s.inner.GetMessageByReplyTarget(ctx, contactID, chatID, replyTargetID)
}
func (s *perContactErroringStore) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return s.inner.MarkProcessed(ctx, messageIDs, interactionID)
}
func (s *perContactErroringStore) ClaimRowsTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	return s.inner.ClaimRowsTx(ctx, tx, messageIDs, sessionRef)
}
func (s *perContactErroringStore) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) error {
	return s.inner.MarkProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}
func (s *perContactErroringStore) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	return s.inner.ClearStaleClaimTx(ctx, tx, messageIDs, expectedSessionRef)
}

// --- claim-mechanism tests --------------------------------------------

// fakeTx records commit/rollback so tests can assert tx lifecycle.
// Embeds pgx.Tx for interface compliance; the methods we don't override
// will nil-deref if called — none of our test paths touch them.
type fakeTx struct {
	pgx.Tx
	commitErr error
	commits   int
	rollbacks int
}

func (t *fakeTx) Commit(_ context.Context) error {
	t.commits++
	return t.commitErr
}

func (t *fakeTx) Rollback(_ context.Context) error {
	t.rollbacks++
	return nil
}

// fakeTxBeginner returns a fresh fakeTx per BeginTx call.
type fakeTxBeginner struct {
	last *fakeTx
	err  error
}

func (b *fakeTxBeginner) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if b.err != nil {
		return nil, b.err
	}
	tx := &fakeTx{}
	b.last = tx
	return tx, nil
}

// fakeEventLookup records calls and dispenses pre-seeded results.
type fakeEventLookup struct {
	calls     int
	found     bool
	eventID   uuid.UUID
	lookupErr error
}

func (f *fakeEventLookup) FindEventBySourceRef(_ context.Context, _, _ string) (uuid.UUID, bool, error) {
	f.calls++
	if f.lookupErr != nil {
		return uuid.Nil, false, f.lookupErr
	}
	return f.eventID, f.found, nil
}

// fakeConsumerEnqueuer records EnqueueInteractionRecorderJob calls.
type fakeConsumerEnqueuer struct {
	calls      int
	lastID     uuid.UUID
	enqueueErr error
}

func (f *fakeConsumerEnqueuer) EnqueueInteractionRecorderJob(_ context.Context, eventID uuid.UUID) error {
	f.calls++
	f.lastID = eventID
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	return nil
}

// TestEngine_ClaimPath_CommitsAtomically asserts the steady-state
// gate-1 path: with TxBeginner wired and a fresh session, the engine
// claims rows, publishes via PublishTx, and commits the tx.
func TestEngine_ClaimPath_CommitsAtomically(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	msgs := []Message{
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
	}
	store.byContactAndChat[contactID.String()+"|chatA"] = msgs

	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}
	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, publisher.envelopes, 1, "PublishTx was invoked")
	require.NotNil(t, beginner.last)
	assert.Equal(t, 1, beginner.last.commits, "tx committed exactly once")
}

// TestEngine_ClaimPath_PartialClaimRollsBack covers the gate-1 race
// guard: when ClaimRowsTx returns a strict subset of the requested
// IDs, the engine rolls back and yields without publishing.
func TestEngine_ClaimPath_PartialClaimRollsBack(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m2", IsOutgoing: false, SentAt: base.Add(time.Minute)},
	}
	// Inject partial-claim: claim returns only the first requested ID.
	store.claimRowsTxFn = func(req []uuid.UUID) ([]uuid.UUID, error) {
		if len(req) == 0 {
			return nil, nil
		}
		return []uuid.UUID{req[0]}, nil
	}

	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}
	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	assert.Empty(t, publisher.envelopes, "PublishTx skipped on partial claim")
	require.NotNil(t, beginner.last)
	assert.Equal(t, 0, beginner.last.commits, "tx never committed")
	assert.GreaterOrEqual(t, beginner.last.rollbacks, 1, "tx rolled back")
}

// TestEngine_StaleRecovery_LooksUpEventAndEnqueues exercises gate 2:
// when at least one row's ClaimedSessionRef matches the computed
// contact-scoped claim key, the engine consults EventLookup and (on
// hit) enqueues an InteractionRecorder job via ConsumerJobEnqueuer —
// without publishing a duplicate event.
func TestEngine_StaleRecovery_LooksUpEventAndEnqueues(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()
	existingEventID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	// The claim key the engine will compute for this session+contact:
	// sourceRef ("fakesrc:chatA:m1") + ":" + contactID.
	staleRef := "fakesrc:chatA:m1:" + contactID.String()
	staleTime := base.Add(-10 * time.Minute)
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{
		{
			ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base,
			ClaimedAt: &staleTime, ClaimedSessionRef: &staleRef,
		},
	}

	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}
	lookup := &fakeEventLookup{found: true, eventID: existingEventID}
	enqueuer := &fakeConsumerEnqueuer{}
	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, lookup, enqueuer)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	assert.Empty(t, publisher.envelopes, "recovery path does NOT re-publish")
	assert.Equal(t, 1, lookup.calls, "engine consulted EventLookup")
	assert.Equal(t, 1, enqueuer.calls, "engine enqueued consumer job")
	assert.Equal(t, existingEventID, enqueuer.lastID, "enqueued against the existing event")
}

// TestEngine_StaleRecovery_NoEventClearsClaim exercises the defensive
// branch: claim_session_ref is non-NULL but FindEventBySource returns
// no row. The engine should clear the stale claim and yield (next pass
// will treat rows as unclaimed).
func TestEngine_StaleRecovery_NoEventClearsClaim(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	// The claim key the engine will compute for this session+contact:
	// sourceRef ("fakesrc:chatA:m1") + ":" + contactID.
	staleRef := "fakesrc:chatA:m1:" + contactID.String()
	staleTime := base.Add(-10 * time.Minute)
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{
		{
			ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base,
			ClaimedAt: &staleTime, ClaimedSessionRef: &staleRef,
		},
	}

	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}
	lookup := &fakeEventLookup{found: false} // defensive case
	enqueuer := &fakeConsumerEnqueuer{}
	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, lookup, enqueuer)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	assert.Empty(t, publisher.envelopes)
	assert.Equal(t, 0, enqueuer.calls, "no enqueue when event not found")
	require.Len(t, store.clearStaleClaimCalls, 1, "engine cleared the stale claim")
	assert.Equal(t, staleRef, store.clearStaleClaimCalls[0].expectedSessionRef)
}

// TestEngine_NoTxBeginnerFallsBackToNonTxPublish covers the
// compatibility path: passing nil TxBeginner keeps the legacy
// non-tx Publish behavior so existing tests (and any future
// no-DB modes) continue to work.
func TestEngine_NoTxBeginnerFallsBackToNonTxPublish(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	store.byContactAndChat[contactID.String()+"|chatA"] = []Message{
		{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
	}
	publisher := &fakePublisher{}
	finder := &fakeFinder{
		findRecentOutboundErr:       db.ErrNotFound,
		findRecentBySourceAndDirErr: db.ErrNotFound,
	}

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, nil, nil, nil)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))
	require.Len(t, publisher.envelopes, 1, "non-tx Publish path produced one envelope")
}

// TestCreateInteractionForSession_ClaimAndEventKeyAreContactScoped is the
// direct regression test for the fanned-out collision: two different
// contacts aggregating the identical (chatID, firstExternalID) session
// share the same sourceRef but must NOT collide on claim or event
// identity. Asserts the exact key format (sourceRef + ":" +
// contactID.String()) rather than mere inequality — an implementation
// that derives the key from contactID alone (dropping sourceRef) would
// pass an inequality check here while still colliding two different
// sessions of the SAME contact on the event unique key.
func TestCreateInteractionForSession_ClaimAndEventKeyAreContactScoped(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactA := uuid.New()
	contactB := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}

	sourceRef := adapter.SourceRef("chatA", "m1")
	sess := msgSession{
		direction:       repository.InteractionDirectionInbound,
		chatID:          "chatA",
		firstExternalID: "m1",
		messages: []Message{
			{ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base},
		},
	}

	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, nil, nil)

	require.NoError(t, engine.createInteractionForSession(ctx, contactA, sess))
	require.NoError(t, engine.createInteractionForSession(ctx, contactB, sess))

	wantKeyA := sourceRef + ":" + contactA.String()
	wantKeyB := sourceRef + ":" + contactB.String()

	require.Len(t, store.claimRowsTxCalls, 2)
	assert.Equal(t, wantKeyA, store.claimRowsTxCalls[0].sessionRef)
	assert.Equal(t, wantKeyB, store.claimRowsTxCalls[1].sessionRef)

	require.Len(t, publisher.envelopes, 2)
	assert.Equal(t, wantKeyA, publisher.envelopes[0].SourceID)
	assert.Equal(t, wantKeyB, publisher.envelopes[1].SourceID)

	var payloadA, payloadB events.MessageReceivedPayload
	require.NoError(t, events.Unmarshal(&publisher.envelopes[0], &payloadA))
	require.NoError(t, events.Unmarshal(&publisher.envelopes[1], &payloadB))
	assert.Equal(t, sourceRef, payloadA.ExternalMessageID, "interaction.source_ref stays contact-free")
	assert.Equal(t, sourceRef, payloadB.ExternalMessageID, "interaction.source_ref stays contact-free")
}

// TestCreateInteractionForSession_ContactFreeClaimIsBoundaryShift proves
// the backlog self-heals: a staging row still carrying the pre-fix
// contact-free claimed_session_ref must be read as a boundary-shift (the
// old claim is cleared and the rows re-claimed under the new
// contact-scoped key), not as stale-recovery for a completed session —
// which would look up an event, find none matching, and yield without
// publishing, leaving the row stranded forever.
func TestCreateInteractionForSession_ContactFreeClaimIsBoundaryShift(t *testing.T) {
	ctx := context.Background()
	base := accelerated.GetCurrentTime()
	contactID := uuid.New()

	adapter := fakeAdapter{name: "fakesrc"}
	store := newFakeStore()
	publisher := &fakePublisher{}
	beginner := &fakeTxBeginner{}
	lookup := &fakeEventLookup{}        // must not be consulted
	enqueuer := &fakeConsumerEnqueuer{} // must not be called

	sourceRef := adapter.SourceRef("chatA", "m1")
	staleTime := base.Add(-10 * time.Minute)
	sess := msgSession{
		direction:       repository.InteractionDirectionInbound,
		chatID:          "chatA",
		firstExternalID: "m1",
		messages: []Message{
			{
				ID: uuid.New(), ChatID: "chatA", ExternalID: "m1", IsOutgoing: false, SentAt: base,
				ClaimedAt: &staleTime, ClaimedSessionRef: &sourceRef, // pre-fix, contact-free claim
			},
		},
	}

	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48, beginner, lookup, enqueuer)
	require.NoError(t, engine.createInteractionForSession(ctx, contactID, sess))

	assert.Equal(t, 0, lookup.calls, "boundary-shift path must not consult EventLookup")
	assert.Equal(t, 0, enqueuer.calls, "boundary-shift path must not re-enqueue a recovery job")

	require.Len(t, store.clearStaleClaimCalls, 1)
	assert.Equal(t, sourceRef, store.clearStaleClaimCalls[0].expectedSessionRef, "clears the OLD contact-free ref")

	wantKey := sourceRef + ":" + contactID.String()
	require.Len(t, store.claimRowsTxCalls, 1)
	assert.Equal(t, wantKey, store.claimRowsTxCalls[0].sessionRef, "re-claims under the new contact-scoped key")

	require.Len(t, publisher.envelopes, 1, "publishes a fresh event rather than silently yielding")
	assert.Equal(t, wantKey, publisher.envelopes[0].SourceID)
}

// TestSameIDSet covers the partial-claim helper.
func TestSameIDSet(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()

	assert.True(t, sameIDSet([]uuid.UUID{a, b}, []uuid.UUID{b, a}), "order-independent")
	assert.True(t, sameIDSet(nil, nil), "two empty sets are equal")
	assert.True(t, sameIDSet([]uuid.UUID{}, nil), "empty slice == nil slice")
	assert.False(t, sameIDSet([]uuid.UUID{a, b}, []uuid.UUID{a}), "different lengths")
	assert.False(t, sameIDSet([]uuid.UUID{a, b}, []uuid.UUID{a, c}), "different members")
}

// TestSessionIsStaleRecovery locks the per-row recovery-detection
// invariant.
func TestSessionIsStaleRecovery(t *testing.T) {
	ref := "fakesrc:chatA:m1"
	other := "other:ref"
	sess := msgSession{
		messages: []Message{
			{ClaimedSessionRef: &other},
			{ClaimedSessionRef: &ref},
		},
	}
	assert.True(t, sess.isStaleRecovery(ref))
	assert.False(t, sess.isStaleRecovery("missing"))

	sess2 := msgSession{messages: []Message{{}, {}}} // no claims
	assert.False(t, sess2.isStaleRecovery(ref))
}
