package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// reactionEvent builds a one-to-one raw_message event whose payload's reaction
// member is the given raw JSON, or carries no reaction member when it is empty.
func reactionEvent(t *testing.T, env *ingestRawTestEnv, kind events.Kind, guid, reaction string) map[string]any {
	t.Helper()
	ev := buildRawMessageEvent(t, kind, env.pairedHostID, guid, "reaction-chat-"+guid, "+15551234567")
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(ev["payload"].(json.RawMessage), &payload))
	payload["text"] = json.RawMessage(`"Loved \"see you at noon\""`)
	payload["reply_to_guid"] = json.RawMessage(`"p:0/target-guid"`)
	payload["sent_at"] = json.RawMessage(`"2026-03-01T12:00:00Z"`)
	if reaction != "" {
		payload["reaction"] = json.RawMessage(reaction)
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	ev["payload"] = json.RawMessage(raw)
	return ev
}

// ingestReaction posts the event, requires it accepted, and returns the stored row.
func ingestReaction(t *testing.T, env *ingestRawTestEnv, kind events.Kind, reaction string) *repository.MessagesMessage {
	t.Helper()
	guid := "reaction-" + uuid.NewString()
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{reactionEvent(t, env, kind, guid, reaction)},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors: %+v", resp.Errors)
	msg, err := env.messagesRepo.GetMessage(t.Context(), guid)
	require.NoError(t, err)
	return msg
}

// spec: ING-048.reaction-facts-stored
func TestIngestReaction_StoresFactsOnTheReactionsOwnMessage(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	thumbs := "👍"

	cases := []struct {
		name     string
		kind     events.Kind
		reaction string
		want     repository.MessageReaction
	}{
		{"love", events.KindRawMessageReceived, `{"kind":"love","target_key":"target-1","removal":false}`,
			repository.MessageReaction{Kind: "love", TargetKey: "target-1"}},
		{"like", events.KindRawMessageReceived, `{"kind":"like","target_key":"target-2","removal":false}`,
			repository.MessageReaction{Kind: "like", TargetKey: "target-2"}},
		{"dislike", events.KindRawMessageReceived, `{"kind":"dislike","target_key":"target-3","removal":false}`,
			repository.MessageReaction{Kind: "dislike", TargetKey: "target-3"}},
		{"laugh", events.KindRawMessageReceived, `{"kind":"laugh","target_key":"target-4","removal":false}`,
			repository.MessageReaction{Kind: "laugh", TargetKey: "target-4"}},
		{"emphasize", events.KindRawMessageReceived, `{"kind":"emphasize","target_key":"target-5","removal":false}`,
			repository.MessageReaction{Kind: "emphasize", TargetKey: "target-5"}},
		{"question", events.KindRawMessageReceived, `{"kind":"question","target_key":"target-6","removal":false}`,
			repository.MessageReaction{Kind: "question", TargetKey: "target-6"}},
		{"emoji", events.KindRawMessageReceived, `{"kind":"emoji","emoji":"👍","target_key":"target-7","removal":false}`,
			repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: "target-7"}},
		{"sticker", events.KindRawMessageReceived, `{"kind":"sticker","target_key":"target-8","removal":false}`,
			repository.MessageReaction{Kind: "sticker", TargetKey: "target-8"}},
		{"removal of a like", events.KindRawMessageReceived, `{"kind":"like","target_key":"target-9","removal":true}`,
			repository.MessageReaction{Kind: "like", TargetKey: "target-9", Removal: true}},
		{"removal of an emoji", events.KindRawMessageReceived, `{"kind":"emoji","emoji":"👍","target_key":"target-10","removal":true}`,
			repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: "target-10", Removal: true}},
		{"sent love", events.KindRawMessageSent, `{"kind":"love","target_key":"target-11","removal":false}`,
			repository.MessageReaction{Kind: "love", TargetKey: "target-11"}},
		{"sent removal of an emoji", events.KindRawMessageSent, `{"kind":"emoji","emoji":"👍","target_key":"target-12","removal":true}`,
			repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: "target-12", Removal: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ingestReaction(t, env, tc.kind, tc.reaction)
			require.Equal(t, &tc.want, msg.Reaction)
			require.Equal(t, tc.kind == events.KindRawMessageSent, msg.IsOutgoing)
		})
	}
}

// withoutIdentity blanks the columns that tell two stored copies of one message
// apart, so rows ingested from equal payloads compare equal.
func withoutIdentity(m *repository.MessagesMessage) repository.MessagesMessage {
	out := *m
	out.ID, out.Guid, out.ChatGuid, out.CreatedAt = uuid.Nil, "", "", time.Time{}
	return out
}

// spec: ING-048.no-reaction-stores-no-facts
func TestIngestReaction_MessageWithoutAValidReactionStoresNoFacts(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()

	absent := ingestReaction(t, env, events.KindRawMessageReceived, "")
	require.Nil(t, absent.Reaction, "an older daemon's message stores no facts")
	require.Equal(t, `Loved "see you at noon"`, *absent.Text)
	require.Equal(t, "text", absent.MessageType)
	require.Equal(t, "p:0/target-guid", *absent.ReplyToGuid, "the reply link is stored as sent")
	require.Equal(t, env.pairedContactA, *absent.MatchedContactID)
	require.False(t, absent.IsOutgoing)

	cases := []struct{ name, reaction string }{
		{"null", `null`},
		{"kind outside the set", `{"kind":"wow","target_key":"target-1","removal":false}`},
		{"empty target key", `{"kind":"like","target_key":"","removal":false}`},
		{"missing target key", `{"kind":"like","removal":false}`},
		{"emoji kind without an emoji", `{"kind":"emoji","target_key":"target-1","removal":false}`},
		{"emoji kind with an empty emoji", `{"kind":"emoji","emoji":"","target_key":"target-1","removal":false}`},
		{"emoji on a non-emoji kind", `{"kind":"love","emoji":"👍","target_key":"target-1","removal":false}`},
		{"removal missing", `{"kind":"like","target_key":"target-1"}`},
		{"a member of the wrong type", `{"kind":"like","target_key":"target-1","removal":"yes"}`},
		{"not an object", `"like"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ingestReaction(t, env, events.KindRawMessageReceived, tc.reaction)
			require.Nil(t, msg.Reaction)
			require.Equal(t, withoutIdentity(absent), withoutIdentity(msg), "every other column is stored as without the field")
		})
	}
}
