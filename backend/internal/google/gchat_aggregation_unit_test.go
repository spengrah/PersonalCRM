package google

import (
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGChatAdapter_Formatting pins the wire-format hooks: SourceName, SourceRef,
// PeerRef, and Description for each direction.
func TestGChatAdapter_Formatting(t *testing.T) {
	a := gchatAdapter{}

	assert.Equal(t, "gchat", a.SourceName())
	assert.Equal(t,
		"gchat:spaces/AAA:spaces/AAA/messages/1",
		a.SourceRef("spaces/AAA", "spaces/AAA/messages/1"),
	)
	assert.Equal(t, "gchat:spaces/AAA", a.PeerRef("spaces/AAA"))

	assert.Equal(t, "GChat outreach (3 messages)",
		a.Description(repository.InteractionDirectionOutbound, 3))
	assert.Equal(t, "GChat response (1 messages)",
		a.Description(repository.InteractionDirectionInbound, 1))
	assert.Equal(t, "GChat exchange (5 messages)",
		a.Description(repository.InteractionDirectionMutual, 5))
	// Unknown direction falls back to "exchange".
	assert.Equal(t, "GChat exchange (2 messages)",
		a.Description("something-else", 2))
}

// TestGChatAdapter_SourceRefPrefixEscape proves the LIKE-escape fires
// UNCONDITIONALLY (spec §5.X.4): benign inputs pass through unchanged, while
// `_`, `%`, and `\` are escaped so they cannot act as LIKE wildcards.
func TestGChatAdapter_SourceRefPrefixEscape(t *testing.T) {
	a := gchatAdapter{}

	cases := []struct {
		name string
		chat string
		want string
	}{
		{
			name: "benign space resource name unchanged",
			chat: "spaces/AAA",
			want: `gchat:spaces/AAA:%`,
		},
		{
			name: "underscore and percent escaped",
			chat: "a_b%c",
			want: `gchat:a\_b\%c:%`,
		},
		{
			name: "backslash doubled",
			chat: `a\b`,
			want: `gchat:a\\b:%`,
		},
		{
			name: "all special chars together",
			chat: `x_%\y`,
			want: `gchat:x\_\%\\y:%`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, a.SourceRefPrefix(tc.chat))
		})
	}
}

// TestMapCommsMessage_Projection covers the repository.CommsMessage →
// aggregation.Message projection: ChatID from thread_id, IsOutgoing from
// direction, ExternalID passthrough, claim/interaction fields preserved, and
// the defensive nil thread_id case.
func TestMapCommsMessage_Projection(t *testing.T) {
	id := uuid.New()
	interactionID := uuid.New()
	claimedAt := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
	sessionRef := "gchat:spaces/AAA:spaces/AAA/messages/1"
	sentAt := accelerated.GetCurrentTime().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	thread := "spaces/AAA"

	t.Run("outbound row with all fields", func(t *testing.T) {
		m := repository.CommsMessage{
			ID:                id,
			Source:            "gchat",
			ExternalID:        "spaces/AAA/messages/1",
			ThreadID:          &thread,
			Direction:         repository.InteractionDirectionOutbound,
			SentAt:            sentAt,
			InteractionID:     &interactionID,
			ClaimedAt:         &claimedAt,
			ClaimedSessionRef: &sessionRef,
		}
		got := mapCommsMessage(m)
		assert.Equal(t, id, got.ID)
		assert.Equal(t, "spaces/AAA", got.ChatID)
		assert.True(t, got.IsOutgoing)
		assert.Equal(t, sentAt, got.SentAt)
		assert.Equal(t, "spaces/AAA/messages/1", got.ExternalID)
		require.NotNil(t, got.InteractionID)
		assert.Equal(t, interactionID, *got.InteractionID)
		require.NotNil(t, got.ClaimedAt)
		assert.Equal(t, claimedAt, *got.ClaimedAt)
		require.NotNil(t, got.ClaimedSessionRef)
		assert.Equal(t, sessionRef, *got.ClaimedSessionRef)
		assert.Nil(t, got.ReplyTargetID)
	})

	t.Run("inbound row, nil claim/interaction fields preserved as nil", func(t *testing.T) {
		m := repository.CommsMessage{
			ID:         id,
			Source:     "gchat",
			ExternalID: "spaces/AAA/messages/2",
			ThreadID:   &thread,
			Direction:  repository.InteractionDirectionInbound,
			SentAt:     sentAt,
		}
		got := mapCommsMessage(m)
		assert.False(t, got.IsOutgoing)
		assert.Nil(t, got.InteractionID)
		assert.Nil(t, got.ClaimedAt)
		assert.Nil(t, got.ClaimedSessionRef)
	})

	t.Run("nil thread_id yields empty ChatID (defensive)", func(t *testing.T) {
		m := repository.CommsMessage{
			ID:         id,
			Source:     "gchat",
			ExternalID: "spaces/AAA/messages/3",
			ThreadID:   nil,
			Direction:  repository.InteractionDirectionInbound,
			SentAt:     sentAt,
		}
		got := mapCommsMessage(m)
		assert.Equal(t, "", got.ChatID)
	})
}

// TestMapCommsMessage_ReplyTargetID covers parsing the reply target external id
// from source_metadata: present non-empty string → set; absent/empty/wrong-type
// → nil.
func TestMapCommsMessage_ReplyTargetID(t *testing.T) {
	base := repository.CommsMessage{
		ID:         uuid.New(),
		Source:     "gchat",
		ExternalID: "spaces/AAA/messages/9",
		ThreadID:   strPtr("spaces/AAA"),
		Direction:  repository.InteractionDirectionInbound,
		SentAt:     accelerated.GetCurrentTime().UTC(),
	}

	withMeta := func(m map[string]any) []byte {
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return b
	}

	t.Run("present non-empty string", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{
			replyTargetMetadataKey: "spaces/AAA/messages/parent",
		})
		got := mapCommsMessage(m)
		require.NotNil(t, got.ReplyTargetID)
		assert.Equal(t, "spaces/AAA/messages/parent", *got.ReplyTargetID)
	})

	t.Run("key absent", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{"other": "x"})
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
	})

	t.Run("key present but empty string", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{replyTargetMetadataKey: ""})
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
	})

	t.Run("key present but wrong type", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{replyTargetMetadataKey: 123})
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
	})

	t.Run("nil/empty metadata", func(t *testing.T) {
		m := base
		m.SourceMetadata = nil
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
		m.SourceMetadata = []byte{}
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		m := base
		m.SourceMetadata = []byte("{not json")
		assert.Nil(t, mapCommsMessage(m).ReplyTargetID)
	})
}

// TestGChatStoreAdapter_SatisfiesMessageStore is a compile-time assertion that
// commsMessageStoreAdapter implements the aggregation.MessageStore interface
// (catches a signature drift on either side).
func TestGChatStoreAdapter_SatisfiesMessageStore(t *testing.T) {
	var _ aggregation.MessageStore = (*commsMessageStoreAdapter)(nil)
	var _ aggregation.SourceAdapter = gchatAdapter{}
}
