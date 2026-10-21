//go:build integration_testdb

package threadctx_test

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The decision model's thread-content read carries each message's reaction
// facts beside it, in the same read, with the target in message_key form.
// spec: ING-049
func TestListThreadContent_CarriesReactionFactsBesideEachMessage(t *testing.T) {
	t.Parallel()
	e := newBuilderEnv(t)
	content := repository.NewThreadContentRepository(e.database.Queries)
	messages := repository.NewMessagesMessageRepository(e.database.Queries)
	contact := e.contact(t)
	chat := "chat-" + uuid.NewString()
	thread := e.venue(t, repository.InteractionSourceMessages, repository.VenueKindDM, chat)
	thumbs := "👍"
	target := "target-" + uuid.NewString()
	store := func(guid string, outgoing bool, at time.Time, reaction *repository.MessageReaction) {
		t.Helper()
		text := "body of " + guid
		_, err := messages.UpsertMessage(e.ctx, repository.UpsertMessagesMessageParams{
			Guid: guid, ChatGuid: chat, PeerHandle: "+15550001111", Text: &text, MessageType: "text",
			SentAt: at, IsOutgoing: outgoing, MatchedContactID: &contact, Reaction: reaction,
		})
		require.NoError(t, err)
	}
	store(target, true, e.at(1), nil)
	store("love-"+target, false, e.at(2), &repository.MessageReaction{Kind: "love", TargetKey: target})
	store("unlove-"+target, false, e.at(3), &repository.MessageReaction{Kind: "love", TargetKey: target, Removal: true})
	store("emoji-"+target, false, e.at(4), &repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: target})

	rows, err := content.ListThreadContent(e.ctx, thread, e.at(4))
	require.NoError(t, err)
	require.Len(t, rows, 4)
	byKey := map[string]repository.ThreadContentRow{}
	for _, row := range rows {
		byKey[row.MessageKey] = row
	}
	targetKey := "messages:" + target

	t.Run("a non-reaction iMessage row carries no facts", func(t *testing.T) {
		require.Contains(t, byKey, targetKey)
		require.Nil(t, byKey[targetKey].Reaction)
	})
	t.Run("a reaction carries its kind and its target's message_key", func(t *testing.T) {
		require.Equal(t, &repository.MessageReaction{Kind: "love", TargetKey: targetKey}, byKey["messages:love-"+target].Reaction)
	})
	t.Run("a removal carries its removal flag", func(t *testing.T) {
		require.Equal(t, &repository.MessageReaction{Kind: "love", TargetKey: targetKey, Removal: true}, byKey["messages:unlove-"+target].Reaction)
	})
	t.Run("an emoji reaction carries its emoji", func(t *testing.T) {
		require.Equal(t, &repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: targetKey}, byKey["messages:emoji-"+target].Reaction)
	})

	t.Run("rows of other sources carry no facts", func(t *testing.T) {
		gmailThread := "thread-" + uuid.NewString()
		email := e.venue(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, gmailThread)
		e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "a@example.test", body: "thanks", contact: contact, at: e.at(1)})
		telegramChat := rand.Int64N(1 << 40)
		telegram := e.venue(t, repository.InteractionSourceTelegram, repository.VenueKindDM, strconv.FormatInt(telegramChat, 10))
		e.telegram(t, telegramChat, "private", 1, 301, "Sam", &contact, e.at(1))

		for _, venue := range []uuid.UUID{email, telegram} {
			rows, err := content.ListThreadContent(e.ctx, venue, e.at(1))
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Nil(t, rows[0].Reaction)
		}
	})
}
