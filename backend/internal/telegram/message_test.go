package telegram

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const selfID int64 = 111111

func makeEntities(users map[int64]*tg.User, chats map[int64]*tg.Chat) tg.Entities {
	return tg.Entities{Users: users, Chats: chats}
}

func TestParseMessage_TextOnly(t *testing.T) {
	msg := &tg.Message{
		ID:      1001,
		Message: "Hello world",
		Out:     false,
		Date:    1700000000,
		PeerID:  &tg.PeerUser{UserID: 222},
	}
	entities := makeEntities(map[int64]*tg.User{
		222: {ID: 222, FirstName: "Alice", Username: "alice"},
	}, nil)

	parsed := ParseMessage(msg, entities, selfID)
	require.NotNil(t, parsed)
	assert.Equal(t, int32(1001), parsed.TelegramMessageID)
	assert.Equal(t, int64(222), parsed.TelegramChatID)
	assert.Equal(t, "private", parsed.ChatType)
	assert.Equal(t, "text", parsed.MessageType)
	assert.Equal(t, "Hello world", *parsed.MessageText)
	assert.False(t, parsed.IsOutgoing)
	assert.Equal(t, "alice", *parsed.PeerUsername)
	assert.Equal(t, "Alice", *parsed.PeerFirstName)
}

func TestParseMessage_WithMedia(t *testing.T) {
	tests := []struct {
		name     string
		media    tg.MessageMediaClass
		expected string
	}{
		{"photo", &tg.MessageMediaPhoto{}, "photo"},
		{"voice", &tg.MessageMediaDocument{Voice: true}, "voice"},
		{"video", &tg.MessageMediaDocument{Video: true}, "video"},
		{"document", &tg.MessageMediaDocument{}, "document"},
		{"other", &tg.MessageMediaGeo{}, "other"},
		{"empty", &tg.MessageMediaEmpty{}, "text"},
		{"nil", nil, "text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &tg.Message{
				ID:     1,
				Date:   1700000000,
				PeerID: &tg.PeerUser{UserID: 222},
				Media:  tt.media,
			}
			parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil), selfID)
			require.NotNil(t, parsed)
			assert.Equal(t, tt.expected, parsed.MessageType)
		})
	}
}

func TestParseMessage_Sticker(t *testing.T) {
	msg := &tg.Message{
		ID:     1,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: 222},
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeSticker{},
				},
			},
		},
	}
	parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil), selfID)
	require.NotNil(t, parsed)
	assert.Equal(t, "sticker", parsed.MessageType)
}

func TestParseMessage_WithReplyTo(t *testing.T) {
	header := &tg.MessageReplyHeader{
		ReplyToMsgID: 500,
	}
	header.SetFlags()
	header.SetReplyToMsgID(500)

	msg := &tg.Message{
		ID:     1001,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: 222},
	}
	msg.SetReplyTo(header)

	entities := makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil)
	parsed := ParseMessage(msg, entities, selfID)
	require.NotNil(t, parsed)
	require.NotNil(t, parsed.ReplyToMsgID, "ReplyToMsgID should be extracted")
	assert.Equal(t, int32(500), *parsed.ReplyToMsgID)
}

func TestParseMessage_EditedMessage(t *testing.T) {
	msg := &tg.Message{
		ID:     1001,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: 222},
	}
	msg.SetEditDate(1700001000)

	entities := makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil)
	parsed := ParseMessage(msg, entities, selfID)
	require.NotNil(t, parsed)
	require.NotNil(t, parsed.EditedAt)
}

func TestParseMessage_OutgoingMessage(t *testing.T) {
	msg := &tg.Message{
		ID:     1001,
		Out:    true,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: 222},
	}
	parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil), selfID)
	require.NotNil(t, parsed)
	assert.True(t, parsed.IsOutgoing)
}

func TestParseMessage_GroupChat(t *testing.T) {
	msg := &tg.Message{
		ID:     1001,
		Date:   1700000000,
		PeerID: &tg.PeerChat{ChatID: 333},
	}
	msg.SetFromID(&tg.PeerUser{UserID: 444})

	entities := makeEntities(
		map[int64]*tg.User{444: {ID: 444, FirstName: "Bob", Username: "bob"}},
		map[int64]*tg.Chat{333: {ID: 333, Title: "Test Group"}},
	)
	parsed := ParseMessage(msg, entities, selfID)
	require.NotNil(t, parsed)
	assert.Equal(t, "group", parsed.ChatType)
	assert.Equal(t, int64(333), parsed.TelegramChatID)
	assert.Equal(t, "Test Group", *parsed.ChatTitle)
	assert.Equal(t, "bob", *parsed.PeerUsername)
}

func TestParseMessage_SkipSelfChat(t *testing.T) {
	msg := &tg.Message{
		ID:     1,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: selfID},
	}
	parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{selfID: {ID: selfID}}, nil), selfID)
	assert.Nil(t, parsed)
}

func TestParseMessage_SkipBot(t *testing.T) {
	msg := &tg.Message{
		ID:     1,
		Date:   1700000000,
		PeerID: &tg.PeerUser{UserID: 555},
	}
	parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{555: {ID: 555, Bot: true}}, nil), selfID)
	assert.Nil(t, parsed)
}

func TestParseMessage_SkipChannel(t *testing.T) {
	msg := &tg.Message{
		ID:     1,
		Date:   1700000000,
		PeerID: &tg.PeerChannel{ChannelID: 666},
	}
	parsed := ParseMessage(msg, makeEntities(nil, nil), selfID)
	assert.Nil(t, parsed)
}

func TestParseMessage_MediaCaption(t *testing.T) {
	msg := &tg.Message{
		ID:      1,
		Date:    1700000000,
		Message: "Look at this photo",
		PeerID:  &tg.PeerUser{UserID: 222},
		Media:   &tg.MessageMediaPhoto{},
	}
	parsed := ParseMessage(msg, makeEntities(map[int64]*tg.User{222: {ID: 222}}, nil), selfID)
	require.NotNil(t, parsed)
	assert.Equal(t, "photo", parsed.MessageType)
	assert.Equal(t, "Look at this photo", *parsed.MessageText)
}
