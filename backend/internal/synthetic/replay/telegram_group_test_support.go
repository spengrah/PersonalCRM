package replay

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// TelegramGroupResult is the settled outcome of a Telegram GROUP replay.
type TelegramGroupResult struct {
	ContactID    uuid.UUID
	ChatID       int64
	SenderUserID int64
	Matched      bool // false for MatchUnknown (stranded sender)
	Tracked      bool // false when shouldTrackChat gated the message out (by size)
}

// newGroupHandler builds the MessageHandler the group adapter drives. Same
// construction the private path uses (nil api by default — the group
// HandleNewMessage path never dereferences it), with the harness's
// groupMaxMembers so the size gate behaves as the tests expect.
func (h *Harness) newGroupHandler() *telegram.MessageHandler {
	return telegram.NewMessageHandler(
		h.telegramRepo,
		repository.NewTelegramChatConfigRepository(h.database.Queries),
		repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool),
		nil,
		telegramSelfUserID,
		h.groupMaxMembers,
		h.peerMatcher.matcher,
		h.peerMatcher.engine,
	)
}

// buildGroupUpdate constructs the *tg.UpdateNewMessage + tg.Entities the group
// path reads: a *tg.PeerChat message with the sender in FromID, the chat title +
// participant count on entities.Chats[chatID], and the sender's tg.User on
// entities.Users[senderID]. Matches the production update shape (handlers.go +
// message.go *tg.PeerChat branch).
func buildGroupUpdate(spec factory.TelegramGroupMessageSpec) (tg.Entities, *tg.UpdateNewMessage) {
	user := &tg.User{ID: spec.SenderUserID, Username: spec.SenderUsername, FirstName: "Synth"}
	chat := &tg.Chat{ID: spec.ChatID, Title: spec.ChatTitle, ParticipantsCount: spec.ParticipantsCount}
	entities := tg.Entities{
		Users: map[int64]*tg.User{spec.SenderUserID: user},
		Chats: map[int64]*tg.Chat{spec.ChatID: chat},
	}
	msg := &tg.Message{
		ID:      int(spec.TelegramMessageID),
		Message: spec.Text,
		Out:     false, // inbound
		Date:    int(spec.SentAt.Unix()),
		PeerID:  &tg.PeerChat{ChatID: spec.ChatID},
	}
	// SetFromID sets the flag bit GetFromID checks; a bare FromID struct-field
	// assignment leaves the flag unset, so ParseMessage's group branch would not
	// see the sender (PeerUserID stays nil → matcher never runs).
	msg.SetFromID(&tg.PeerUser{UserID: spec.SenderUserID})
	return entities, &tg.UpdateNewMessage{Message: msg}
}

// trackGroup records the sender peer (telegram_message cleanup) and the group
// chat id (telegram_chat_config cleanup) into the ledger.
func (h *Harness) trackGroup(spec factory.TelegramGroupMessageSpec) {
	h.track(func(c *created) {
		c.addTelegramPeer(spec.SenderUserID)
		c.addTelegramChat(spec.ChatID)
	})
}

// ReplayTelegramGroup feeds a synthetic inbound GROUP message through the REAL
// MessageHandler.HandleNewMessage (nil api — the group path never touches it),
// then settles on a group-scoped Gate-A predicate keyed to the spec's intent +
// member count:
//   - MatchSeeded (tracked): the telegram_message row (chat, message id) is
//     linked to an interaction for the contact.
//   - MatchUnknown (tracked): the message row exists with matched_contact_id
//     IS NULL (stranded; a discovery candidate once the per-peer threshold is
//     crossed — see ReplayTelegramGroupMessages for that path).
//   - untracked-by-size (ParticipantsCount > groupMaxMembers, status "auto"):
//     shouldTrackChat returns false before UpsertMessage, so NO telegram_message
//     row is written; settle asserts the absence + that the config row was
//     upserted with the large member count (the gate ran).
//
// contactID is the seeded contact this message targets (for MatchSeeded tracked).
func (h *Harness) ReplayTelegramGroup(ctx context.Context, contactID uuid.UUID, spec factory.TelegramGroupMessageSpec) (TelegramGroupResult, error) {
	handler := h.newGroupHandler()
	h.trackGroup(spec)

	entities, update := buildGroupUpdate(spec)
	if err := handler.HandleNewMessage(ctx, entities, update); err != nil {
		return TelegramGroupResult{}, fmt.Errorf("telegram group handle message: %w", err)
	}

	tracked := spec.ParticipantsCount <= h.groupMaxMembers

	if !tracked {
		// Untracked-by-size: the message must NOT be stored. Settle once Gate B is
		// clear (no jobs for an empty contact set short-circuits to 0); the
		// no-store assertion is the test's responsibility via NotStored.
		if err := h.Settle(ctx, nil, ""); err != nil {
			return TelegramGroupResult{}, err
		}
		return TelegramGroupResult{ChatID: spec.ChatID, SenderUserID: spec.SenderUserID, Tracked: false}, nil
	}

	if spec.Intent == factory.MatchUnknown {
		predicate := func(ctx context.Context) (bool, error) {
			return h.telegramPeerStranded(ctx, spec.SenderUserID)
		}
		if err := h.Settle(ctx, predicate, ""); err != nil {
			return TelegramGroupResult{}, err
		}
		return TelegramGroupResult{ChatID: spec.ChatID, SenderUserID: spec.SenderUserID, Matched: false, Tracked: true}, nil
	}

	if err := h.Settle(ctx, h.telegramSettled(spec.SenderUserID, spec.TelegramMessageID), ""); err != nil {
		return TelegramGroupResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return TelegramGroupResult{ContactID: contactID, ChatID: spec.ChatID, SenderUserID: spec.SenderUserID, Matched: true, Tracked: true}, nil
}

// ReplayTelegramGroupMessages drives a SEQUENCE of group messages (sharing one
// chat id + sender) through the pipeline so the per-peer discovery threshold can
// be crossed (UpdateDiscoveryCandidatesForPeer only upserts once the peer's total
// message count reaches the matcher's minimum). Each spec must carry the SAME
// ChatID + SenderUserID; the caller builds them via
// factory.TelegramGroupMessageInChat. Settles on the last spec.
func (h *Harness) ReplayTelegramGroupMessages(ctx context.Context, contactID uuid.UUID, specs []factory.TelegramGroupMessageSpec) (TelegramGroupResult, error) {
	if len(specs) == 0 {
		return TelegramGroupResult{}, errors.New("replay telegram group: no specs")
	}
	handler := h.newGroupHandler()
	var last factory.TelegramGroupMessageSpec
	for _, spec := range specs {
		h.trackGroup(spec)
		entities, update := buildGroupUpdate(spec)
		if err := handler.HandleNewMessage(ctx, entities, update); err != nil {
			return TelegramGroupResult{}, fmt.Errorf("telegram group handle message: %w", err)
		}
		last = spec
	}

	if last.Intent == factory.MatchUnknown {
		predicate := func(ctx context.Context) (bool, error) {
			return h.telegramPeerStranded(ctx, last.SenderUserID)
		}
		if err := h.Settle(ctx, predicate, ""); err != nil {
			return TelegramGroupResult{}, err
		}
		return TelegramGroupResult{ChatID: last.ChatID, SenderUserID: last.SenderUserID, Matched: false, Tracked: true}, nil
	}

	if err := h.Settle(ctx, h.telegramSettled(last.SenderUserID, last.TelegramMessageID), ""); err != nil {
		return TelegramGroupResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return TelegramGroupResult{ContactID: contactID, ChatID: last.ChatID, SenderUserID: last.SenderUserID, Matched: true, Tracked: true}, nil
}

// GroupMessageStored reports whether a telegram_message row exists for the group
// chat + message id. Tests assert false for the untracked-by-size case (the
// shouldTrackChat gate returned before UpsertMessage) and true for tracked.
func (h *Harness) GroupMessageStored(ctx context.Context, chatID int64, messageID int32) (bool, error) {
	n, err := h.support.CountTelegramMessagesByChatAndMessageID(ctx, chatID, messageID)
	return n > 0, err
}

// GroupConfigMemberCount returns the stored telegram_chat_config.member_count for
// a group chat (nil when unset). Tests assert the size gate upserted the config
// with the observed member count, and that the participant-refresh path updated
// it.
func (h *Harness) GroupConfigMemberCount(ctx context.Context, chatID int64) (*int32, error) {
	cfg, err := repository.NewTelegramChatConfigRepository(h.database.Queries).GetConfig(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return cfg.MemberCount, nil
}

// RefreshGroupMemberCount drives HandleChatParticipant with a synthetic
// *tg.UpdateChatParticipant after installing a stub tg.Invoker-backed client via
// the production SetAPI seam. The stub recognizes the messages.getFullChat
// request and writes a synthetic *tg.MessagesChatFull carrying participantCount
// members, so the handler's member-count refresh runs end-to-end with NO live
// Telegram and NO production change. Asserts via GroupConfigMemberCount.
func (h *Harness) RefreshGroupMemberCount(ctx context.Context, chatID int64, participantCount int) error {
	handler := h.newGroupHandler()
	handler.SetAPI(tg.NewClient(&fullChatStubInvoker{chatID: chatID, participantCount: participantCount}))
	update := &tg.UpdateChatParticipant{ChatID: chatID}
	if err := handler.HandleChatParticipant(ctx, tg.Entities{}, update); err != nil {
		return fmt.Errorf("telegram handle chat participant: %w", err)
	}
	return nil
}

// fullChatStubInvoker is a tg.Invoker that answers messages.getFullChat with a
// synthetic *tg.MessagesChatFull whose ChatFull.Participants is a
// *tg.ChatParticipants of participantCount entries. It populates the decoded
// output struct DIRECTLY (no MTProto bin wire encode/decode), which is all
// MessagesGetFullChat needs — c.rpc.Invoke hands the stub the *tg.MessagesChatFull
// result pointer to fill. Any other request returns an error so an unexpected
// call fails loudly rather than silently. Test-support only.
type fullChatStubInvoker struct {
	chatID           int64
	participantCount int
}

func (s *fullChatStubInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	req, ok := input.(*tg.MessagesGetFullChatRequest)
	if !ok {
		return fmt.Errorf("synthetic stub invoker: unexpected request %T", input)
	}
	if req.ChatID != s.chatID {
		return fmt.Errorf("synthetic stub invoker: chat id mismatch (got %d, want %d)", req.ChatID, s.chatID)
	}
	out, ok := output.(*tg.MessagesChatFull)
	if !ok {
		return fmt.Errorf("synthetic stub invoker: unexpected output %T", output)
	}
	participants := make([]tg.ChatParticipantClass, 0, s.participantCount)
	for i := 0; i < s.participantCount; i++ {
		participants = append(participants, &tg.ChatParticipant{UserID: int64(i + 1)})
	}
	out.FullChat = &tg.ChatFull{
		ID:           s.chatID,
		Participants: &tg.ChatParticipants{ChatID: s.chatID, Participants: participants},
	}
	return nil
}
