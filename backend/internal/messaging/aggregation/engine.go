package aggregation

import (
	"context"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
//
// txBeginner / eventLookup / consumerEnqueuer are nil-safe: when any is
// nil, the engine takes a fall-back path that does not exercise the
// claim mechanism. This keeps PR2's unit-test fakes working unchanged.
// Production wiring passes all three.
type Engine struct {
	adapter   SourceAdapter
	store     MessageStore
	finder    InteractionFinder
	promoter  InteractionPromoter
	extender  InteractionExtender
	publisher EventPublisher

	txBeginner       TxBeginner          // nil → non-tx Publish fallback (test mode)
	eventLookup      EventLookup         // nil → stale-claim recovery degrades to warn-and-yield
	consumerEnqueuer ConsumerJobEnqueuer // nil → stale-claim recovery degrades to warn-and-yield

	burstWindowHours int
	replyBridgeHours int
}

// NewEngine builds a source-parametric aggregation engine.
//
// publisher MUST be the untyped-nil interface when no event bus is
// configured (see EventPublisher doc). The Engine guards on
// publisher == nil inside the session create path; a typed-nil concrete
// pointer would silently bypass that guard.
//
// txBeginner, eventLookup, consumerEnqueuer are optional. When all
// three are nil, the engine takes the pre-PR3 non-tx publish path —
// this keeps the shared-package unit-test fakes working unchanged.
// Production wiring passes all three.
func NewEngine(
	adapter SourceAdapter,
	store MessageStore,
	finder InteractionFinder,
	promoter InteractionPromoter,
	extender InteractionExtender,
	publisher EventPublisher,
	burstWindowHours, replyBridgeHours int,
	txBeginner TxBeginner,
	eventLookup EventLookup,
	consumerEnqueuer ConsumerJobEnqueuer,
) *Engine {
	return &Engine{
		adapter:          adapter,
		store:            store,
		finder:           finder,
		promoter:         promoter,
		extender:         extender,
		publisher:        publisher,
		txBeginner:       txBeginner,
		eventLookup:      eventLookup,
		consumerEnqueuer: consumerEnqueuer,
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
// Three gates (spec §3 Race Mechanics):
//  1. Fresh rows (no ClaimedAt): atomic claim+publish inside a tx.
//  2. Recovery (any row's ClaimedSessionRef == computed sourceRef): an
//     event already exists; look it up and re-enqueue a consumer job
//     against it (do NOT re-publish — event-log dedup would suppress).
//  3. Boundary shift (some row's ClaimedSessionRef differs from the
//     computed sourceRef AND is stale per TTL): clear the stale claim
//     scope, then proceed with the normal fresh-rows claim+publish.
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
	env, err := e.buildEnvelope(contactID, sess, sourceRef, &desc)
	if err != nil {
		return err
	}

	// Fall-back path: when no TxBeginner is wired, take the legacy
	// non-tx publish path. Used by shared-package unit tests and any
	// mode that hasn't opted into the claim mechanism. Tests rely on
	// this to keep the engine_test.go fakes working without DB-shaped
	// tx plumbing.
	if e.txBeginner == nil {
		if pubErr := e.publisher.Publish(ctx, env); pubErr != nil {
			return fmt.Errorf("publish %s interaction event: %w", e.adapter.SourceName(), pubErr)
		}
		return nil
	}

	// Gate 2: recovery pass. Any row whose ClaimedSessionRef matches
	// the just-computed sourceRef tells us the session was previously
	// claimed but Stage 3 never completed. Look up the existing event
	// and re-enqueue a consumer job — re-publishing would be
	// dedup-rejected by the event-log unique index.
	if sess.isStaleRecovery(sourceRef) {
		return e.handleStaleRecovery(ctx, sess, sourceRef)
	}

	// Gate 3: boundary-shift pass. Some row carries a stale
	// ClaimedSessionRef that does NOT match the current sourceRef
	// (e.g. an earlier inbound arrived out-of-order, shifting the
	// session boundary). Clear the stale claim columns scoped to that
	// stale ref so a parallel worker's freshly-claimed row is not
	// clobbered, then fall through to the fresh-rows claim+publish.
	if stale := sess.staleBoundaryShiftRefs(sourceRef); len(stale) > 0 {
		if err := e.clearStaleBoundaryShiftClaims(ctx, sess, stale); err != nil {
			return err
		}
	}

	// Gate 1: fresh rows. Open a tx, conditionally claim rows still
	// eligible, publish atomically. Rolled back on any failure /
	// partial-claim race.
	tx, err := e.txBeginner.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("aggregation: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit per pgx idiom

	requested := sess.messageIDs()
	claimed, err := e.store.ClaimRowsTx(ctx, tx, requested, sourceRef)
	if err != nil {
		return fmt.Errorf("aggregation: claim rows: %w", err)
	}
	if !sameIDSet(claimed, requested) {
		// Partial claim — another worker raced us. Roll back; the next
		// aggregator pass will re-derive sessions from whatever's still
		// eligible.
		log.Info().
			Str("source", e.adapter.SourceName()).
			Str("source_ref", sourceRef).
			Int("requested", len(requested)).
			Int("claimed", len(claimed)).
			Msg("aggregation: partial claim; rolling back and yielding to racing worker")
		return nil
	}

	if err := e.publisher.PublishTx(ctx, tx, env); err != nil {
		return fmt.Errorf("aggregation: publish tx: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("aggregation: commit tx: %w", err)
	}
	return nil
}

// handleStaleRecovery is taken when the session's rows show a prior
// claim for the same sourceRef. Per spec §3, do NOT re-publish (event-
// log dedup would suppress); look up the existing event and re-enqueue
// a consumer job against it. The recovery enqueue is bounded by River
// UniqueOpts so repeated stale passes coalesce into one in-flight job.
func (e *Engine) handleStaleRecovery(ctx context.Context, sess msgSession, sourceRef string) error {
	if e.eventLookup == nil || e.consumerEnqueuer == nil {
		log.Warn().
			Str("source", e.adapter.SourceName()).
			Str("source_ref", sourceRef).
			Msg("aggregation: stale-claim recovery skipped; lookup/enqueuer not wired")
		return nil
	}
	eventID, found, err := e.eventLookup.FindEventBySourceRef(ctx, e.adapter.SourceName(), sourceRef)
	if err != nil {
		return fmt.Errorf("aggregation: event lookup for stale-claim recovery: %w", err)
	}
	if !found {
		// Spec §3 defensive: claim_session_ref exists but no event
		// matches. Treat as unclaimed — clear the stale claim columns
		// and let the next pass re-publish.
		return e.clearStaleClaimAndYield(ctx, sess, sourceRef)
	}
	log.Warn().
		Str("source", e.adapter.SourceName()).
		Str("source_ref", sourceRef).
		Str("event_id", eventID.String()).
		Msg("aggregation: stale-claim recovery — re-enqueuing consumer job")
	if err := e.consumerEnqueuer.EnqueueInteractionRecorderJob(ctx, eventID); err != nil {
		return fmt.Errorf("aggregation: re-enqueue interaction recorder job: %w", err)
	}
	return nil
}

// clearStaleClaimAndYield clears the claim columns for rows whose
// ClaimedSessionRef still equals the supplied stale ref. Used by the
// defensive recovery branch (FindEventBySource returned no row). The
// next aggregator pass will see rows as unclaimed and proceed normally.
func (e *Engine) clearStaleClaimAndYield(ctx context.Context, sess msgSession, staleRef string) error {
	tx, err := e.txBeginner.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("aggregation: begin tx for stale-claim clear: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := e.store.ClearStaleClaimTx(ctx, tx, sess.messageIDs(), staleRef); err != nil {
		return fmt.Errorf("aggregation: clear stale claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("aggregation: commit stale-claim clear: %w", err)
	}
	return nil
}

// clearStaleBoundaryShiftClaims clears claim columns for rows whose
// ClaimedSessionRef equals any of the supplied stale refs (boundary-
// shift case). One tx per stale ref so each ClearStaleClaimTx call has
// a single expectedSessionRef scope.
func (e *Engine) clearStaleBoundaryShiftClaims(ctx context.Context, sess msgSession, staleRefs []string) error {
	ids := sess.messageIDs()
	for _, ref := range staleRefs {
		tx, err := e.txBeginner.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("aggregation: begin tx for boundary-shift clear: %w", err)
		}
		if err := e.store.ClearStaleClaimTx(ctx, tx, ids, ref); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("aggregation: clear boundary-shift claim: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("aggregation: commit boundary-shift clear: %w", err)
		}
		log.Warn().
			Str("source", e.adapter.SourceName()).
			Str("stale_ref", ref).
			Msg("aggregation: cleared stale boundary-shift claim")
	}
	return nil
}

// buildEnvelope formats the message.received / message.sent event for
// a freshly-created interaction. Mutual sessions carry
// Direction="mutual" in the payload so the consumer can write a
// matching mutual row.
//
// Kind selection:
//   - inbound session  → KindMessageReceived (Direction="")
//   - outbound session → KindMessageSent (Direction="")
//   - mutual session   → KindMessageReceived (Direction="mutual")
//
// SourceID = sourceRef so publisher-idempotency dedupes retries.
func (e *Engine) buildEnvelope(
	contactID uuid.UUID,
	sess msgSession,
	sourceRef string,
	description *string,
) (*events.Envelope, error) {
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
		return nil, fmt.Errorf("unexpected session direction %q", sess.direction)
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
		return nil, fmt.Errorf("marshal %s: %w", kind, marshalErr)
	}

	return &events.Envelope{
		Source:     e.adapter.SourceName(),
		SourceID:   sourceRef,
		Kind:       kind,
		Payload:    raw,
		ObservedAt: messageAt,
	}, nil
}

// sameIDSet reports whether a and b contain the same UUIDs
// (order-independent). Used by createInteractionForSession to detect
// partial-claim races.
func sameIDSet(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	m := make(map[uuid.UUID]int, len(a))
	for _, id := range a {
		m[id]++
	}
	for _, id := range b {
		m[id]--
		if m[id] < 0 {
			return false
		}
	}
	for _, c := range m {
		if c != 0 {
			return false
		}
	}
	return true
}
