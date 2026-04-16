package telegram

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// interactionRecorder creates new interactions. Satisfied by *service.ContactService.
type interactionRecorder interface {
	RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error)
}

// interactionPromoter promotes outbound → mutual. Satisfied by *service.ContactService.
type interactionPromoter interface {
	PromoteInteractionToMutual(ctx context.Context, interactionID, contactID uuid.UUID, replyAt time.Time) error
}

// interactionExtender extends an existing interaction. Satisfied by *service.ContactService.
type interactionExtender interface {
	ExtendInteraction(ctx context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error
}

// AggregationEngine processes raw telegram_message rows into interaction records.
type AggregationEngine struct {
	burstWindowHours int
	replyBridgeHours int
	messageRepo      *repository.TelegramMessageRepository
	interactionRepo  *repository.InteractionRepository
	recorder         interactionRecorder
	promoter         interactionPromoter
	extender         interactionExtender
	// eventBus is the shadow-mode event bus. Nil when
	// EVENT_BUS_INTERACTION_MODE=off; a non-nil bus indicates publish-on-
	// success of createInteractionForSession (plan Decision 6).
	eventBus *events.Bus
}

// NewAggregationEngine creates a new aggregation engine. eventBus may be
// nil; non-nil enables shadow-mode sibling publishes of message.received /
// message.sent alongside the direct-path RecordInteraction.
func NewAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	messageRepo *repository.TelegramMessageRepository,
	interactionRepo *repository.InteractionRepository,
	recorder interactionRecorder,
	promoter interactionPromoter,
	extender interactionExtender,
	eventBus *events.Bus,
) *AggregationEngine {
	return &AggregationEngine{
		burstWindowHours: burstWindowHours,
		replyBridgeHours: replyBridgeHours,
		messageRepo:      messageRepo,
		interactionRepo:  interactionRepo,
		recorder:         recorder,
		promoter:         promoter,
		extender:         extender,
		eventBus:         eventBus,
	}
}

// burst groups consecutive same-direction messages within a time window.
type burst struct {
	direction string // "outbound" or "inbound"
	messages  []repository.TelegramMessage
	chatID    int64
}

func (b burst) firstMsgID() int32      { return b.messages[0].TelegramMessageID }
func (b burst) firstSentAt() time.Time { return b.messages[0].SentAt }

// msgSession is a resolved aggregation unit — may be a single burst or merged bursts.
type msgSession struct {
	direction string // "outbound", "inbound", or "mutual"
	messages  []repository.TelegramMessage
	chatID    int64
	firstMsg  int32 // telegram_message_id of the first message (for source_ref)
}

func (s msgSession) sourceRef() string {
	return fmt.Sprintf("tg:%d:%d", s.chatID, s.firstMsg)
}

func (s msgSession) lastSentAt() time.Time { return s.messages[len(s.messages)-1].SentAt }

func (s msgSession) messageIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(s.messages))
	for i, m := range s.messages {
		ids[i] = m.ID
	}
	return ids
}

func (s msgSession) description() string {
	label := "exchange"
	switch s.direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("Telegram %s (%d messages)", label, len(s.messages))
}

// AggregateAll processes all contacts with unprocessed messages (batch mode).
// Used after backfill. Pre-resolves outbound+inbound into mutual before calling RecordInteraction.
func (e *AggregationEngine) AggregateAll(ctx context.Context) error {
	contactIDs, err := e.messageRepo.ListUnprocessedContactIDs(ctx)
	if err != nil {
		return fmt.Errorf("list unprocessed contact IDs: %w", err)
	}

	if len(contactIDs) == 0 {
		return nil
	}

	interactionsCreated := 0
	for _, contactID := range contactIDs {
		n, err := e.aggregateBatch(ctx, contactID)
		if err != nil {
			log.Warn().Err(err).Str("contact_id", contactID.String()).Msg("telegram: batch aggregation failed for contact")
			continue
		}
		interactionsCreated += n
	}

	log.Info().Int("contacts", len(contactIDs)).Int("interactions", interactionsCreated).Msg("telegram: batch aggregation complete")
	return nil
}

// AggregateForContactBatch processes all unprocessed messages for a single contact in batch mode.
// Used after import/link to aggregate newly-matched messages without scanning all contacts.
func (e *AggregationEngine) AggregateForContactBatch(ctx context.Context, contactID uuid.UUID) error {
	_, err := e.aggregateBatch(ctx, contactID)
	return err
}

// aggregateBatch processes all unprocessed messages for a contact in batch mode.
func (e *AggregationEngine) aggregateBatch(ctx context.Context, contactID uuid.UUID) (int, error) {
	msgs, err := e.messageRepo.ListUnprocessedByContact(ctx, contactID)
	if err != nil {
		return 0, fmt.Errorf("list unprocessed messages: %w", err)
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	// Partition by chat
	chatMsgs := partitionByChat(msgs)
	interactionsCreated := 0

	for chatID, chatMessages := range chatMsgs {
		// Group into bursts
		bursts := e.groupIntoBursts(chatMessages, chatID)

		// Resolve bursts into sessions (merge outbound+inbound into mutual where applicable)
		sessions := e.resolveSessions(bursts)

		// Create interactions for each session
		for _, sess := range sessions {
			if err := e.createInteractionForSession(ctx, contactID, sess); err != nil {
				log.Warn().Err(err).Int64("chat_id", chatID).Msg("telegram: failed to create interaction for session")
				continue
			}
			interactionsCreated++
		}
	}

	return interactionsCreated, nil
}

// AggregateForContact processes unprocessed messages for a specific contact+chat (incremental mode).
func (e *AggregationEngine) AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID int64) error {
	msgs, err := e.messageRepo.ListUnprocessedByContactAndChat(ctx, contactID, chatID)
	if err != nil {
		return fmt.Errorf("list unprocessed messages: %w", err)
	}
	if len(msgs) == 0 {
		return nil
	}

	// Group into bursts
	bursts := e.groupIntoBursts(msgs, chatID)

	// Resolve into sessions (same logic as batch)
	sessions := e.resolveSessions(bursts)

	sourceRefPrefix := fmt.Sprintf("tg:%d:%%", chatID)
	now := msgs[len(msgs)-1].SentAt // use latest message time as upper bound

	for _, sess := range sessions {
		// Check explicit reply bridging first
		if sess.direction == repository.InteractionDirectionInbound || sess.direction == repository.InteractionDirectionMutual {
			if bridged := e.tryExplicitReplyBridge(ctx, contactID, sess); bridged {
				continue
			}
		}

		// Check time-based reply bridging for inbound sessions
		if sess.direction == repository.InteractionDirectionInbound {
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.replyBridgeHours) * time.Hour)
			existing, err := e.interactionRepo.FindRecentOutboundTelegramInteraction(
				ctx, contactID, sourceRefPrefix, windowStart, sess.messages[0].SentAt,
			)
			if err == nil {
				// Promote outbound → mutual
				if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
					log.Warn().Err(err).Msg("telegram: failed to promote interaction to mutual")
				} else {
					if err := e.messageRepo.MarkMessagesProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Msg("telegram: failed to mark messages processed after promotion")
					}
					continue
				}
			}
		}

		// Check same-direction coalescing (burst window)
		// For mutual sessions: check if there's an outbound interaction to promote
		// rather than extend, since ExtendInteraction doesn't update direction.
		if sess.direction == repository.InteractionDirectionMutual {
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.burstWindowHours) * time.Hour)
			existing, err := e.interactionRepo.FindRecentTelegramInteraction(
				ctx, contactID, repository.InteractionDirectionOutbound, sourceRefPrefix, windowStart, now,
			)
			if err == nil {
				// Promote outbound → mutual (updates direction + contact fields)
				if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
					log.Warn().Err(err).Msg("telegram: failed to promote interaction to mutual during coalescing")
				} else {
					if err := e.messageRepo.MarkMessagesProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Msg("telegram: failed to mark messages processed after promotion")
					}
					continue
				}
			}
			// No outbound to promote — fall through to create new mutual interaction
		} else {
			// Same-direction coalescing: extend the existing interaction
			windowStart := sess.messages[0].SentAt.Add(-time.Duration(e.burstWindowHours) * time.Hour)
			existing, err := e.interactionRepo.FindRecentTelegramInteraction(
				ctx, contactID, sess.direction, sourceRefPrefix, windowStart, now,
			)
			if err == nil {
				desc := sess.description()
				if err := e.extender.ExtendInteraction(ctx, existing.ID, contactID, sess.direction, sess.lastSentAt(), &desc); err != nil {
					log.Warn().Err(err).Msg("telegram: failed to extend interaction")
				} else {
					if err := e.messageRepo.MarkMessagesProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
						log.Warn().Err(err).Msg("telegram: failed to mark messages processed after extension")
					}
					continue
				}
			}
		}

		// Create new interaction
		if err := e.createInteractionForSession(ctx, contactID, sess); err != nil {
			log.Warn().Err(err).Int64("chat_id", chatID).Msg("telegram: failed to create interaction for session")
		}
	}

	return nil
}

// tryExplicitReplyBridge checks if any inbound message has reply_to_msg_id pointing
// to an outgoing message with an interaction_id. Bridges regardless of time gap.
func (e *AggregationEngine) tryExplicitReplyBridge(ctx context.Context, contactID uuid.UUID, sess msgSession) bool {
	for _, msg := range sess.messages {
		if msg.ReplyToMsgID == nil {
			continue
		}
		// Resolve the referenced message
		referenced, err := e.messageRepo.GetMessage(ctx, sess.chatID, *msg.ReplyToMsgID)
		if err != nil {
			continue // message not found or error
		}
		// Check if it's outgoing and has an interaction_id
		if !referenced.IsOutgoing || referenced.InteractionID == nil {
			continue
		}
		// Verify the interaction is outbound telegram
		existing, err := e.interactionRepo.GetInteraction(ctx, *referenced.InteractionID)
		if err != nil || existing.Source != repository.InteractionSourceTelegram || existing.Direction != repository.InteractionDirectionOutbound {
			continue
		}
		// Promote to mutual
		if err := e.promoter.PromoteInteractionToMutual(ctx, existing.ID, contactID, sess.lastSentAt()); err != nil {
			log.Warn().Err(err).Msg("telegram: failed to promote interaction via explicit reply")
			continue
		}
		if err := e.messageRepo.MarkMessagesProcessed(ctx, sess.messageIDs(), existing.ID); err != nil {
			log.Warn().Err(err).Msg("telegram: failed to mark messages processed after explicit reply bridge")
		}
		return true
	}
	return false
}

// createInteractionForSession creates a new interaction for a session and marks messages processed.
func (e *AggregationEngine) createInteractionForSession(ctx context.Context, contactID uuid.UUID, sess msgSession) error {
	sourceRef := sess.sourceRef()
	desc := sess.description()

	interaction, err := e.recorder.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:   contactID,
		Source:      repository.InteractionSourceTelegram,
		SourceRef:   &sourceRef,
		OccurredAt:  sess.lastSentAt(),
		Description: &desc,
		Direction:   sess.direction,
	})
	if err != nil {
		return fmt.Errorf("record interaction: %w", err)
	}

	// Shadow-mode sibling publish (plan Decision 6). Failures are logged
	// and discarded — the direct-path write is authoritative. Runs before
	// MarkMessagesProcessed so the event table reflects what produced the
	// interaction row.
	if e.eventBus != nil {
		if pubErr := e.publishForSession(ctx, contactID, sess, sourceRef, &desc); pubErr != nil {
			log.Warn().Err(pubErr).
				Str("contact_id", contactID.String()).
				Str("source_ref", sourceRef).
				Str("direction", sess.direction).
				Msg("telegram: shadow publish failed")
		}
	}

	if err := e.messageRepo.MarkMessagesProcessed(ctx, sess.messageIDs(), interaction.ID); err != nil {
		return fmt.Errorf("mark messages processed: %w", err)
	}

	return nil
}

// publishForSession emits the message.received / message.sent event
// corresponding to a freshly-created telegram interaction. Fresh-mutual
// sessions carry Direction="mutual" in the payload so the consumer can
// write a matching mutual row (plan Decision 6).
//
// Kind selection:
//   - inbound session  → KindMessageReceived
//   - outbound session → KindMessageSent
//   - mutual session   → KindMessageReceived (arbitrary; the payload's
//     Direction field carries the authoritative direction)
//
// SourceID = sourceRef so publisher-idempotency dedupes retries.
func (e *AggregationEngine) publishForSession(
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

	// Build payload. Kind-specific type so events.Marshal validates the
	// kind↔payload pairing.
	var raw []byte
	var marshalErr error
	switch kind {
	case events.KindMessageReceived:
		msg, err := events.Marshal(kind, events.MessageReceivedPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           fmt.Sprintf("tg:%d", sess.chatID),
			MessageAt:         messageAt,
			Description:       description,
			ExternalMessageID: sourceRef,
			Direction:         payloadDirection,
		})
		raw, marshalErr = msg, err
	case events.KindMessageSent:
		msg, err := events.Marshal(kind, events.MessageSentPayload{
			Version:           1,
			ContactID:         &cid,
			PeerRef:           fmt.Sprintf("tg:%d", sess.chatID),
			MessageAt:         messageAt,
			Description:       description,
			ExternalMessageID: sourceRef,
			Direction:         payloadDirection,
		})
		raw, marshalErr = msg, err
	}
	if marshalErr != nil {
		return fmt.Errorf("marshal %s: %w", kind, marshalErr)
	}

	env := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   sourceRef,
		Kind:       kind,
		Payload:    raw,
		ObservedAt: messageAt,
	}
	return e.eventBus.Publish(ctx, env)
}

// groupIntoBursts groups consecutive same-direction messages within the burst window.
func (e *AggregationEngine) groupIntoBursts(msgs []repository.TelegramMessage, chatID int64) []burst {
	if len(msgs) == 0 {
		return nil
	}

	var bursts []burst
	current := burst{
		direction: msgDirection(msgs[0]),
		messages:  []repository.TelegramMessage{msgs[0]},
		chatID:    chatID,
	}

	for i := 1; i < len(msgs); i++ {
		dir := msgDirection(msgs[i])
		gap := msgs[i].SentAt.Sub(msgs[i-1].SentAt)

		if dir != current.direction || gap > time.Duration(e.burstWindowHours)*time.Hour {
			bursts = append(bursts, current)
			current = burst{
				direction: dir,
				messages:  []repository.TelegramMessage{msgs[i]},
				chatID:    chatID,
			}
		} else {
			current.messages = append(current.messages, msgs[i])
		}
	}
	bursts = append(bursts, current)
	return bursts
}

// resolveSessions merges bursts into sessions, applying reply bridging for batch mode.
func (e *AggregationEngine) resolveSessions(bursts []burst) []msgSession {
	if len(bursts) == 0 {
		return nil
	}

	var sessions []msgSession

	for i := 0; i < len(bursts); i++ {
		b := bursts[i]

		// Check if this inbound burst should bridge with the previous outbound session
		if b.direction == repository.InteractionDirectionInbound && len(sessions) > 0 {
			prev := &sessions[len(sessions)-1]
			if prev.direction == repository.InteractionDirectionOutbound && prev.chatID == b.chatID {
				shouldBridge := false

				// Time-based bridging
				gap := b.firstSentAt().Sub(prev.lastSentAt())
				if gap <= time.Duration(e.replyBridgeHours)*time.Hour {
					shouldBridge = true
				}

				// Explicit reply bridging
				if !shouldBridge {
					for _, msg := range b.messages {
						if msg.ReplyToMsgID != nil {
							for _, prevMsg := range prev.messages {
								if prevMsg.TelegramMessageID == *msg.ReplyToMsgID {
									shouldBridge = true
									break
								}
							}
							if shouldBridge {
								break
							}
						}
					}
				}

				if shouldBridge {
					// Merge into mutual session
					prev.direction = repository.InteractionDirectionMutual
					prev.messages = append(prev.messages, b.messages...)
					continue
				}
			}
		}

		sessions = append(sessions, msgSession{
			direction: b.direction,
			messages:  b.messages,
			chatID:    b.chatID,
			firstMsg:  b.firstMsgID(),
		})
	}

	return sessions
}

// partitionByChat groups messages by telegram_chat_id.
func partitionByChat(msgs []repository.TelegramMessage) map[int64][]repository.TelegramMessage {
	result := make(map[int64][]repository.TelegramMessage)
	for _, m := range msgs {
		result[m.TelegramChatID] = append(result[m.TelegramChatID], m)
	}
	return result
}

func msgDirection(msg repository.TelegramMessage) string {
	if msg.IsOutgoing {
		return repository.InteractionDirectionOutbound
	}
	return repository.InteractionDirectionInbound
}
