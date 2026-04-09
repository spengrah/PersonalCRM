package telegram

import (
	"time"

	"github.com/gotd/td/tg"
)

// ParsedMessage is the internal representation of a Telegram message,
// independent of gotd types. gotd types do not leak beyond this file.
type ParsedMessage struct {
	TelegramMessageID int32
	TelegramChatID    int64
	ChatType          string // "private" or "group"
	ChatTitle         *string
	MessageText       *string
	MessageType       string // "text", "photo", "voice", "video", "document", "sticker", "other"
	SentAt            time.Time
	EditedAt          *time.Time
	IsOutgoing        bool
	ReplyToMsgID      *int32
	PeerUserID        *int64
	PeerUsername      *string
	PeerFirstName     *string
	PeerLastName      *string
	PeerPhone         *string
}

// ParseMessage converts a tg.Message and handler entities into a ParsedMessage.
// Returns nil if the message should be filtered out (self-chat, bot, etc.).
func ParseMessage(msg *tg.Message, entities tg.Entities, selfUserID int64) *ParsedMessage {
	parsed := &ParsedMessage{
		TelegramMessageID: int32(msg.ID),
		IsOutgoing:        msg.Out,
		SentAt:            time.Unix(int64(msg.Date), 0),
	}

	// Text (also serves as media caption)
	if msg.Message != "" {
		text := msg.Message
		parsed.MessageText = &text
	}

	// Media type
	parsed.MessageType = classifyMedia(msg.Media)

	// Edit timestamp
	if editDate, ok := msg.GetEditDate(); ok && editDate != 0 {
		t := time.Unix(int64(editDate), 0)
		parsed.EditedAt = &t
	}

	// Reply-to
	if replyTo, ok := msg.GetReplyTo(); ok {
		if header, ok := replyTo.(*tg.MessageReplyHeader); ok {
			if replyMsgID, ok := header.GetReplyToMsgID(); ok {
				id := int32(replyMsgID)
				parsed.ReplyToMsgID = &id
			}
		}
	}

	// Extract peer info based on chat type
	switch peer := msg.PeerID.(type) {
	case *tg.PeerUser:
		parsed.ChatType = "private"
		parsed.TelegramChatID = peer.UserID

		// Skip self-chat (Saved Messages)
		if peer.UserID == selfUserID {
			return nil
		}

		// For private chats, the peer is the other user
		peerUserID := peer.UserID
		if msg.Out {
			// Outgoing: peer is the recipient
			parsed.PeerUserID = &peerUserID
		} else {
			// Incoming: peer is the sender (same as chat peer in 1:1)
			parsed.PeerUserID = &peerUserID
		}

		// Resolve user info from entities
		if user, ok := entities.Users[peerUserID]; ok {
			if user.Bot {
				return nil // skip bot conversations
			}
			fillUserInfo(parsed, user)
		}

	case *tg.PeerChat:
		parsed.ChatType = "group"
		parsed.TelegramChatID = peer.ChatID

		// Chat title from entities
		if chat, ok := entities.Chats[peer.ChatID]; ok {
			title := chat.Title
			parsed.ChatTitle = &title
		}

		// Sender from FromID
		if fromID, ok := msg.GetFromID(); ok {
			if fromPeer, ok := fromID.(*tg.PeerUser); ok {
				senderID := fromPeer.UserID

				// Skip self-messages in groups for peer info (we still store the message)
				if senderID != selfUserID {
					parsed.PeerUserID = &senderID
					if user, ok := entities.Users[senderID]; ok {
						if user.Bot {
							return nil
						}
						fillUserInfo(parsed, user)
					}
				}
			}
		}

	default:
		// PeerChannel or unknown — skip (supergroups/channels not tracked)
		return nil
	}

	return parsed
}

func fillUserInfo(parsed *ParsedMessage, user *tg.User) {
	if user.Username != "" {
		username := user.Username
		parsed.PeerUsername = &username
	}
	if user.FirstName != "" {
		firstName := user.FirstName
		parsed.PeerFirstName = &firstName
	}
	if user.LastName != "" {
		lastName := user.LastName
		parsed.PeerLastName = &lastName
	}
	if user.Phone != "" {
		phone := user.Phone
		parsed.PeerPhone = &phone
	}
}

// classifyMedia returns the message_type based on the media attachment.
func classifyMedia(media tg.MessageMediaClass) string {
	if media == nil {
		return "text"
	}
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		if m.Voice {
			return "voice"
		}
		if m.Video {
			return "video"
		}
		// Check document attributes for sticker
		if doc, ok := m.Document.AsNotEmpty(); ok {
			for _, attr := range doc.Attributes {
				if _, isSticker := attr.(*tg.DocumentAttributeSticker); isSticker {
					return "sticker"
				}
			}
		}
		return "document"
	case *tg.MessageMediaEmpty:
		return "text"
	default:
		return "other"
	}
}
