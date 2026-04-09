package telegram

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/gotd/td/tg"
	"github.com/rs/zerolog/log"
)

// MessageHandler processes Telegram update events and stores messages.
type MessageHandler struct {
	messageRepo       *repository.TelegramMessageRepository
	chatConfigRepo    *repository.TelegramChatConfigRepository
	syncRepo          *repository.SyncRepository
	syncStateID       *uuid.UUID
	selfUserID        int64
	groupMaxMembers   int
	api               *tg.Client
	peerMatcher       *PeerMatcher
	aggregationEngine *AggregationEngine
}

// NewMessageHandler creates a new message handler.
func NewMessageHandler(
	messageRepo *repository.TelegramMessageRepository,
	chatConfigRepo *repository.TelegramChatConfigRepository,
	syncRepo *repository.SyncRepository,
	syncStateID *uuid.UUID,
	selfUserID int64,
	groupMaxMembers int,
	peerMatcher *PeerMatcher,
	aggregationEngine *AggregationEngine,
) *MessageHandler {
	return &MessageHandler{
		messageRepo:       messageRepo,
		chatConfigRepo:    chatConfigRepo,
		syncRepo:          syncRepo,
		syncStateID:       syncStateID,
		selfUserID:        selfUserID,
		groupMaxMembers:   groupMaxMembers,
		peerMatcher:       peerMatcher,
		aggregationEngine: aggregationEngine,
	}
}

// SetAPI sets the tg.Client reference. Must be called inside the client.Run
// callback before handlers process updates.
func (h *MessageHandler) SetAPI(api *tg.Client) {
	h.api = api
}

// HandleNewMessage processes OnNewMessage updates (private + group chats).
func (h *MessageHandler) HandleNewMessage(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil // skip MessageService, MessageEmpty
	}

	parsed := ParseMessage(msg, e, h.selfUserID)
	if parsed == nil {
		return nil // filtered out (self-chat, bot, channel)
	}

	if parsed.ChatType == "group" {
		tracked, err := h.shouldTrackChat(ctx, parsed, e)
		if err != nil {
			log.Warn().Err(err).Int64("chat_id", parsed.TelegramChatID).Msg("telegram: failed to check chat tracking")
			return nil
		}
		if !tracked {
			return nil
		}
	}

	if _, err := h.messageRepo.UpsertMessage(ctx, parsedToUpsertParams(parsed)); err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	h.updateSyncTimestamp(ctx)

	log.Info().
		Int64("chat_id", parsed.TelegramChatID).
		Int32("message_id", parsed.TelegramMessageID).
		Str("chat_type", parsed.ChatType).
		Bool("is_outgoing", parsed.IsOutgoing).
		Msg("telegram: message stored")

	// Phase 4: identity matching + aggregation (best-effort)
	if h.peerMatcher != nil && parsed.PeerUserID != nil {
		contactID, err := h.peerMatcher.MatchPeer(ctx, *parsed.PeerUserID, parsed.PeerUsername, parsed.PeerFirstName, parsed.PeerLastName, parsed.PeerPhone)
		if err != nil {
			log.Warn().Err(err).Int64("peer_user_id", *parsed.PeerUserID).Msg("telegram: peer matching failed")
		} else if contactID != nil && h.aggregationEngine != nil {
			// Use batch aggregation to process all chats for this contact
			// (MatchPeer links messages across all chats for the peer)
			if err := h.aggregationEngine.AggregateForContactBatch(ctx, *contactID); err != nil {
				log.Warn().Err(err).Str("contact_id", contactID.String()).Msg("telegram: aggregation failed")
			}
		} else if contactID == nil {
			// Unmatched peer — update discovery candidates incrementally
			h.peerMatcher.UpdateDiscoveryCandidatesForPeer(ctx, *parsed.PeerUserID, parsed.PeerUsername, parsed.PeerFirstName, parsed.PeerLastName)
		}
	}

	return nil
}

// HandleEditMessage processes OnEditMessage updates.
func (h *MessageHandler) HandleEditMessage(ctx context.Context, e tg.Entities, update *tg.UpdateEditMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}

	parsed := ParseMessage(msg, e, h.selfUserID)
	if parsed == nil {
		return nil
	}

	if parsed.ChatType == "group" {
		tracked, err := h.shouldTrackChat(ctx, parsed, e)
		if err != nil {
			log.Warn().Err(err).Int64("chat_id", parsed.TelegramChatID).Msg("telegram: failed to check chat tracking for edit")
			return nil
		}
		if !tracked {
			return nil
		}
	}

	if _, err := h.messageRepo.UpsertMessage(ctx, parsedToUpsertParams(parsed)); err != nil {
		return fmt.Errorf("upsert edited message: %w", err)
	}

	log.Debug().
		Int64("chat_id", parsed.TelegramChatID).
		Int32("message_id", parsed.TelegramMessageID).
		Msg("telegram: edit stored")

	return nil
}

// HandleDeleteMessages processes OnDeleteMessages updates (no chat_id).
func (h *MessageHandler) HandleDeleteMessages(ctx context.Context, _ tg.Entities, update *tg.UpdateDeleteMessages) error {
	ids := make([]int32, len(update.Messages))
	for i, id := range update.Messages {
		ids[i] = int32(id)
	}

	if err := h.messageRepo.SoftDeleteMessages(ctx, ids); err != nil {
		return fmt.Errorf("soft delete messages: %w", err)
	}

	log.Debug().Int("count", len(ids)).Msg("telegram: messages soft-deleted")
	return nil
}

// HandleChatParticipant processes OnChatParticipant updates to refresh member_count.
func (h *MessageHandler) HandleChatParticipant(ctx context.Context, _ tg.Entities, update *tg.UpdateChatParticipant) error {
	if h.api == nil {
		return nil
	}

	chatID := update.ChatID

	fullChat, err := h.api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		log.Warn().Err(err).Int64("chat_id", chatID).Msg("telegram: failed to get full chat for participant count")
		return nil
	}

	var memberCount int32
	if fc, ok := fullChat.FullChat.(*tg.ChatFull); ok {
		// For regular chats, count participants from the participants list
		if participants, ok := fc.Participants.(*tg.ChatParticipants); ok {
			memberCount = int32(len(participants.Participants))
		} else {
			return nil
		}
	} else {
		return nil
	}

	if err := h.chatConfigRepo.UpdateMemberCount(ctx, chatID, memberCount); err != nil {
		log.Warn().Err(err).Int64("chat_id", chatID).Msg("telegram: failed to update member count")
	} else {
		log.Debug().Int64("chat_id", chatID).Int32("member_count", memberCount).Msg("telegram: member count updated")
	}

	return nil
}

// shouldTrackChat checks whether messages from this group chat should be stored.
func (h *MessageHandler) shouldTrackChat(ctx context.Context, parsed *ParsedMessage, entities tg.Entities) (bool, error) {
	cfg, err := h.chatConfigRepo.GetConfig(ctx, parsed.TelegramChatID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return false, fmt.Errorf("get chat config: %w", err)
	}
	if errors.Is(err, db.ErrNotFound) {
		// Chat not yet discovered — upsert with defaults
		var memberCount *int32
		if chat, ok := entities.Chats[parsed.TelegramChatID]; ok {
			mc := int32(chat.ParticipantsCount)
			memberCount = &mc
		}
		cfg, err = h.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
			TelegramChatID: parsed.TelegramChatID,
			ChatTitle:      parsed.ChatTitle,
			ChatType:       parsed.ChatType,
			MemberCount:    memberCount,
			Status:         "auto",
		})
		if err != nil {
			return false, fmt.Errorf("upsert chat config: %w", err)
		}
	}

	return EffectiveTracked(cfg.Status, cfg.MemberCount, h.groupMaxMembers), nil
}

// EffectiveTracked computes whether a chat is effectively tracked.
func EffectiveTracked(status string, memberCount *int32, groupMaxMembers int) bool {
	switch status {
	case "tracked":
		return true
	case "ignored":
		return false
	default: // "auto"
		if memberCount == nil {
			return true // unknown size — track by default
		}
		return int(*memberCount) <= groupMaxMembers
	}
}

func parsedToUpsertParams(p *ParsedMessage) repository.UpsertTelegramMessageParams {
	return repository.UpsertTelegramMessageParams{
		TelegramMessageID: p.TelegramMessageID,
		TelegramChatID:    p.TelegramChatID,
		ChatType:          p.ChatType,
		ChatTitle:         p.ChatTitle,
		MessageText:       p.MessageText,
		MessageType:       p.MessageType,
		SentAt:            p.SentAt,
		EditedAt:          p.EditedAt,
		IsOutgoing:        p.IsOutgoing,
		ReplyToMsgID:      p.ReplyToMsgID,
		PeerUserID:        p.PeerUserID,
		PeerUsername:      p.PeerUsername,
		PeerFirstName:     p.PeerFirstName,
		PeerLastName:      p.PeerLastName,
		PeerPhone:         p.PeerPhone,
	}
}

func (h *MessageHandler) updateSyncTimestamp(ctx context.Context) {
	if h.syncStateID == nil {
		return
	}
	_, err := h.syncRepo.UpdateSyncStateStatus(ctx, *h.syncStateID, repository.SyncStatusIdle, nil)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: failed to update sync timestamp")
	}
}
