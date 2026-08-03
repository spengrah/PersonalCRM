package commsadapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdapter_WireFormat pins the wire-format hooks for a configured source:
// SourceName, SourceRef, PeerRef, and Description for each direction. These
// bytes are the interaction.source_ref / event PeerRef / interaction.description
// values every comms source writes, so a change here is a data-format change.
func TestAdapter_WireFormat(t *testing.T) {
	a := NewAdapter("gchat", "GChat")

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

// TestAdapter_SourceRefPrefixEscapesLikeWildcards proves the LIKE-escape fires
// UNCONDITIONALLY: benign inputs pass through unchanged, while `_`, `%`, and `\`
// are escaped so they cannot act as LIKE wildcards. PeerRef of the same chat id
// is deliberately NOT escaped — the reenqueuer strips it literally.
func TestAdapter_SourceRefPrefixEscapesLikeWildcards(t *testing.T) {
	a := NewAdapter("gchat", "GChat")

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
			// PeerRef must NOT escape: the consumer reenqueuer recovers the
			// chat id with a literal strip, so an escaped peer ref would
			// yield a chat id that matches no row.
			assert.Equal(t, "gchat:"+tc.chat, a.PeerRef(tc.chat))
		})
	}
}

// TestAdapter_PrefixEqualsSourceName is the mechanical guard for the package's
// central invariant: every ref an Adapter emits is prefixed with its own
// SourceName plus ":". consumer.CommsAggregatorReenqueuer recovers the chat id
// by stripping exactly that, so a source whose prefix drifts from its source
// string silently loses post-record re-aggregation.
func TestAdapter_PrefixEqualsSourceName(t *testing.T) {
	for _, a := range []Adapter{
		NewAdapter("gchat", "GChat"),
		NewAdapter("whatsapp", "WhatsApp"),
	} {
		t.Run(a.SourceName(), func(t *testing.T) {
			want := a.SourceName() + ":"
			assert.True(t, strings.HasPrefix(a.SourceRef("chat-1", "msg-1"), want),
				"SourceRef must start with %q, got %q", want, a.SourceRef("chat-1", "msg-1"))
			assert.True(t, strings.HasPrefix(a.SourceRefPrefix("chat-1"), want),
				"SourceRefPrefix must start with %q, got %q", want, a.SourceRefPrefix("chat-1"))
			assert.True(t, strings.HasPrefix(a.PeerRef("chat-1"), want),
				"PeerRef must start with %q, got %q", want, a.PeerRef("chat-1"))
			// The reenqueuer's exact recovery step, reproduced.
			assert.Equal(t, "chat-1", strings.TrimPrefix(a.PeerRef("chat-1"), want))
		})
	}
}

// TestMapMessage_Projection covers the repository.CommsMessage →
// aggregation.Message projection: ChatID from thread_id, IsOutgoing from
// direction, ExternalID passthrough, claim/interaction fields preserved, and the
// defensive nil thread_id case.
func TestMapMessage_Projection(t *testing.T) {
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
		got := MapMessage(m)
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
		got := MapMessage(m)
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
		got := MapMessage(m)
		assert.Equal(t, "", got.ChatID)
	})
}

// TestMapMessage_ReplyTargetID covers parsing the reply target external id from
// source_metadata: present non-empty string → set; absent/empty/wrong-type →
// nil.
func TestMapMessage_ReplyTargetID(t *testing.T) {
	thread := "spaces/AAA"
	base := repository.CommsMessage{
		ID:         uuid.New(),
		Source:     "gchat",
		ExternalID: "spaces/AAA/messages/9",
		ThreadID:   &thread,
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
			ReplyTargetMetadataKey: "spaces/AAA/messages/parent",
		})
		got := MapMessage(m)
		require.NotNil(t, got.ReplyTargetID)
		assert.Equal(t, "spaces/AAA/messages/parent", *got.ReplyTargetID)
	})

	t.Run("key absent", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{"other": "x"})
		assert.Nil(t, MapMessage(m).ReplyTargetID)
	})

	t.Run("key present but empty string", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{ReplyTargetMetadataKey: ""})
		assert.Nil(t, MapMessage(m).ReplyTargetID)
	})

	t.Run("key present but wrong type", func(t *testing.T) {
		m := base
		m.SourceMetadata = withMeta(map[string]any{ReplyTargetMetadataKey: 123})
		assert.Nil(t, MapMessage(m).ReplyTargetID)
	})

	t.Run("nil/empty metadata", func(t *testing.T) {
		m := base
		m.SourceMetadata = nil
		assert.Nil(t, MapMessage(m).ReplyTargetID)
		m.SourceMetadata = []byte{}
		assert.Nil(t, MapMessage(m).ReplyTargetID)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		m := base
		m.SourceMetadata = []byte("{not json")
		assert.Nil(t, MapMessage(m).ReplyTargetID)
	})
}

// TestStore_SatisfiesMessageStore is a compile-time assertion that StoreAdapter
// implements aggregation.MessageStore and Adapter implements
// aggregation.SourceAdapter (catches a signature drift on either side).
func TestStore_SatisfiesMessageStore(t *testing.T) {
	var _ aggregation.MessageStore = (*StoreAdapter)(nil)
	var _ aggregation.SourceAdapter = NewAdapter("x", "X")
}

// TestEventLookup_NilBusReturnsNilInterface is the typed-nil trap the
// aggregation package's EventPublisher note warns about: returning
// (*busEventLookup)(nil) would make the engine's `lookup == nil` guard see a
// NON-nil interface and take the wrong branch. The `== nil` comparison is the
// assertion a typed-nil regression fails.
func TestEventLookup_NilBusReturnsNilInterface(t *testing.T) {
	got := EventLookup(nil)
	assert.Nil(t, got)
	assert.True(t, got == nil, "EventLookup(nil) must be the untyped-nil interface value, got %#v", got)
}

// TestPublisher_NilBusReturnsNilInterface — same contract for the publisher
// side; a typed-nil here would silently bypass the engine's publisher==nil
// guard in the session-create path.
func TestPublisher_NilBusReturnsNilInterface(t *testing.T) {
	got := Publisher(nil)
	assert.Nil(t, got)
	assert.True(t, got == nil, "Publisher(nil) must be the untyped-nil interface value, got %#v", got)
}

// TestNewEngine_AcceptsNilBusPoolEnqueuer is the construction-level smoke test:
// pure construction with nil repos/bus/pool/enqueuer must not panic and must
// return a usable engine. Mirrors telegram's
// TestNewAggregationEngine_AcceptsNilEventBus / _AcceptsNilPool.
func TestNewEngine_AcceptsNilBusPoolEnqueuer(t *testing.T) {
	require.NotPanics(t, func() {
		e := NewEngine(
			NewAdapter("gchat", "GChat"),
			2, 48,
			nil, nil, nil, nil,
			nil, nil, nil,
		)
		require.NotNil(t, e)
	})
}
