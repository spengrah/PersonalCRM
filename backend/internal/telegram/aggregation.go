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
// aggregator. eventBus is required in cutover (default post-PR-6).
// Passing a nil *events.Bus makes the engine's session-create path
// return an error — equivalent to the off/shadow modes documented in
// EventBusConfig (spec §3.9).
//
// CRITICAL: a nil *events.Bus is converted to the untyped-nil
// aggregation.EventPublisher interface value explicitly. Assigning a
// typed-nil pointer to an interface variable produces a non-nil
// interface, which would silently bypass the engine's publisher==nil
// guard.
func NewAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	messageRepo *repository.TelegramMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter interactionPromoter,
	extender interactionExtender,
	eventBus *events.Bus,
) *AggregationEngine {
	adapter := telegramAdapter{}
	store := &telegramMessageStoreAdapter{repo: messageRepo}
	finder := &interactionFinderAdapter{repo: interactionRepo}

	var pub aggregation.EventPublisher
	if eventBus != nil {
		pub = eventBus
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

func (a *telegramMessageStoreAdapter) GetMessageByReplyTarget(ctx context.Context, chatID, replyTargetID string) (aggregation.Message, bool, error) {
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

// mapTelegramMessage projects a repository.TelegramMessage into the
// source-neutral aggregation.Message. Preserving InteractionID is
// critical for cross-batch explicit reply bridging.
func mapTelegramMessage(m repository.TelegramMessage) aggregation.Message {
	out := aggregation.Message{
		ID:            m.ID,
		ChatID:        strconv.FormatInt(m.TelegramChatID, 10),
		IsOutgoing:    m.IsOutgoing,
		SentAt:        m.SentAt,
		ExternalID:    strconv.Itoa(int(m.TelegramMessageID)),
		InteractionID: m.InteractionID,
	}
	if m.ReplyToMsgID != nil {
		s := strconv.Itoa(int(*m.ReplyToMsgID))
		out.ReplyTargetID = &s
	}
	return out
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
