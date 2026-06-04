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

// commsChatAwareAggregator captures the chat-aware aggregator surface needed by
// the comms reenqueuer. Concrete is *aggregation.Engine.AggregateForContact
// (chat-scoped). The interface keeps the consumer package free of a back-edge
// to the google adapter package; main.go wires the concrete engine in.
type commsChatAwareAggregator interface {
	AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID string) error
}

// commsRiverInserter is the narrow surface used to enqueue a fresh
// MessagingAggregateForContactArgs in addition to the chat-aware call.
type commsRiverInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// CommsAggregatorReenqueuer wires a comms_message-backed aggregation engine +
// River client into the post-Stage-3 reenqueue path. It mirrors
// MessagesAggregatorReenqueuer (string chatID) but generalizes the source: the
// PeerRef prefix is `source + ":"`, so the same struct serves any comms-backed
// chat source (gchat now; telegram/messages on later migration). Two actions
// run per call:
//
//  1. Enqueue a fresh MessagingAggregateForContactArgs River job —
//     UniqueOpts dedup makes it a no-op when one is already in-flight, but a
//     brand-new job fires when the previous worker just finished. Catches rows
//     that landed in OTHER chats for the same contact while Stage 3 committed.
//
//  2. Synchronously invoke AggregateForContact for the chat just processed —
//     closes the burst-extend window so a follow-up message in the same chat
//     extends the interaction just created instead of creating a new one.
//
// Both happen best-effort: failures are logged (the interaction has already
// committed; rolling back is not an option).
type CommsAggregatorReenqueuer struct {
	engine      commsChatAwareAggregator
	riverClient commsRiverInserter
	source      string
}

// NewCommsAggregatorReenqueuer builds the reenqueuer over a comms-backed
// chat-aware aggregator + river client. source is the interaction source
// (e.g. "gchat") and drives both the enqueued job's Source field and the
// PeerRef prefix used to parse the chat scope.
func NewCommsAggregatorReenqueuer(
	engine commsChatAwareAggregator,
	riverClient commsRiverInserter,
	source string,
) *CommsAggregatorReenqueuer {
	return &CommsAggregatorReenqueuer{engine: engine, riverClient: riverClient, source: source}
}

// Reenqueue implements AggregatorReenqueuer.
func (r *CommsAggregatorReenqueuer) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	// Fire a fresh aggregator job up-front. The shared uniqueness policy
	// dedups in-flight jobs but intentionally allows a fresh job after a
	// prior one completed.
	if r.riverClient != nil {
		_, err := r.riverClient.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
			ContactID: contactID,
			Source:    r.source,
		}, &river.InsertOpts{
			UniqueOpts: consumerjobs.MessagingAggregateUniqueOpts(),
		})
		if err != nil {
			log.Warn().
				Err(err).
				Str("source", r.source).
				Str("contact_id", contactID.String()).
				Msg("comms-reenqueuer: enqueue MessagingAggregateForContactArgs failed; continuing with synchronous pass")
		}
	}

	// Chat-aware synchronous pass for the chat just processed.
	chatID, ok := parseCommsPeerRef(env, r.source+":")
	if !ok {
		log.Warn().
			Str("source", env.Source).
			Str("envelope_kind", string(env.Kind)).
			Msg("comms-reenqueuer: could not parse chat scope from envelope; skipping synchronous pass")
		return nil
	}
	if r.engine == nil {
		log.Warn().
			Str("source", r.source).
			Msg("comms-reenqueuer: no engine wired; skipping synchronous pass")
		return nil
	}
	return r.engine.AggregateForContact(ctx, contactID, chatID)
}

// parseCommsPeerRef extracts the chat scope from a message.* envelope emitted
// by a comms-backed aggregator. Format: "<prefix><chatID>" where prefix is
// "<source>:" (e.g. "gchat:spaces/AAAA" → "spaces/AAAA"; the chatID retains the
// `/`, which is part of the opaque space resource name, not a delimiter).
// Returns (chatID, true) on success; ("", false) for malformed /
// sweeper-sentinel envelopes (which carry no PeerRef) or a non-message kind.
func parseCommsPeerRef(env *events.Envelope, prefix string) (string, bool) {
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
	if !strings.HasPrefix(peerRef, prefix) {
		return "", false
	}
	chatID := peerRef[len(prefix):]
	if chatID == "" {
		return "", false
	}
	return chatID, true
}
