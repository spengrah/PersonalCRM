package replay

import (
	"context"
	"fmt"

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
