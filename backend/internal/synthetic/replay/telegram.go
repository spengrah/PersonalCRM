package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/gotd/td/tg"
)

// telegramSelfUserID is the synthetic "me" Telegram user id. Distinct from every
// namespace's peer band ([1e12, 2e12)) so a peer is never mistaken for self.
const telegramSelfUserID int64 = 999_999_999

// telegramPeerMatcherDeps bundles the telegram peer matcher + aggregation engine
// the telegram adapter wires into a real MessageHandler.
type telegramPeerMatcherDeps struct {
	matcher *telegram.PeerMatcher
	engine  *telegram.AggregationEngine
}

// TelegramResult is the settled outcome of a Telegram replay.
type TelegramResult struct {
	ContactID  uuid.UUID
	PeerUserID int64
	Matched    bool // false for MatchUnknown (stranded peer)
}

// ReplayTelegram feeds a synthetic PRIVATE inbound message through the REAL
// MessageHandler.HandleNewMessage with a nil api client (the private path never
// touches it). For MatchSeeded the seeded contact's telegram handle matches the
// peer username → matched interaction. For MatchUnknown the peer is stranded
// (telegram_message.matched_contact_id IS NULL) + a discovery candidate. Group
// chats are handled by ReplayTelegramGroup.
//
// contactID is the seeded contact this message targets (for MatchSeeded). The
// caller must seed it with a telegram method matching spec.MatchHandle.
func (h *Harness) ReplayTelegram(ctx context.Context, contactID uuid.UUID, spec factory.TelegramMessageSpec) (TelegramResult, error) {
	handler := telegram.NewMessageHandler(
		h.telegramRepo,
		repository.NewTelegramChatConfigRepository(h.database.Queries),
		repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool),
		nil, // syncStateID: optional, only used to stamp last-sync timestamps
		telegramSelfUserID,
		h.groupMaxMembers, // irrelevant on the private path
		h.peerMatcher.matcher,
		h.peerMatcher.engine,
	)
	// api stays nil: the private path never touches it.

	// Track this peer for cleanup (by-peer telegram_message delete).
	h.track(func(c *created) { c.addTelegramPeer(spec.PeerUserID) })

	entities, update := BuildPrivateUpdate(spec)

	if err := handler.HandleNewMessage(ctx, entities, update); err != nil {
		return TelegramResult{}, fmt.Errorf("telegram handle message: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		// Stranded peer: settle on the message row existing with a NULL contact.
		predicate := func(ctx context.Context) (bool, error) {
			return h.telegramPeerStranded(ctx, spec.PeerUserID)
		}
		if err := h.Settle(ctx, predicate, ""); err != nil {
			return TelegramResult{}, err
		}
		return TelegramResult{PeerUserID: spec.PeerUserID, Matched: false}, nil
	}

	if err := h.Settle(ctx, h.telegramSettled(spec.PeerUserID, spec.TelegramMessageID), ""); err != nil {
		return TelegramResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceTelegram); err != nil {
		return TelegramResult{}, err
	}
	return TelegramResult{ContactID: contactID, PeerUserID: spec.PeerUserID, Matched: true}, nil
}

// TelegramBatchItem is one private Telegram payload in a batch: the seeded
// contact it targets and the message. PairKey marks the two items of a promotion
// pair (0 = not part of one) — the inbound member is driven in a later
// dependency generation so its outbound's interaction already exists when the
// reply bridge looks for it.
type TelegramBatchItem struct {
	ContactID uuid.UUID
	Spec      factory.TelegramMessageSpec
	PairKey   int
}

// ReplayTelegramBatch drives N private Telegram payloads through ONE
// MessageHandler pass per dependency generation and settles once per generation.
// Items must be in chronological replay order (oldest first); the adapter drives
// them in exactly that order.
//
// Telegram aggregates INLINE inside HandleNewMessage, but aggregation only
// claims rows and publishes an envelope — the interaction is written later by a
// River consumer, and the claimed rows are excluded from subsequent aggregation
// reads for a 5-minute TTL. That is why a promotion pair needs a settle barrier
// between its halves and not merely chronological order.
func (h *Harness) ReplayTelegramBatch(ctx context.Context, items []TelegramBatchItem) (BatchResult, error) {
	const source = "telegram"

	entries := telegramBatchEntries(items)
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}
	before := h.snapshotInteractionIDs(ctx, contactIDs)

	handler := telegram.NewMessageHandler(
		h.telegramRepo,
		repository.NewTelegramChatConfigRepository(h.database.Queries),
		repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool),
		nil, // syncStateID: optional, only used to stamp last-sync timestamps
		telegramSelfUserID,
		h.groupMaxMembers, // irrelevant on the private path
		h.peerMatcher.matcher,
		h.peerMatcher.engine,
	)
	// api stays nil: the private path never touches it.

	// Track every peer up front so a mid-batch failure still leaves the by-peer
	// telegram_message delete able to reclaim what was written.
	h.track(func(c *created) {
		for _, it := range items {
			c.addTelegramPeer(it.Spec.PeerUserID)
		}
	})

	for _, generation := range partitionGenerations(entries) {
		peerIDs := make([]int64, 0, len(generation))
		messageIDs := make([]int32, 0, len(generation))
		for _, i := range generation {
			spec := items[i].Spec
			entities, update := BuildPrivateUpdate(spec)
			if err := handler.HandleNewMessage(ctx, entities, update); err != nil {
				return res, h.drainPartial(ctx, source, "", contactIDs, fmt.Errorf("telegram handle message: %w", err))
			}
			peerIDs = append(peerIDs, spec.PeerUserID)
			messageIDs = append(messageIDs, spec.TelegramMessageID)
		}
		res.SyncCalls++

		// Gate A is scoped to THIS generation's (peer, message id) pairs.
		if err := h.Settle(ctx, h.telegramBatchSettled(peerIDs, messageIDs), ""); err != nil {
			return res, h.drainPartial(ctx, source, "", contactIDs, err)
		}
		res.SettleCalls++
	}

	res.Interactions = h.trackBatchInteractions(ctx, contactIDs, before)
	for _, contactID := range contactIDs {
		if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceTelegram); err != nil {
			return res, err
		}
	}
	return res, nil
}

// telegramBatchSettled is the batch Gate A: every one of these (peer, message
// id) pairs has an interaction-linked telegram_message row. The key stays
// composite for the same reason the single-message gate's is — only the peer
// band is collision-checked at namespace setup.
func (h *Harness) telegramBatchSettled(peerUserIDs []int64, telegramMessageIDs []int32) gateA {
	want := int64(len(peerUserIDs))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountSettledTelegramMessagesByPeerAndMessageIDs(ctx, peerUserIDs, telegramMessageIDs)
		return n >= want, err
	}
}

// telegramBatchEntries projects the typed items into the source-neutral view.
// The identifier is the composite (peer, message id) because message ids alone
// are not namespace-disjoint; the addressed identifier is the handle the peer
// matcher resolves the contact by.
func telegramBatchEntries(items []TelegramBatchItem) []batchEntry {
	out := make([]batchEntry, 0, len(items))
	for _, it := range items {
		out = append(out, batchEntry{
			contactID:     it.ContactID,
			identifier:    fmt.Sprintf("%d:%d", it.Spec.PeerUserID, it.Spec.TelegramMessageID),
			seeded:        it.Spec.Intent == factory.MatchSeeded,
			outbound:      it.Spec.Out,
			pairKey:       it.PairKey,
			addressed:     it.Spec.MatchHandle,
			addressedType: identity.IdentifierTypeTelegram,
		})
	}
	return out
}

// BuildPrivateUpdate constructs the *tg.UpdateNewMessage + tg.Entities the private
// path reads: a *tg.PeerUser message whose PeerID is the peer (from which
// ParseMessage derives PeerUserID regardless of direction) and whose FromID is the
// sender — the peer for inbound, self (telegramSelfUserID) for outbound, so
// IsOutgoing=msg.Out reads the direction while peer matching still resolves the
// contact via PeerID. Exposed so the factory outbound-marker test can assert the
// message shape without a DB harness. FromID is set via SetFromID (NOT a bare
// struct-field assignment): SetFromID sets the flag bit GetFromID checks (see
// buildGroupUpdate), so a reader using the getter actually sees it.
func BuildPrivateUpdate(spec factory.TelegramMessageSpec) (tg.Entities, *tg.UpdateNewMessage) {
	user := &tg.User{ID: spec.PeerUserID, Username: spec.PeerUsername, FirstName: "Synth"}
	entities := tg.Entities{Users: map[int64]*tg.User{spec.PeerUserID: user}}
	fromID := &tg.PeerUser{UserID: spec.PeerUserID}
	if spec.Out {
		fromID = &tg.PeerUser{UserID: telegramSelfUserID}
	}
	msg := &tg.Message{
		ID:      int(spec.TelegramMessageID),
		Message: spec.Text,
		Out:     spec.Out,
		Date:    int(spec.SentAt.Unix()),
		PeerID:  &tg.PeerUser{UserID: spec.PeerUserID},
	}
	msg.SetFromID(fromID)
	return entities, &tg.UpdateNewMessage{Message: msg}
}
