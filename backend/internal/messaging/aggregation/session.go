package aggregation

import (
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// burst groups consecutive same-direction messages within a time window.
type burst struct {
	direction string // "outbound" or "inbound"
	messages  []Message
	chatID    string
}

func (b burst) firstExternalID() string { return b.messages[0].ExternalID }
func (b burst) firstSentAt() time.Time  { return b.messages[0].SentAt }

// msgSession is a resolved aggregation unit — may be a single burst or
// merged bursts.
type msgSession struct {
	direction       string // "outbound", "inbound", or "mutual"
	messages        []Message
	chatID          string
	firstExternalID string // ExternalID of the first message (for sourceRef)
}

func (s msgSession) lastSentAt() time.Time { return s.messages[len(s.messages)-1].SentAt }

func (s msgSession) messageIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(s.messages))
	for i, m := range s.messages {
		ids[i] = m.ID
	}
	return ids
}

// sourceRef returns the source-specific deterministic source_ref string
// for this session, computed via the adapter so the engine never bakes
// in source-specific formatting.
func (s msgSession) sourceRef(adapter SourceAdapter) string {
	return adapter.SourceRef(s.chatID, s.firstExternalID)
}

// description returns the human-readable session description, also
// per-adapter so each source can label sessions naturally
// ("Telegram outreach (3 messages)", "iMessage exchange (2 messages)").
func (s msgSession) description(adapter SourceAdapter) string {
	return adapter.Description(s.direction, len(s.messages))
}

// groupIntoBursts groups consecutive same-direction messages within the
// burst window.
func (e *Engine) groupIntoBursts(msgs []Message, chatID string) []burst {
	if len(msgs) == 0 {
		return nil
	}

	var bursts []burst
	current := burst{
		direction: msgDirection(msgs[0]),
		messages:  []Message{msgs[0]},
		chatID:    chatID,
	}

	for i := 1; i < len(msgs); i++ {
		dir := msgDirection(msgs[i])
		gap := msgs[i].SentAt.Sub(msgs[i-1].SentAt)

		if dir != current.direction || gap > time.Duration(e.burstWindowHours)*time.Hour {
			bursts = append(bursts, current)
			current = burst{
				direction: dir,
				messages:  []Message{msgs[i]},
				chatID:    chatID,
			}
		} else {
			current.messages = append(current.messages, msgs[i])
		}
	}
	bursts = append(bursts, current)
	return bursts
}

// resolveSessions merges bursts into sessions, applying reply bridging
// for batch mode. Time-based bridging fires when the gap is within the
// configured reply-bridge window; explicit reply bridging compares the
// inbound burst's reply targets against the previous outbound burst's
// external IDs (string equality — opaque per-source IDs).
func (e *Engine) resolveSessions(bursts []burst) []msgSession {
	if len(bursts) == 0 {
		return nil
	}

	var sessions []msgSession

	for i := 0; i < len(bursts); i++ {
		b := bursts[i]

		// Check if this inbound burst should bridge with the previous
		// outbound session.
		if b.direction == repository.InteractionDirectionInbound && len(sessions) > 0 {
			prev := &sessions[len(sessions)-1]
			if prev.direction == repository.InteractionDirectionOutbound && prev.chatID == b.chatID {
				shouldBridge := false

				// Time-based bridging.
				gap := b.firstSentAt().Sub(prev.lastSentAt())
				if gap <= time.Duration(e.replyBridgeHours)*time.Hour {
					shouldBridge = true
				}

				// Explicit reply bridging via opaque external IDs.
				if !shouldBridge {
					for _, msg := range b.messages {
						if msg.ReplyTargetID != nil {
							for _, prevMsg := range prev.messages {
								if prevMsg.ExternalID == *msg.ReplyTargetID {
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
					prev.direction = repository.InteractionDirectionMutual
					prev.messages = append(prev.messages, b.messages...)
					continue
				}
			}
		}

		sessions = append(sessions, msgSession{
			direction:       b.direction,
			messages:        b.messages,
			chatID:          b.chatID,
			firstExternalID: b.firstExternalID(),
		})
	}

	return sessions
}

// partitionByChat groups messages by their (source-neutral) chat ID.
func partitionByChat(msgs []Message) map[string][]Message {
	result := make(map[string][]Message)
	for _, m := range msgs {
		result[m.ChatID] = append(result[m.ChatID], m)
	}
	return result
}

func msgDirection(msg Message) string {
	if msg.IsOutgoing {
		return repository.InteractionDirectionOutbound
	}
	return repository.InteractionDirectionInbound
}
