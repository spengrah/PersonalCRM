package telegram

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"personal-crm/backend/internal/repository"
)

var phoneDigitsOnly = regexp.MustCompile(`[^0-9]`)

// peerRematchBase shares per-peer iteration logic between the username and
// phone telegram rematch handlers. Both handlers translate an identifier
// (handle or phone) into a set of distinct peer_user_ids whose unmatched
// messages match that identifier, then call OnPeerLinked per peer and run
// AggregateForContactBatch exactly once for the contact.
type peerRematchBase struct {
	messageRepo       *repository.TelegramMessageRepository
	peerMatcher       *PeerMatcher
	aggregationEngine *AggregationEngine
}

// rematchPeers iterates the peer set, links each via OnPeerLinked, sums the
// pre-link unmatched message counts as the "matched" total, and runs
// aggregation once if anything changed.
func (b *peerRematchBase) rematchPeers(ctx context.Context, contactID uuid.UUID, peers []repository.UnmatchedPeer) (int, error) {
	matched := 0
	for _, p := range peers {
		// Read pre-link count first so we report what the link will affect.
		// Failures are non-fatal; a 0 just means we under-report this peer.
		n, err := b.messageRepo.CountUnmatchedMessagesByPeer(ctx, p.PeerUserID)
		if err != nil {
			log.Warn().Err(err).Int64("peer_user_id", p.PeerUserID).
				Str("contact_id", contactID.String()).
				Msg("telegram rematch: count failed")
			n = 0
		}
		username := ""
		if p.PeerUsername != nil {
			username = *p.PeerUsername
		}
		if err := b.peerMatcher.OnPeerLinked(ctx, p.PeerUserID, username, contactID); err != nil {
			log.Warn().Err(err).Int64("peer_user_id", p.PeerUserID).
				Str("contact_id", contactID.String()).
				Msg("telegram rematch: peer link failed")
			continue
		}
		matched += int(n)
	}
	if matched > 0 {
		if err := b.aggregationEngine.AggregateForContactBatch(ctx, contactID); err != nil {
			log.Warn().Err(err).Str("contact_id", contactID.String()).
				Msg("telegram rematch: aggregation failed")
			// Don't fail the job — messages are linked. Next aggregation pass
			// will catch up.
		}
	}
	return matched, nil
}

// UsernameRematchHandler implements service.RematchHandler for the "telegram"
// identifier type, matching by peer_username (case-insensitive).
type UsernameRematchHandler struct{ peerRematchBase }

// NewUsernameRematchHandler constructs a UsernameRematchHandler.
func NewUsernameRematchHandler(mr *repository.TelegramMessageRepository, pm *PeerMatcher, ae *AggregationEngine) *UsernameRematchHandler {
	return &UsernameRematchHandler{peerRematchBase{messageRepo: mr, peerMatcher: pm, aggregationEngine: ae}}
}

// IdentifierType returns "telegram".
func (h *UsernameRematchHandler) IdentifierType() string { return "telegram" }

// Rematch links pre-existing telegram messages whose peer_username matches
// the given handle to the supplied contact and runs aggregation.
func (h *UsernameRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, handleNormalized string) (int, error) {
	if handleNormalized == "" {
		return 0, nil
	}
	peers, err := h.messageRepo.FindDistinctUnmatchedPeerUserIDsByUsername(ctx, handleNormalized)
	if err != nil {
		return 0, fmt.Errorf("find peers by username: %w", err)
	}
	return h.rematchPeers(ctx, contactID, peers)
}

// PhoneRematchHandler implements service.RematchHandler for the "phone"
// identifier type, matching by peer_phone using digits-only comparison.
type PhoneRematchHandler struct{ peerRematchBase }

// NewPhoneRematchHandler constructs a PhoneRematchHandler.
func NewPhoneRematchHandler(mr *repository.TelegramMessageRepository, pm *PeerMatcher, ae *AggregationEngine) *PhoneRematchHandler {
	return &PhoneRematchHandler{peerRematchBase{messageRepo: mr, peerMatcher: pm, aggregationEngine: ae}}
}

// IdentifierType returns "phone".
func (h *PhoneRematchHandler) IdentifierType() string { return "phone" }

// Rematch links pre-existing telegram messages whose peer_phone matches the
// digits-only form of the given phone to the supplied contact.
func (h *PhoneRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, phoneNormalized string) (int, error) {
	digits := phoneDigitsOnly.ReplaceAllString(phoneNormalized, "")
	if digits == "" {
		return 0, nil
	}
	peers, err := h.messageRepo.FindDistinctUnmatchedPeerUserIDsByPhone(ctx, digits)
	if err != nil {
		return 0, fmt.Errorf("find peers by phone: %w", err)
	}
	return h.rematchPeers(ctx, contactID, peers)
}
