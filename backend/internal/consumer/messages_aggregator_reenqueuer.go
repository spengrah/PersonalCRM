package consumer

import (
	"context"
	"strings"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog/log"
)

// messagesChatAwareAggregator captures the chat-aware aggregator
// surface needed by the messages reenqueuer. Concrete is
// *aggregation.Engine.AggregateForContact (chat-scoped). Interface
// keeps the consumer package free of a back-edge to the messages
// adapter package; main.go wires the concrete in via a closure.
type messagesChatAwareAggregator interface {
	AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID string) error
}

// messagesRiverInserter is the narrow surface used to enqueue a
// fresh MessagingAggregateForContactArgs in addition to the chat-
// aware call. Mirrors RiverInteractionRecorderEnqueuer's shape.
type messagesRiverInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// MessagesAggregatorReenqueuer wires the messages aggregation engine
// + River client into the post-Stage-3 reenqueue path. Two actions
// run per call:
//
//  1. Enqueue a fresh MessagingAggregateForContactArgs River job —
//     UniqueOpts dedup against in-flight jobs means a no-op when one
//     is already pending, but a brand-new job fires when the previous
//     worker just finished. Catches rows that landed in OTHER chats
//     for the same contact while Stage 3 was committing.
//
//  2. Synchronously invoke AggregateForContact for the chat just
//     processed — closes the burst-extend window so a follow-up
//     message in the same chat extends the interaction we just
//     created instead of creating a new one.
//
// Both happen best-effort: failures are logged (the interaction has
// already committed; rolling back is not an option).
type MessagesAggregatorReenqueuer struct {
	engine      messagesChatAwareAggregator
	riverClient messagesRiverInserter
	source      string
}

// NewMessagesAggregatorReenqueuer builds the reenqueuer over a
// messages chat-aware aggregator + river client. source is typically
// "messages"; constructor accepts a parameter so tests can override.
func NewMessagesAggregatorReenqueuer(
	engine messagesChatAwareAggregator,
	riverClient messagesRiverInserter,
	source string,
) *MessagesAggregatorReenqueuer {
	if source == "" {
		source = "messages"
	}
	return &MessagesAggregatorReenqueuer{engine: engine, riverClient: riverClient, source: source}
}

// Reenqueue implements AggregatorReenqueuer.
func (r *MessagesAggregatorReenqueuer) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	// Fire a fresh aggregator job up-front. UniqueOpts dedups
	// in-flight; completed jobs do NOT block re-enqueue so this
	// always either coalesces or runs.
	if r.riverClient != nil {
		_, err := r.riverClient.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
			ContactID: contactID,
			Source:    r.source,
		}, &river.InsertOpts{
			UniqueOpts: river.UniqueOpts{ByArgs: true},
		})
		if err != nil {
			log.Warn().
				Err(err).
				Str("source", r.source).
				Str("contact_id", contactID.String()).
				Msg("messages-reenqueuer: enqueue MessagingAggregateForContactArgs failed; continuing with synchronous pass")
		}
	}

	// Chat-aware synchronous pass for the chat just processed.
	chatID, ok := parseMessagesPeerRef(env)
	if !ok {
		log.Warn().
			Str("source", env.Source).
			Str("envelope_kind", string(env.Kind)).
			Msg("messages-reenqueuer: could not parse chat scope from envelope; skipping synchronous pass")
		return nil
	}
	if r.engine == nil {
		log.Warn().
			Str("source", r.source).
			Msg("messages-reenqueuer: no engine wired; skipping synchronous pass")
		return nil
	}
	return r.engine.AggregateForContact(ctx, contactID, chatID)
}

// parseMessagesPeerRef extracts the chat scope from a message.*
// envelope emitted by the messages aggregator. Format:
// "messages:<chat_guid>". Returns (chatID, true) on success; ("", false)
// for malformed / sweeper-sentinel envelopes (which carry no PeerRef).
func parseMessagesPeerRef(env *events.Envelope) (string, bool) {
	var peerRef string
	switch env.Kind {
	case events.KindMessageReceived:
		var p events.MessageReceivedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return "", false
		}
		peerRef = p.PeerRef
	case events.KindMessageSent:
		var p events.MessageSentPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return "", false
		}
		peerRef = p.PeerRef
	default:
		return "", false
	}
	const prefix = "messages:"
	if !strings.HasPrefix(peerRef, prefix) {
		return "", false
	}
	chatID := peerRef[len(prefix):]
	if chatID == "" {
		return "", false
	}
	return chatID, true
}
