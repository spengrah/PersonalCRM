package aggregation

import (
	"context"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// sortBySentAt sorts msgs by SentAt ascending in place. The MessageStore
// contract requires adapters to emit sorted rows; this defensive sort
// absorbs minor adapter drift so the burst/session derivation can
// safely assume chronological adjacency.
func sortBySentAt(msgs []Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].SentAt.Before(msgs[j].SentAt)
	})
}

// Engine processes per-source staging-message rows into interaction
// records. Created per source — concurrent engines for telegram +
// messages + whatsapp share no mutable state.
type Engine struct {
	adapter   SourceAdapter
	store     MessageStore
	finder    InteractionFinder
	promoter  InteractionPromoter
	extender  InteractionExtender
	publisher EventPublisher

	burstWindowHours int
	replyBridgeHours int
}

// NewEngine builds a source-parametric aggregation engine.
//
// publisher MUST be the untyped-nil interface when no event bus is
// configured (see EventPublisher doc). The Engine guards on
// publisher == nil inside the session create path; a typed-nil concrete
// pointer would silently bypass that guard.
func NewEngine(
	adapter SourceAdapter,
	store MessageStore,
	finder InteractionFinder,
	promoter InteractionPromoter,
	extender InteractionExtender,
	publisher EventPublisher,
	burstWindowHours, replyBridgeHours int,
) *Engine {
	return &Engine{
		adapter:          adapter,
		store:            store,
		finder:           finder,
		promoter:         promoter,
		extender:         extender,
		publisher:        publisher,
		burstWindowHours: burstWindowHours,
		replyBridgeHours: replyBridgeHours,
	}
}

// AggregateAll processes all contacts with unprocessed messages
// (batch mode). Used after backfill. Per-contact errors are swallowed
// with a Warn log so a single failing contact does not abort the batch.
func (e *Engine) AggregateAll(ctx context.Context) error {
	contactIDs, err := e.store.ListUnprocessedContactIDs(ctx)
	if err != nil {
		return fmt.Errorf("list unprocessed contact IDs: %w", err)
	}

	if len(contactIDs) == 0 {
		return nil
	}

	source := e.adapter.SourceName()
	interactionsCreated := 0
	for _, contactID := range contactIDs {
		n, err := e.aggregateBatch(ctx, contactID)
		if err != nil {
			log.Warn().
				Err(err).
				Str("source", source).
				Str("contact_id", contactID.String()).
				Msg("aggregation: batch aggregation failed for contact")
			continue
		}
		interactionsCreated += n
	}

	log.Info().
		Str("source", source).
		Int("contacts", len(contactIDs)).
		Int("interactions", interactionsCreated).
		Msg("aggregation: batch aggregation complete")
	return nil
}

// AggregateForContactBatch processes all unprocessed messages for a
// single contact in batch mode. Used after import/link to aggregate
// newly-matched messages without scanning every contact.
func (e *Engine) AggregateForContactBatch(ctx context.Context, contactID uuid.UUID) error {
	_, err := e.aggregateBatch(ctx, contactID)
	return err
}

// aggregateBatch processes all unprocessed messages for a contact in
// batch mode (no extend/bridge — just create-path).
func (e *Engine) aggregateBatch(ctx context.Context, contactID uuid.UUID) (int, error) {
	msgs, err := e.store.ListUnprocessedByContact(ctx, contactID)
	if err != nil {
		return 0, fmt.Errorf("list unprocessed messages: %w", err)
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	chatMsgs := partitionByChat(msgs)
	interactionsCreated := 0

	for chatID, chatMessages := range chatMsgs {
		// Defensive sort: the MessageStore contract requires sorted
		// rows, but partitionByChat preserves input order so a
		// noisy adapter that emits unsorted batches would corrupt
		// bursts here. The sort is in-place and stable.
		sortBySentAt(chatMessages)
		bursts := e.groupIntoBursts(chatMessages, chatID)
		sessions := e.resolveSessions(bursts)

		for _, sess := range sessions {
			if err := e.createInteractionForSession(ctx, contactID, sess); err != nil {
				log.Warn().
					Err(err).
					Str("source", e.adapter.SourceName()).
					Str("chat_id", chatID).
					Msg("aggregation: failed to create interaction for session")
				continue
			}
			interactionsCreated++
		}
	}

	return interactionsCreated, nil
}

// AggregateForContact processes unprocessed messages for a specific
// contact+chat (incremental mode). Tries explicit reply bridge →
// time-based reply bridge (inbound only) → same-direction coalescing
// → create.
func (e *Engine) AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID string) error {
	msgs, err := e.store.ListUnprocessedByContactAndChat(ctx, contactID, chatID)
	if err != nil {
		return fmt.Errorf("list unprocessed messages: %w", err)
	}
	if len(msgs) == 0 {
		return nil
	}

	// Defensive sort: MessageStore contract requires sorted rows; we
	// re-sort to absorb adapter drift before deriving bursts/sessions
	// that depend on chronological adjacency.
	sortBySentAt(msgs)

	bursts := e.groupIntoBursts(msgs, chatID)
	sessions := e.resolveSessions(bursts)

	source := e.adapter.SourceName()
	sourceRefPrefix := e.adapter.SourceRefPrefix(chatID)
	// Use latest message time as upper bound — matches the pre-refactor
	// telegram behaviour at aggregation.go:198.
	now := msgs[len(msgs)-1].SentAt

	for _, sess := range sessions {
		// Explicit reply bridging first (inbound/mutual sessions).
		if sess.direction == repository.InteractionDirectionInbound || sess.direction == repository.InteractionDirectionMutual {
			if bridged := e.tryExplicitReplyBridge(ctx, contactID, sess); bridged {
				continue
			}
		}

		// Time-based reply bridging for inbound sessions.
		if sess.direction == repository.InteractionDirectionInbound {
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.replyBridgeHours) * time.Hour)
			existing, err := e.finder.FindRecentOutboundBySource(
				ctx, contactID, source, sourceRefPrefix, windowStart, sess.messages[0].SentAt,
			)
			if err == nil {
				// Promote outbound → mutual.
				if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
					log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to promote interaction to mutual")
				} else {
					if err := e.store.MarkProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to mark messages processed after promotion")
					}
					continue
				}
			}
		}

		// Same-direction coalescing within the burst window.
		// For mutual sessions: check whether an outbound interaction
		// exists to promote rather than extend, because
		// ExtendInteraction does not update direction.
		if sess.direction == repository.InteractionDirectionMutual {
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.burstWindowHours) * time.Hour)
			existing, err := e.finder.FindRecentBySourceAndDirection(
				ctx, contactID, source, repository.InteractionDirectionOutbound, sourceRefPrefix, windowStart, now,
			)
			if err == nil {
				if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
					log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to promote interaction to mutual during coalescing")
				} else {
					if err := e.store.MarkProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to mark messages processed after promotion")
					}
					continue
				}
			}
			// No outbound to promote — fall through to create-path.
		} else {
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.burstWindowHours) * time.Hour)
			existing, err := e.finder.FindRecentBySourceAndDirection(
				ctx, contactID, source, sess.direction, sourceRefPrefix, windowStart, now,
			)
			if err == nil {
				desc := sess.description(e.adapter)
				if err := e.extender.ExtendInteraction(ctx, existing.ID, contactID, sess.direction, sess.lastSentAt(), &desc); err != nil {
					log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to extend interaction")
				} else {
					if err := e.store.MarkProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to mark messages processed after extension")
					}
					continue
				}
			}
		}

		// Create new interaction (via publish).
		if err := e.createInteractionForSession(ctx, contactID, sess); err != nil {
			log.Warn().
				Err(err).
				Str("source", source).
				Str("chat_id", chatID).
				Msg("aggregation: failed to create interaction for session")
		}
	}

	return nil
}

// tryExplicitReplyBridge checks if any inbound message in the session
// has ReplyTargetID pointing to an outgoing message with an existing
// interaction. Bridges regardless of time gap.
func (e *Engine) tryExplicitReplyBridge(ctx context.Context, contactID uuid.UUID, sess msgSession) bool {
	source := e.adapter.SourceName()
	for _, msg := range sess.messages {
		if msg.ReplyTargetID == nil {
			continue
		}
		referenced, ok, err := e.store.GetMessageByReplyTarget(ctx, sess.chatID, *msg.ReplyTargetID)
		if err != nil || !ok {
			continue
		}
		if !referenced.IsOutgoing || referenced.InteractionID == nil {
			continue
		}
		existing, err := e.finder.GetInteraction(ctx, *referenced.InteractionID)
		if err != nil || existing.Source != source || existing.Direction != repository.InteractionDirectionOutbound {
			continue
		}
		if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
			log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to promote interaction via explicit reply")
			continue
		}
		if err := e.store.MarkProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
			log.Warn().Err(err).Str("source", source).Msg("aggregation: failed to mark messages processed after explicit reply bridge")
		}
		return true
	}
	return false
}

// createInteractionForSession publishes a message.received /
// message.sent event for a session. In cutover the async consumer
// handles the interaction-row write inside a single tx that also marks
// the source's staging rows processed.
//
// Returns an error when publisher is the nil interface — mode=off/
// shadow is effectively broken post-cutover and the publisher refuses
// to silently drop the interaction.
func (e *Engine) createInteractionForSession(ctx context.Context, contactID uuid.UUID, sess msgSession) error {
	if e.publisher == nil {
		return fmt.Errorf("aggregation: cutover wiring requires publisher; refusing to drop interaction (mode=off/shadow is post-cutover broken per spec §3.9)")
	}
	sourceRef := sess.sourceRef(e.adapter)
	desc := sess.description(e.adapter)
	if pubErr := e.publishForSession(ctx, contactID, sess, sourceRef, &desc); pubErr != nil {
		return fmt.Errorf("publish %s interaction event: %w", e.adapter.SourceName(), pubErr)
	}
	return nil
}

// publishForSession emits the message.received / message.sent event
// for a freshly-created interaction. Mutual sessions carry
// Direction="mutual" in the payload so the consumer can write a
// matching mutual row.
//
// Kind selection:
//   - inbound session  → KindMessageReceived (Direction="")
//   - outbound session → KindMessageSent (Direction="")
//   - mutual session   → KindMessageReceived (Direction="mutual")
//
// SourceID = sourceRef so publisher-idempotency dedupes retries.
func (e *Engine) publishForSession(
	ctx context.Context,
	contactID uuid.UUID,
	sess msgSession,
	sourceRef string,
	description *string,
) error {
	cid := contactID
	messageAt := sess.lastSentAt()

	var kind events.Kind
	var payloadDirection string
	switch sess.direction {
	case repository.InteractionDirectionOutbound:
		kind = events.KindMessageSent
		payloadDirection = ""
	case repository.InteractionDirectionInbound:
		kind = events.KindMessageReceived
		payloadDirection = ""
	case repository.InteractionDirectionMutual:
		kind = events.KindMessageReceived
		payloadDirection = repository.InteractionDirectionMutual
	default:
		return fmt.Errorf("unexpected session direction %q", sess.direction)
	}

	peerRef := e.adapter.PeerRef(sess.chatID)
	msgIDs := sess.messageIDs()
	var raw []byte
	var marshalErr error
	switch kind {
	case events.KindMessageReceived:
		msg, err := events.Marshal(kind, events.MessageReceivedPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           peerRef,
			MessageAt:         messageAt,
			Description:       description,
			ExternalMessageID: sourceRef,
			Direction:         payloadDirection,
			MessageIDs:        msgIDs,
		})
		raw, marshalErr = msg, err
	case events.KindMessageSent:
		msg, err := events.Marshal(kind, events.MessageSentPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           peerRef,
			MessageAt:         messageAt,
			Description:       description,
			ExternalMessageID: sourceRef,
			Direction:         payloadDirection,
			MessageIDs:        msgIDs,
		})
		raw, marshalErr = msg, err
	}
	if marshalErr != nil {
		return fmt.Errorf("marshal %s: %w", kind, marshalErr)
	}

	env := &events.Envelope{
		Source:     e.adapter.SourceName(),
		SourceID:   sourceRef,
		Kind:       kind,
		Payload:    raw,
		ObservedAt: messageAt,
	}
	return e.publisher.Publish(ctx, env)
}
