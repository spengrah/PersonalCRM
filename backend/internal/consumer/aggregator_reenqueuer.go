package consumer

import (
	"context"
	"strconv"
	"strings"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog/log"
)

// AggregatorReenqueuer dispatches a per-source aggregator pass for the
// (contactID) implied by env. The full envelope is passed through so
// per-source implementations can extract chat scope (e.g. Telegram
// chatID from the payload's PeerRef) and call the chat-aware
// aggregation path — necessary to preserve extend/bridge/coalescing
// semantics for rows arriving in the Stage 2 → Stage 3 window.
//
// Sweeper-synthesized envelopes (no payload PeerRef) fall back to the
// batch path; the post-commit hook always carries a parseable PeerRef.
type AggregatorReenqueuer interface {
	Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error
}

// AggregatorReenqueuerRegistry is a source → AggregatorReenqueuer
// dispatcher. Constructed in main.go with one entry per registered
// source. Unknown sources are a logged warning (not an error) — the
// consumer's interaction has already committed by the time this fires,
// so a missing reenqueuer entry should not roll back work.
type AggregatorReenqueuerRegistry struct {
	entries map[string]AggregatorReenqueuer
}

// NewAggregatorReenqueuerRegistry builds the registry from a source →
// reenqueuer map.
func NewAggregatorReenqueuerRegistry(entries map[string]AggregatorReenqueuer) *AggregatorReenqueuerRegistry {
	return &AggregatorReenqueuerRegistry{entries: entries}
}

// Reenqueue dispatches to the source's reenqueuer entry.
func (r *AggregatorReenqueuerRegistry) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	entry, ok := r.entries[env.Source]
	if !ok {
		log.Warn().
			Str("source", env.Source).
			Str("contact_id", contactID.String()).
			Msg("aggregator-reenqueuer: no entry registered for source; skipping")
		return nil
	}
	return entry.Reenqueue(ctx, env, contactID)
}

// NoopAggregatorReenqueuer is a no-op implementation used by sources
// whose aggregator wiring is not yet live. Returning nil on every call
// lets the registry stay populated without enqueueing real work.
type NoopAggregatorReenqueuer struct{}

// Reenqueue implements AggregatorReenqueuer. Always returns nil.
func (NoopAggregatorReenqueuer) Reenqueue(_ context.Context, _ *events.Envelope, _ uuid.UUID) error {
	return nil
}

// TelegramAggregateForContact captures the subset of the Telegram
// aggregation engine needed by the reenqueuer. Concrete is the
// telegram package's *AggregationEngine. Interface keeps the consumer
// package free of a back-edge to the telegram package; main.go wires
// the concrete in via a closure.
type TelegramAggregateForContact interface {
	AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID int64) error
	AggregateForContactBatch(ctx context.Context, contactID uuid.UUID) error
}

// TelegramAggregatorReenqueuer wraps a Telegram aggregation engine and
// implements AggregatorReenqueuer. Extracts the chat ID from the event
// payload's PeerRef ("tg:<chatID>") to drive the chat-aware
// AggregateForContact path (preserves extend/bridge/coalescing
// semantics for rows arriving in the Stage 2 → Stage 3 window).
//
// Falls back to the batch path when:
//   - the payload cannot be unmarshaled (sentinel envelopes from the
//     sweeper carry no PeerRef);
//   - PeerRef does not parse as int64 (defensive — Telegram chat IDs
//     are always int64).
type TelegramAggregatorReenqueuer struct {
	engine TelegramAggregateForContact
}

// NewTelegramAggregatorReenqueuer builds the Telegram-source
// reenqueuer adapter.
func NewTelegramAggregatorReenqueuer(engine TelegramAggregateForContact) *TelegramAggregatorReenqueuer {
	return &TelegramAggregatorReenqueuer{engine: engine}
}

// Reenqueue implements AggregatorReenqueuer.
func (r *TelegramAggregatorReenqueuer) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	chatID, ok := parseTelegramPeerRef(env)
	if !ok {
		// No parseable chat scope — fall back to the batch path. This
		// path is taken by the periodic sweeper's sentinel envelopes;
		// it loses extend/bridge semantics for those passes but is
		// safe because the sweeper is a catch-up tick, not a specific
		// stranded session.
		return r.engine.AggregateForContactBatch(ctx, contactID)
	}
	return r.engine.AggregateForContact(ctx, contactID, chatID)
}

// parseTelegramPeerRef extracts the int64 chat ID from a Telegram
// message.* envelope. Returns (0, false) when the payload is missing
// or PeerRef does not parse.
func parseTelegramPeerRef(env *events.Envelope) (int64, bool) {
	var peerRef string
	switch env.Kind {
	case events.KindMessageReceived:
		var p events.MessageReceivedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return 0, false
		}
		peerRef = p.PeerRef
	case events.KindMessageSent:
		var p events.MessageSentPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return 0, false
		}
		peerRef = p.PeerRef
	default:
		return 0, false
	}
	// Telegram peer-ref format is "tg:<int64-chatID>".
	const prefix = "tg:"
	if !strings.HasPrefix(peerRef, prefix) {
		return 0, false
	}
	chatID, err := strconv.ParseInt(peerRef[len(prefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return chatID, true
}

// RiverInteractionRecorderEnqueuer implements
// aggregation.ConsumerJobEnqueuer by enqueueing an
// InteractionRecorderJobArgs against the application's River client
// with UniqueOpts{ByArgs: true} so repeated stale-claim recovery
// passes against the same eventID coalesce into one in-flight job.
//
// Mirrors the existing RematchDispatcher uniqueness pattern in
// backend/internal/events/bus.go (ContactID/RematchJobID carry
// `river:"unique"` tags; InsertOpts sets ByArgs: true with default
// ByState). The default ByState excludes `discarded`, so a
// permanently-failing consumer's MaxAttempts exhaustion eventually
// frees a fresh recovery slot.
type RiverInteractionRecorderEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverInteractionRecorderEnqueuer builds the River-backed
// enqueuer. Pass the application's River client; nil disables the
// recovery path (the engine's nil-safe fallback logs a warning and
// yields).
func NewRiverInteractionRecorderEnqueuer(client *river.Client[pgx.Tx]) *RiverInteractionRecorderEnqueuer {
	return &RiverInteractionRecorderEnqueuer{client: client}
}

// EnqueueInteractionRecorderJob implements aggregation.ConsumerJobEnqueuer.
func (e *RiverInteractionRecorderEnqueuer) EnqueueInteractionRecorderJob(ctx context.Context, eventID uuid.UUID) error {
	_, err := e.client.Insert(ctx, consumerjobs.InteractionRecorderJobArgs{EventID: eventID}, &river.InsertOpts{
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			// ByArgs hashes only fields tagged `river:"unique"` on the
			// JobArgs struct — InteractionRecorderJobArgs.EventID. Two
			// recovery enqueues with the same EventID coalesce into one
			// job, including a completed prior job. River's default
			// ByState excludes `discarded`, so a permanently-failing
			// consumer eventually frees a slot.
			ByArgs: true,
		},
	})
	return err
}
