package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// identityMatcher defines the interface for matching identifiers to contacts.
// Satisfied by *service.IdentityService.
type identityMatcher interface {
	MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error)
}

// externalContactUpserter defines the interface for upserting/updating external contacts.
type externalContactUpserter interface {
	Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
	GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error)
	UpdateMatch(ctx context.Context, id uuid.UUID, crmContactID *uuid.UUID, status repository.MatchStatus) (*repository.ExternalContact, error)
}

// PeerMatcher matches Telegram peers to CRM contacts using the identity service.
type PeerMatcher struct {
	identityService     identityMatcher
	messageRepo         *repository.TelegramMessageRepository
	externalContactRepo externalContactUpserter
	discoveryMinMsgs    int
}

// NewPeerMatcher creates a new peer matcher.
func NewPeerMatcher(
	identityService identityMatcher,
	messageRepo *repository.TelegramMessageRepository,
	externalContactRepo externalContactUpserter,
	discoveryMinMsgs int,
) *PeerMatcher {
	return &PeerMatcher{
		identityService:     identityService,
		messageRepo:         messageRepo,
		externalContactRepo: externalContactRepo,
		discoveryMinMsgs:    discoveryMinMsgs,
	}
}

// MatchPeer attempts to match a Telegram peer to a CRM contact.
// Returns the matched contact ID or nil if unmatched.
func (m *PeerMatcher) MatchPeer(ctx context.Context, peerUserID int64, peerUsername, peerFirstName, peerLastName, peerPhone *string) (*uuid.UUID, error) {
	peerIDStr := strconv.FormatInt(peerUserID, 10)
	displayName := buildDisplayName(peerFirstName, peerLastName)

	// 1. Try username match
	if peerUsername != nil && *peerUsername != "" {
		result, err := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: *peerUsername,
			Type:          identity.IdentifierTypeTelegram,
			Source:        "telegram",
			SourceID:      &peerIDStr,
			DisplayName:   displayName,
		})
		if err != nil {
			log.Warn().Err(err).Str("username", *peerUsername).Msg("telegram: failed to match username")
		} else if result.ContactID != nil {
			// Matched via username
			if m.messageRepo != nil {
				if err := m.messageRepo.UpdateMessageContact(ctx, peerUserID, *result.ContactID); err != nil {
					log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: failed to update message contacts")
				}
			}
			m.markExternalContactMatched(ctx, peerUserID, *result.ContactID)
			log.Info().
				Int64("peer_user_id", peerUserID).
				Str("contact_id", result.ContactID.String()).
				Str("match_type", string(result.MatchType)).
				Msg("telegram: peer matched via username")
			return result.ContactID, nil
		}
	}

	// 2. Try phone match
	if peerPhone != nil && *peerPhone != "" {
		result, err := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: *peerPhone,
			Type:          identity.IdentifierTypePhone,
			Source:        "telegram",
			SourceID:      &peerIDStr,
			DisplayName:   displayName,
		})
		if err != nil {
			log.Warn().Err(err).Str("phone", *peerPhone).Msg("telegram: failed to match phone")
		} else if result.ContactID != nil {
			// Matched via phone — also link the telegram identity
			if m.messageRepo != nil {
				if err := m.messageRepo.UpdateMessageContact(ctx, peerUserID, *result.ContactID); err != nil {
					log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: failed to update message contacts")
				}
			}
			m.markExternalContactMatched(ctx, peerUserID, *result.ContactID)

			// Link telegram identity to this contact (so future matches use username cache)
			if peerUsername != nil && *peerUsername != "" {
				_, linkErr := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
					RawIdentifier:  *peerUsername,
					Type:           identity.IdentifierTypeTelegram,
					Source:         "telegram",
					SourceID:       &peerIDStr,
					DisplayName:    displayName,
					KnownContactID: result.ContactID,
				})
				if linkErr != nil {
					log.Warn().Err(linkErr).Msg("telegram: failed to link telegram identity after phone match")
				}
			}

			log.Info().
				Int64("peer_user_id", peerUserID).
				Str("contact_id", result.ContactID.String()).
				Msg("telegram: peer matched via phone")
			return result.ContactID, nil
		}
	}

	// 3. Unmatched — external_identity was already created by MatchOrCreate
	log.Debug().Int64("peer_user_id", peerUserID).Msg("telegram: peer unmatched")
	return nil, nil
}

// MatchAllUnmatched runs identity matching for all distinct unmatched peers.
func (m *PeerMatcher) MatchAllUnmatched(ctx context.Context) error {
	peers, err := m.messageRepo.ListDistinctUnmatchedPeers(ctx)
	if err != nil {
		return fmt.Errorf("list unmatched peers: %w", err)
	}

	matched, unmatched := 0, 0
	for _, peer := range peers {
		contactID, err := m.MatchPeer(ctx, peer.PeerUserID, peer.PeerUsername, peer.PeerFirstName, peer.PeerLastName, peer.PeerPhone)
		if err != nil {
			log.Warn().Err(err).Int64("peer_user_id", peer.PeerUserID).Msg("telegram: batch match failed for peer")
			continue
		}
		if contactID != nil {
			matched++
		} else {
			unmatched++
		}
	}

	log.Info().Int("matched", matched).Int("unmatched", unmatched).Int("total", len(peers)).Msg("telegram: batch identity matching complete")
	return nil
}

// UpdateDiscoveryCandidates upserts external_contact rows for qualifying unmatched peers.
func (m *PeerMatcher) UpdateDiscoveryCandidates(ctx context.Context) error {
	counts, err := m.messageRepo.CountMessagesByPeer(ctx)
	if err != nil {
		return fmt.Errorf("count messages by peer: %w", err)
	}

	// Build a map of peer info for display names
	peers, err := m.messageRepo.ListDistinctUnmatchedPeers(ctx)
	if err != nil {
		return fmt.Errorf("list unmatched peers for discovery: %w", err)
	}
	peerMap := make(map[int64]repository.UnmatchedPeer, len(peers))
	for _, p := range peers {
		peerMap[p.PeerUserID] = p
	}

	created := 0
	for _, count := range counts {
		if count.TotalCount < int64(m.discoveryMinMsgs) {
			continue
		}

		// Only upsert for unmatched peers
		peer, ok := peerMap[count.PeerUserID]
		if !ok {
			continue // peer was matched, skip
		}

		displayName := buildDisplayName(peer.PeerFirstName, peer.PeerLastName)
		sourceID := strconv.FormatInt(count.PeerUserID, 10)

		metadata := map[string]any{
			"message_count":  count.TotalCount,
			"outbound_count": count.OutboundCount,
			"inbound_count":  count.InboundCount,
		}
		if !count.LastMessageAt.IsZero() {
			metadata["last_message_at"] = count.LastMessageAt.Format("2006-01-02T15:04:05Z")
		}
		if peer.PeerUsername != nil {
			metadata["username"] = "@" + *peer.PeerUsername
		}

		_, err := m.externalContactRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:      "telegram",
			SourceID:    sourceID,
			DisplayName: displayName,
			FirstName:   peer.PeerFirstName,
			LastName:    peer.PeerLastName,
			Metadata:    metadata,
		})
		if err != nil {
			log.Warn().Err(err).Int64("peer_user_id", count.PeerUserID).Msg("telegram: failed to upsert discovery candidate")
			continue
		}
		created++
	}

	log.Info().Int("candidates", created).Msg("telegram: discovery candidates updated")
	return nil
}

// UpdateDiscoveryCandidatesForPeer checks if a specific unmatched peer has crossed the
// discovery threshold and upserts their external_contact if so. Used for live messages.
// Uses a single-peer count query to avoid scanning all peers.
func (m *PeerMatcher) UpdateDiscoveryCandidatesForPeer(ctx context.Context, peerUserID int64, peerUsername, peerFirstName, peerLastName *string) {
	count, err := m.messageRepo.CountMessagesByPeerID(ctx, peerUserID)
	if err != nil {
		log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: failed to count messages for discovery check")
		return
	}

	if count.TotalCount < int64(m.discoveryMinMsgs) {
		return // below threshold
	}

	displayName := buildDisplayName(peerFirstName, peerLastName)
	sourceID := strconv.FormatInt(peerUserID, 10)

	metadata := map[string]any{
		"message_count":  count.TotalCount,
		"outbound_count": count.OutboundCount,
		"inbound_count":  count.InboundCount,
	}
	if !count.LastMessageAt.IsZero() {
		metadata["last_message_at"] = count.LastMessageAt.Format("2006-01-02T15:04:05Z")
	}
	if peerUsername != nil {
		metadata["username"] = "@" + *peerUsername
	}

	if _, err := m.externalContactRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    sourceID,
		DisplayName: displayName,
		FirstName:   peerFirstName,
		LastName:    peerLastName,
		Metadata:    metadata,
	}); err != nil {
		log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: failed to upsert discovery candidate for live peer")
	}
}

// OnPeerLinked is called after a Telegram import/link to back-fill message matching.
// It links the identity, updates matched_contact_id on messages, and returns true
// to signal that aggregation should run.
func (m *PeerMatcher) OnPeerLinked(ctx context.Context, peerUserID int64, peerUsername string, contactID uuid.UUID) error {
	// 1. Link the telegram identity to this contact
	if peerUsername != "" {
		peerIDStr := strconv.FormatInt(peerUserID, 10)
		_, err := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier:  peerUsername,
			Type:           identity.IdentifierTypeTelegram,
			Source:         "telegram",
			SourceID:       &peerIDStr,
			KnownContactID: &contactID,
		})
		if err != nil {
			log.Warn().Err(err).Msg("telegram: failed to link identity on import")
		}
	}

	// 2. Update matched_contact_id on all messages for this peer
	if m.messageRepo != nil {
		if err := m.messageRepo.UpdateMessageContact(ctx, peerUserID, contactID); err != nil {
			return fmt.Errorf("update message contacts on import: %w", err)
		}
	}

	log.Info().
		Int64("peer_user_id", peerUserID).
		Str("contact_id", contactID.String()).
		Msg("telegram: peer linked via import, messages updated")
	return nil
}

// markExternalContactMatched updates any existing external_contact for this peer to "matched" status.
func (m *PeerMatcher) markExternalContactMatched(ctx context.Context, peerUserID int64, contactID uuid.UUID) {
	sourceID := strconv.FormatInt(peerUserID, 10)
	existing, err := m.externalContactRepo.GetBySource(ctx, "telegram", sourceID, nil)
	if err != nil || existing == nil {
		return // no existing candidate, nothing to update
	}
	if existing.MatchStatus == repository.MatchStatusMatched || existing.MatchStatus == repository.MatchStatusImported {
		return // already marked
	}
	if _, err := m.externalContactRepo.UpdateMatch(ctx, existing.ID, &contactID, repository.MatchStatusMatched); err != nil {
		log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: failed to mark external contact as matched")
	}
}

func buildDisplayName(firstName, lastName *string) *string {
	var parts []string
	if firstName != nil && *firstName != "" {
		parts = append(parts, *firstName)
	}
	if lastName != nil && *lastName != "" {
		parts = append(parts, *lastName)
	}
	if len(parts) == 0 {
		return nil
	}
	name := strings.Join(parts, " ")
	return &name
}
