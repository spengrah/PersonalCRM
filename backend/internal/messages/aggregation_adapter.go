package messages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SourceName is the source string written into interaction.source,
// event.source, external_sync_state.source, and the river job args.
// The four MUST agree (spec §3 source naming matrix).
const SourceName = "messages"

// NewAggregationEngine constructs a shared aggregator engine instance
// configured for the messages source. Mirror of telegram's
// NewAggregationEngine — same nil-safety conventions; production
// wiring passes all three of (pool, eventBus, enqueuer).
//
// The returned engine is the chat-aware producer used by both:
//   - the MessagingAggregateForContactWorker (per-chat invocation over
//     the contact's distinct unprocessed chats); and
//   - the MessagesAggregatorReenqueuer (post-Stage-3 re-aggregation
//     for the chat just processed).
func NewAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	messageRepo *repository.MessagesMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter aggregation.InteractionPromoter,
	extender aggregation.InteractionExtender,
	eventBus *events.Bus,
	pool aggregation.TxBeginner,
	enqueuer aggregation.ConsumerJobEnqueuer,
) *aggregation.Engine {
	adapter := messagesAdapter{}
	store := &messagesMessageStoreAdapter{repo: messageRepo}
	finder := &interactionFinderAdapter{repo: interactionRepo}

	// Untyped-nil propagation — see telegram/aggregation.go for the
	// rationale (typed-nil concrete pointer would silently bypass
	// engine guards on publisher == nil).
	var pub aggregation.EventPublisher
	if eventBus != nil {
		pub = eventBus
	}
	var lookup aggregation.EventLookup
	if eventBus != nil {
		lookup = &busEventLookup{bus: eventBus}
	}

	return aggregation.NewEngine(
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
}

// --- adapters --------------------------------------------------------

// messagesAdapter binds source-name + format hooks for the messages
// source. Wire format:
//   - SourceRef:   "messages:<chat_guid>:<first_guid>"
//   - PeerRef:     "messages:<chat_guid>"
//   - Description: "Messages <label> (<n> messages)"
type messagesAdapter struct{}

// SourceName returns the source string written into interaction.source.
func (messagesAdapter) SourceName() string {
	return SourceName
}

// SourceRef formats the deterministic burst sourceRef.
func (messagesAdapter) SourceRef(chatID, firstExternalID string) string {
	return SourceName + ":" + chatID + ":" + firstExternalID
}

// SourceRefPrefix returns the LIKE pattern for scoped "recent
// interaction in this chat" queries.
//
// Apple Messages chat.guid values are opaque strings that empirically
// contain `_` (e.g., "iMessage;-;_chat-uuid_") — `_` and `%` are
// PostgreSQL LIKE wildcards. We escape them with `\` per the
// SourceAdapter contract (PR2). The underlying sqlc queries use
// `LIKE pattern ESCAPE '\'` so the explicit escape character takes
// effect.
//
// The replacer also escapes a literal `\` in the chat ID itself —
// belt-and-suspenders for forward compatibility if Apple ever ships
// a chat.guid with a backslash.
func (messagesAdapter) SourceRefPrefix(chatID string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(chatID)
	return SourceName + ":" + escaped + ":%"
}

// PeerRef formats the event-payload PeerRef field. NOT a LIKE
// pattern — escape is not applied because the consumer's reenqueuer
// parses this field with a literal string strip.
func (messagesAdapter) PeerRef(chatID string) string {
	return SourceName + ":" + chatID
}

// Description formats the human-readable interaction description.
// Label phrasing mirrors Telegram (outreach/response/exchange) so UI
// surfaces stay consistent across sources.
func (messagesAdapter) Description(direction string, msgCount int) string {
	label := "exchange"
	switch direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("Messages %s (%d messages)", label, msgCount)
}

// messagesMessageStoreAdapter projects repository.MessagesMessage rows
// into aggregation.Message. Same pattern as the telegram store
// adapter; lives here (not in repository) so the repository package
// stays free of aggregation-package imports.
type messagesMessageStoreAdapter struct {
	repo *repository.MessagesMessageRepository
}

func (a *messagesMessageStoreAdapter) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.repo.ListUnprocessedContactIDs(ctx)
}

func (a *messagesMessageStoreAdapter) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapMessagesMessage(rows[i])
	}
	return out, nil
}

func (a *messagesMessageStoreAdapter) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContactAndChat(ctx, contactID, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapMessagesMessage(rows[i])
	}
	return out, nil
}

// GetMessageByReplyTarget resolves the message referenced by a reply.
// The messages source uses opaque string guids — no parse step is
// needed (telegram has to parse int64; we don't).
func (a *messagesMessageStoreAdapter) GetMessageByReplyTarget(ctx context.Context, chatID, replyTargetID string) (aggregation.Message, bool, error) {
	row, err := a.repo.GetMessageByReplyTarget(ctx, chatID, replyTargetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return aggregation.Message{}, false, nil
		}
		return aggregation.Message{}, false, err
	}
	return mapMessagesMessage(*row), true, nil
}

func (a *messagesMessageStoreAdapter) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return a.repo.MarkMessagesProcessed(ctx, messageIDs, interactionID)
}

func (a *messagesMessageStoreAdapter) ClaimRowsTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	return a.repo.ClaimMessagesTx(ctx, tx, messageIDs, sessionRef)
}

func (a *messagesMessageStoreAdapter) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	return a.repo.ClearStaleClaimTx(ctx, tx, messageIDs, expectedSessionRef)
}

// mapMessagesMessage projects a repository.MessagesMessage into the
// source-neutral aggregation.Message. Preserving InteractionID is
// critical for cross-batch explicit reply bridging. ClaimedAt /
// ClaimedSessionRef are required for stale-claim / boundary-shift
// recovery detection (spec §3 Race Mechanics).
func mapMessagesMessage(m repository.MessagesMessage) aggregation.Message {
	out := aggregation.Message{
		ID:                m.ID,
		ChatID:            m.ChatGuid,
		IsOutgoing:        m.IsOutgoing,
		SentAt:            m.SentAt,
		ExternalID:        m.Guid,
		InteractionID:     m.InteractionID,
		ClaimedAt:         m.ClaimedAt,
		ClaimedSessionRef: m.ClaimedSessionRef,
	}
	if m.ReplyToGuid != nil {
		s := *m.ReplyToGuid
		out.ReplyTargetID = &s
	}
	return out
}

// busEventLookup adapts *events.Bus to aggregation.EventLookup. Same
// shape as the telegram adapter's busEventLookup.
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
// Same shape as the telegram adapter's interactionFinderAdapter.
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
