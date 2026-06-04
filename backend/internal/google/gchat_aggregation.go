package google

import (
	"context"
	"encoding/json"
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

// GChatSourceName is the source string written into interaction.source,
// event.source, and the river job args for the Google Chat source. It MUST
// agree with the interaction_source_check CHECK value and the
// repository.InteractionSourceGChat constant.
const GChatSourceName = repository.InteractionSourceGChat

// replyTargetMetadataKey is the source_metadata key under which an explicit
// reply's target message resource name is stored (spec §5.X.3). No provider
// writes this key in PR 1 (explicit-reply bridging is deferred), so the
// projection's ReplyTargetID is always nil here — but wiring it now means
// PR 2/PR 3 (or a later explicit-reply follow-up) need not touch the adapter.
const replyTargetMetadataKey = "reply_target_external_id"

// NewGChatAggregationEngine constructs a shared aggregator engine configured
// for the Google Chat source over the comms_message table. Mirror of
// messages.NewAggregationEngine — same nil-safety conventions. Returns the
// shared *aggregation.Engine directly (NO facade): GChat's chat ID is the
// opaque space resource name (a string), so the engine's native
// AggregateForContact(ctx, contactID, chatID string) signature already takes a
// string. Production wiring passes all of (pool, eventBus, enqueuer).
func NewGChatAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	commsRepo *repository.CommsMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter aggregation.InteractionPromoter,
	extender aggregation.InteractionExtender,
	eventBus *events.Bus,
	pool aggregation.TxBeginner,
	enqueuer aggregation.ConsumerJobEnqueuer,
) *aggregation.Engine {
	adapter := gchatAdapter{}
	store := &commsMessageStoreAdapter{repo: commsRepo, source: GChatSourceName}
	finder := &interactionFinderAdapter{repo: interactionRepo}

	// Untyped-nil propagation — a typed-nil concrete pointer would silently
	// bypass the engine's publisher == nil guard. See
	// messaging/aggregation/interfaces.go EventPublisher note.
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

// gchatAdapter binds source-name + format hooks for the Google Chat source.
// Wire format:
//   - SourceRef:   "gchat:<space_resource>:<first_message_resource>"
//   - PeerRef:     "gchat:<space_resource>"
//   - Description: "GChat <label> (<n> messages)"
//
// The chat ID is the opaque Chat space resource name (e.g. "spaces/AAAA9876"),
// a string, so this mirrors the Messages adapter (string chat IDs), not
// Telegram (int64).
type gchatAdapter struct{}

// SourceName returns the source string written into interaction.source.
func (gchatAdapter) SourceName() string {
	return GChatSourceName
}

// SourceRef formats the deterministic burst sourceRef.
func (gchatAdapter) SourceRef(chatID, firstExternalID string) string {
	return GChatSourceName + ":" + chatID + ":" + firstExternalID
}

// SourceRefPrefix returns the LIKE pattern for scoped "recent interaction in
// this chat" queries.
//
// Chat space resource names (e.g. "spaces/AAAA9876") do not legitimately
// contain LIKE wildcards (`_`, `%`) today, but the format is opaque and could
// drift. We escape the chat ID UNCONDITIONALLY (spec §5.X.4 defensive) so the
// prefix is correct even if Chat ever ships a `_`/`%` in a resource name. The
// underlying InteractionFinder queries use `LIKE pattern ESCAPE '\'`, so the
// explicit escape takes effect end-to-end. The replacer also escapes a literal
// `\` in the chat ID — belt-and-suspenders for forward compatibility.
func (gchatAdapter) SourceRefPrefix(chatID string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(chatID)
	return GChatSourceName + ":" + escaped + ":%"
}

// PeerRef formats the event-payload PeerRef field. NOT a LIKE pattern — escape
// is not applied because the consumer's reenqueuer parses this field with a
// literal string strip.
func (gchatAdapter) PeerRef(chatID string) string {
	return GChatSourceName + ":" + chatID
}

// Description formats the human-readable interaction description. Label
// phrasing mirrors Telegram/Messages (outreach/response/exchange) so UI
// surfaces stay consistent across sources.
func (gchatAdapter) Description(direction string, msgCount int) string {
	label := "exchange"
	switch direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("GChat %s (%d messages)", label, msgCount)
}

// commsMessageStoreAdapter projects repository.CommsMessage rows into
// aggregation.Message, pinning a source. Lives in the google package (not
// repository) so the repository package stays free of aggregation-package
// imports — the same boundary the telegram/messages adapters honor. The 7
// MessageStore methods delegate to the source-parameterized repo methods,
// passing a.source; the ForSource suffix on the repo methods is erased here.
type commsMessageStoreAdapter struct {
	repo   *repository.CommsMessageRepository
	source string
}

func (a *commsMessageStoreAdapter) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.repo.ListUnprocessedContactIDsForSource(ctx, a.source)
}

func (a *commsMessageStoreAdapter) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContactForSource(ctx, a.source, contactID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapCommsMessage(rows[i])
	}
	return out, nil
}

func (a *commsMessageStoreAdapter) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContactAndChatForSource(ctx, a.source, contactID, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = mapCommsMessage(rows[i])
	}
	return out, nil
}

// GetMessageByReplyTarget resolves the message referenced by a reply. The
// reply target is itself a stored comms_message row, looked up by its own
// external_id within the same (source, chat) scope.
func (a *commsMessageStoreAdapter) GetMessageByReplyTarget(ctx context.Context, chatID, replyTargetID string) (aggregation.Message, bool, error) {
	row, err := a.repo.GetMessageByReplyTargetForSource(ctx, a.source, chatID, replyTargetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return aggregation.Message{}, false, nil
		}
		return aggregation.Message{}, false, err
	}
	return mapCommsMessage(*row), true, nil
}

func (a *commsMessageStoreAdapter) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return a.repo.MarkMessagesProcessed(ctx, messageIDs, interactionID)
}

func (a *commsMessageStoreAdapter) ClaimRowsTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	return a.repo.ClaimMessagesTx(ctx, tx, messageIDs, sessionRef)
}

func (a *commsMessageStoreAdapter) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	return a.repo.ClearStaleClaimTx(ctx, tx, messageIDs, expectedSessionRef)
}

// mapCommsMessage projects a repository.CommsMessage into the source-neutral
// aggregation.Message. ChatID comes from thread_id (the space resource name);
// a nil thread_id defensively yields ChatID == "" (chat sources always write
// it non-null). Preserving InteractionID / ClaimedAt / ClaimedSessionRef is
// required for cross-batch explicit reply bridging and stale-claim /
// boundary-shift recovery. ReplyTargetID is parsed from
// source_metadata.reply_target_external_id when present as a non-empty string.
func mapCommsMessage(m repository.CommsMessage) aggregation.Message {
	out := aggregation.Message{
		ID:                m.ID,
		IsOutgoing:        m.Direction == repository.InteractionDirectionOutbound,
		SentAt:            m.SentAt,
		ExternalID:        m.ExternalID,
		InteractionID:     m.InteractionID,
		ClaimedAt:         m.ClaimedAt,
		ClaimedSessionRef: m.ClaimedSessionRef,
	}
	if m.ThreadID != nil {
		out.ChatID = *m.ThreadID
	}
	if rt := parseReplyTargetID(m.SourceMetadata); rt != "" {
		out.ReplyTargetID = &rt
	}
	return out
}

// parseReplyTargetID extracts source_metadata.reply_target_external_id as a
// non-empty string, or "" when the key is absent, empty, or the wrong type.
func parseReplyTargetID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	v, ok := meta[replyTargetMetadataKey]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// busEventLookup adapts *events.Bus to aggregation.EventLookup. Same shape as
// the messages/telegram adapters' busEventLookup; re-declared package-private
// here so the google package owns its own copy (the repository package stays
// free of aggregation imports).
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

// interactionFinderAdapter wraps *repository.InteractionRepository and exposes
// the source-neutral aggregation.InteractionFinder surface. Same shape as the
// messages/telegram adapters' interactionFinderAdapter.
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
