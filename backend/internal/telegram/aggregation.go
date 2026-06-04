package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// interactionPromoter promotes outbound → mutual. Satisfied by
// *service.ContactService via Go's structural typing. Re-declared here
// for the shim's NewAggregationEngine signature; the shared package
// declares its own equivalent interface (aggregation.InteractionPromoter).
type interactionPromoter interface {
	PromoteInteractionToMutual(ctx context.Context, interactionID, contactID uuid.UUID, replyAt time.Time) error
}

// interactionExtender extends an existing interaction. Satisfied by
// *service.ContactService via Go's structural typing.
type interactionExtender interface {
	ExtendInteraction(ctx context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error
}

// AggregationEngine is the telegram-side facade for the shared
// aggregation engine (backend/internal/messaging/aggregation). The
// exported method signatures match the pre-refactor type byte-for-byte
// so manager.go, handlers.go, rematch.go, integration tests, and
// cmd/crm-api wiring compile unchanged.
type AggregationEngine struct {
	engine *aggregation.Engine
}

// NewAggregationEngine constructs the telegram facade over the shared
// aggregator. eventBus is required when cutover is enabled. Passing a
// nil *events.Bus makes the engine's session-create path return an
// error — equivalent to the off/shadow modes documented in
// EventBusConfig (spec §3.9).
//
// CRITICAL: nil *events.Bus / nil aggregation.TxBeginner /
// nil aggregation.ConsumerJobEnqueuer must be converted to the
// untyped-nil interface value explicitly. Assigning a typed-nil
// pointer to an interface variable produces a non-nil interface, which
// would silently bypass the engine's nil-guards.
//
// pool is the TxBeginner for the engine's atomic claim+publish step.
// Pass nil to fall back to the legacy non-tx publish path (test mode).
//
// enqueuer is the ConsumerJobEnqueuer for stale-claim recovery (River-
// backed in production; nil in tests that don't exercise the recovery
// path).
func NewAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	messageRepo *repository.TelegramMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter interactionPromoter,
	extender interactionExtender,
	eventBus *events.Bus,
	pool aggregation.TxBeginner,
	enqueuer aggregation.ConsumerJobEnqueuer,
) *AggregationEngine {
	adapter := telegramAdapter{}
	store := &telegramMessageStoreAdapter{repo: messageRepo}
	finder := &interactionFinderAdapter{repo: interactionRepo}

	var pub aggregation.EventPublisher
	if eventBus != nil {
		pub = eventBus
	}

	// EventLookup is satisfied by *events.Bus.FindEventBySource via the
	// busEventLookup adapter — declared as a typed nil so the engine
	// guard correctly sees nil when eventBus is nil.
	var lookup aggregation.EventLookup
	if eventBus != nil {
		lookup = &busEventLookup{bus: eventBus}
	}

	eng := aggregation.NewEngine(
		adapter,
		store,
		finder,
		promoter,
		extender,
		pub,
		burstWindowHours,
		replyBridgeHours,
		pool,
		lookup,
		enqueuer,
	)
	return &AggregationEngine{engine: eng}
}

// AggregateAll processes all contacts with unprocessed telegram messages.
func (e *AggregationEngine) AggregateAll(ctx context.Context) error {
	return e.engine.AggregateAll(ctx)
}

// AggregateForContactBatch processes all unprocessed telegram messages
// for a single contact (no extend/bridge — create-path only).
func (e *AggregationEngine) AggregateForContactBatch(ctx context.Context, contactID uuid.UUID) error {
	return e.engine.AggregateForContactBatch(ctx, contactID)
}

// AggregateForContact processes unprocessed messages for a specific
// contact+chat in incremental mode. chatID stays int64 at the telegram
// boundary; the shim stringifies before delegating to the shared
// engine.
func (e *AggregationEngine) AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID int64) error {
	return e.engine.AggregateForContact(ctx, contactID, strconv.FormatInt(chatID, 10))
}

// --- adapters --------------------------------------------------------

// telegramAdapter binds the source-name and formatting hooks for
// Telegram. Wire format ("tg:<chatID>:<firstMsgID>") preserved
// byte-for-byte from the pre-refactor msgSession.sourceRef.
type telegramAdapter struct{}

func (telegramAdapter) SourceName() string {
	return repository.InteractionSourceTelegram
}

func (telegramAdapter) SourceRef(chatID, firstExternalID string) string {
	return "tg:" + chatID + ":" + firstExternalID
}

func (telegramAdapter) SourceRefPrefix(chatID string) string {
	return "tg:" + chatID + ":%"
}

func (telegramAdapter) PeerRef(chatID string) string {
	return "tg:" + chatID
}

func (telegramAdapter) Description(direction string, msgCount int) string {
	label := "exchange"
	switch direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("Telegram %s (%d messages)", label, msgCount)
}

// telegramMessageStoreAdapter wraps *repository.TelegramMessageRepository
// and exposes the source-neutral aggregation.MessageStore surface.
// Maps repository.TelegramMessage rows into aggregation.Message rows
// row-by-row.
type telegramMessageStoreAdapter struct {
	repo *repository.TelegramMessageRepository
}

func (a *telegramMessageStoreAdapter) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.repo.ListUnprocessedContactIDs(ctx)
}

func (a *telegramMessageStoreAdapter) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapTelegramMessage(rows[i])
	}
	return out, nil
}

func (a *telegramMessageStoreAdapter) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]aggregation.Message, error) {
	parsedChat, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		// Telegram chat IDs are int64 in storage; a non-parseable string
		// here would mean a caller passed a non-numeric chat ID. Log and
		// return empty (no unprocessed messages) — equivalent to "no
		// rows" rather than crashing.
		log.Warn().Err(err).Str("chat_id", chatID).Msg("telegram: aggregation chat_id is not int64-parseable; returning no rows")
		return nil, nil
	}
	rows, err := a.repo.ListUnprocessedByContactAndChat(ctx, contactID, parsedChat)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapTelegramMessage(rows[i])
	}
	return out, nil
}

// GetMessageByReplyTarget ignores contactID: telegram_message rows are keyed
// by a globally-unique (chat_id, message_id), so the lookup is already
// contact-correct (one row per message, not per matched contact).
func (a *telegramMessageStoreAdapter) GetMessageByReplyTarget(ctx context.Context, _ uuid.UUID, chatID, replyTargetID string) (aggregation.Message, bool, error) {
	parsedChat, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		log.Warn().Err(err).Str("chat_id", chatID).Msg("telegram: reply-target chat_id is not int64-parseable; treating as not-found")
		return aggregation.Message{}, false, nil
	}
	parsedMsg, err := strconv.Atoi(replyTargetID)
	if err != nil {
		log.Warn().Err(err).Str("reply_target_id", replyTargetID).Msg("telegram: reply-target msg_id is not int32-parseable; treating as not-found")
		return aggregation.Message{}, false, nil
	}
	row, err := a.repo.GetMessage(ctx, parsedChat, int32(parsedMsg))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return aggregation.Message{}, false, nil
		}
		return aggregation.Message{}, false, err
	}
	return mapTelegramMessage(*row), true, nil
}

func (a *telegramMessageStoreAdapter) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return a.repo.MarkMessagesProcessed(ctx, messageIDs, interactionID)
}

func (a *telegramMessageStoreAdapter) ClaimRowsTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	return a.repo.ClaimMessagesTx(ctx, tx, messageIDs, sessionRef)
}

func (a *telegramMessageStoreAdapter) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	return a.repo.ClearStaleClaimTx(ctx, tx, messageIDs, expectedSessionRef)
}

// mapTelegramMessage projects a repository.TelegramMessage into the
// source-neutral aggregation.Message. Preserving InteractionID is
// critical for cross-batch explicit reply bridging. ClaimedAt /
// ClaimedSessionRef are required for stale-claim / boundary-shift
// recovery detection (spec §3 Race Mechanics).
func mapTelegramMessage(m repository.TelegramMessage) aggregation.Message {
	out := aggregation.Message{
		ID:                m.ID,
		ChatID:            strconv.FormatInt(m.TelegramChatID, 10),
		IsOutgoing:        m.IsOutgoing,
		SentAt:            m.SentAt,
		ExternalID:        strconv.Itoa(int(m.TelegramMessageID)),
		InteractionID:     m.InteractionID,
		ClaimedAt:         m.ClaimedAt,
		ClaimedSessionRef: m.ClaimedSessionRef,
	}
	if m.ReplyToMsgID != nil {
		s := strconv.Itoa(int(*m.ReplyToMsgID))
		out.ReplyTargetID = &s
	}
	return out
}

// busEventLookup adapts *events.Bus to the aggregation.EventLookup
// interface. (uuid.Nil, false, nil) on db.ErrNotFound; (id, true, nil)
// on hit; non-nil error on infrastructure failure.
//
// Lives in the telegram package (not the shared aggregation package)
// because the aggregation package must not import `db` or
// `repository`. The same pattern repeats for any future per-source
// shim (messages, whatsapp).
type busEventLookup struct{ bus *events.Bus }

func (l *busEventLookup) FindEventBySourceRef(ctx context.Context, source, sourceID string) (uuid.UUID, bool, error) {
	env, err := l.bus.FindEventBySource(ctx, source, sourceID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return env.ID, true, nil
}

// interactionFinderAdapter wraps *repository.InteractionRepository and
// exposes the source-neutral aggregation.InteractionFinder surface.
// Lives in the telegram package (not the repository package) so the
// repository type stays free of aggregation-package imports.
type interactionFinderAdapter struct {
	repo *repository.InteractionRepository
}

func (a *interactionFinderAdapter) FindRecentBySourceAndDirection(
	ctx context.Context,
	contactID uuid.UUID,
	source, direction, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*repository.Interaction, error) {
	return a.repo.FindRecentInteractionBySourceAndDirection(ctx, contactID, source, direction, sourceRefPrefix, windowStart, windowEnd)
}

func (a *interactionFinderAdapter) FindRecentOutboundBySource(
	ctx context.Context,
	contactID uuid.UUID,
	source, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*repository.Interaction, error) {
	return a.repo.FindRecentOutboundInteractionBySource(ctx, contactID, source, sourceRefPrefix, windowStart, windowEnd)
}

func (a *interactionFinderAdapter) GetInteraction(ctx context.Context, id uuid.UUID) (*repository.Interaction, error) {
	return a.repo.GetInteraction(ctx, id)
}
