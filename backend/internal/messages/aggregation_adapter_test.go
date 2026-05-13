package messages

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

func TestMessagesAdapter_SourceName(t *testing.T) {
	require.Equal(t, "messages", messagesAdapter{}.SourceName())
}

func TestMessagesAdapter_SourceRef_Format(t *testing.T) {
	got := messagesAdapter{}.SourceRef("iMessage;-;chat-uuid", "guid-1")
	require.Equal(t, "messages:iMessage;-;chat-uuid:guid-1", got)
}

func TestMessagesAdapter_PeerRef_Format(t *testing.T) {
	got := messagesAdapter{}.PeerRef("iMessage;-;chat-uuid")
	require.Equal(t, "messages:iMessage;-;chat-uuid", got)
}

func TestMessagesAdapter_Description_Outbound(t *testing.T) {
	got := messagesAdapter{}.Description(repository.InteractionDirectionOutbound, 3)
	require.Equal(t, "Messages outreach (3 messages)", got)
}

func TestMessagesAdapter_Description_Inbound(t *testing.T) {
	got := messagesAdapter{}.Description(repository.InteractionDirectionInbound, 1)
	require.Equal(t, "Messages response (1 messages)", got)
}

func TestMessagesAdapter_Description_Mutual_DefaultsToExchange(t *testing.T) {
	got := messagesAdapter{}.Description(repository.InteractionDirectionMutual, 5)
	require.Equal(t, "Messages exchange (5 messages)", got)
}

// TestMessagesAdapter_SourceRefPrefix_EscapesLikeWildcards is the
// load-bearing assertion from Codex P2: the prefix MUST escape `_`
// and `%` so a chat_guid containing those wildcards doesn't false-
// match unrelated rows when used as a LIKE pattern. The escape
// character is `\` and the corresponding sqlc queries use
// `LIKE pattern ESCAPE '\'`.
func TestMessagesAdapter_SourceRefPrefix_EscapesLikeWildcards(t *testing.T) {
	cases := []struct {
		name    string
		chatID  string
		want    string
		hasWild string // substring that must NOT appear literally
	}{
		{
			name:    "underscore_in_chat_guid",
			chatID:  "iMessage;-;_chat-uuid_",
			want:    `messages:iMessage;-;\_chat-uuid\_:%`,
			hasWild: "",
		},
		{
			name:   "percent_in_chat_guid",
			chatID: "weird%pattern",
			want:   `messages:weird\%pattern:%`,
		},
		{
			name:   "backslash_belt_and_suspenders",
			chatID: `c\xyz`,
			want:   `messages:c\\xyz:%`,
		},
		{
			name:   "numeric_telegram_style_passes_through",
			chatID: "12345",
			want:   "messages:12345:%",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, messagesAdapter{}.SourceRefPrefix(tc.chatID))
		})
	}
}
