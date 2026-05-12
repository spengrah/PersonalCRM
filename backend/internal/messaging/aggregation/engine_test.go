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

	// listUnprocessedByContactErr controls injection points for negative tests.
	listUnprocessedByContactErr error
}

type markProcessedCall struct {
	messageIDs    []uuid.UUID
	interactionID uuid.UUID
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

func (s *fakeStore) GetMessageByReplyTarget(_ context.Context, chatID, replyTargetID string) (Message, bool, error) {
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
	mu        sync.Mutex
	envelopes []events.Envelope
}

func (p *fakePublisher) Publish(_ context.Context, env *events.Envelope) error {
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
	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48)

	require.NoError(t, engine.AggregateForContactBatch(ctx, contactID))

	require.Len(t, publisher.envelopes, 1)
	env := publisher.envelopes[0]
	assert.Equal(t, "fakesrc", env.Source)
	assert.Equal(t, "fakesrc:chatA:m1", env.SourceID)
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

	engine := NewEngine(adapter, store, finder, promoter, extender, publisher, 2, 48)
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

	engine := NewEngine(adapter, store, finder, promoter, extender, publisher, 2, 48)
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
		// GetInteraction returns the existing outbound row.
		getInteractionRet: &repository.Interaction{
			ID:        priorOutboundID,
			Source:    "fakesrc",
			Direction: repository.InteractionDirectionOutbound,
		},
	}
	promoter := &fakePromoter{}
	publisher := &fakePublisher{}

	engine := NewEngine(adapter, store, finder, promoter, &fakeExtender{}, publisher, 2, 48)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, promoter.calls, 1)
	assert.Equal(t, priorOutboundID, promoter.calls[0].interactionID)
	require.Len(t, store.markProcessedCalls, 1)
	assert.Equal(t, priorOutboundID, store.markProcessedCalls[0].interactionID)
	assert.Empty(t, publisher.envelopes)
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

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48)
	require.NoError(t, engine.AggregateForContact(ctx, contactID, "chatA"))

	require.Len(t, publisher.envelopes, 1)
	assert.Equal(t, events.KindMessageReceived, publisher.envelopes[0].Kind)
	assert.Equal(t, "fakesrc", publisher.envelopes[0].Source)
	assert.Equal(t, "fakesrc:chatA:i1", publisher.envelopes[0].SourceID)
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

	engine := NewEngine(fakeAdapter{name: "fakesrc"}, newFakeStore(), &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, nil, 2, 48)
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
	engine := NewEngine(adapter, store, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48)

	require.NoError(t, engine.AggregateForContactBatch(ctx, contactID))
	require.Len(t, publisher.envelopes, 1)
	assert.Equal(t, "tg:12345:50001", publisher.envelopes[0].SourceID)
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

	engine := NewEngine(adapter, store, finder, &fakePromoter{}, &fakeExtender{}, &fakePublisher{}, 2, 48)
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
	engine := NewEngine(adapter, erroringStore, &fakeFinder{}, &fakePromoter{}, &fakeExtender{}, publisher, 2, 48)
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
func (s *perContactErroringStore) GetMessageByReplyTarget(ctx context.Context, chatID, replyTargetID string) (Message, bool, error) {
	return s.inner.GetMessageByReplyTarget(ctx, chatID, replyTargetID)
}
func (s *perContactErroringStore) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return s.inner.MarkProcessed(ctx, messageIDs, interactionID)
}
