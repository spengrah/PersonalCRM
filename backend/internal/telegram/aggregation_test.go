package telegram

import (
	"strconv"
	"testing"

	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAggregationEngine_AcceptsNilEventBus is a defensive shim-level
// smoke test that confirms the explicit "if bus != nil { pub = bus }"
// nil-iface conversion is wired correctly. Without it, passing a nil
// *events.Bus would defeat the engine's publisher==nil guard.
//
// The behavioural contract (createInteractionForSession returns the
// publisher-required error when publisher is the nil interface value)
// is locked in by TestEngine_FakeSource_NilPublisher_Errors in the
// shared package.
func TestNewAggregationEngine_AcceptsNilEventBus(t *testing.T) {
	// Pure construction: no method invoked, so nil concrete repos are
	// safe. The shim simply wraps them in adapter structs.
	e := NewAggregationEngine(2, 48, nil, nil, nil, nil, nil)
	require.NotNil(t, e)
	require.NotNil(t, e.engine)
}

// TestTelegramAdapter_WireFormat locks in the Telegram-specific
// source_ref / peer_ref / description bytes. The shared package has a
// telegramShapedAdapter test (TestSessionKey_Stability_TelegramShape)
// that asserts the same on a private copy; this test ensures the
// production telegramAdapter (which is the one wired into
// NewAggregationEngine) also produces the contract bytes.
func TestTelegramAdapter_WireFormat(t *testing.T) {
	a := telegramAdapter{}
	assert.Equal(t, repository.InteractionSourceTelegram, a.SourceName())
	assert.Equal(t, "tg:12345:50001", a.SourceRef("12345", "50001"))
	assert.Equal(t, "tg:12345:%", a.SourceRefPrefix("12345"))
	assert.Equal(t, "tg:12345", a.PeerRef("12345"))
	assert.Equal(t, "Telegram outreach (3 messages)", a.Description(repository.InteractionDirectionOutbound, 3))
	assert.Equal(t, "Telegram response (1 messages)", a.Description(repository.InteractionDirectionInbound, 1))
	assert.Equal(t, "Telegram exchange (5 messages)", a.Description(repository.InteractionDirectionMutual, 5))
}

// TestMapTelegramMessage covers the row-mapping helper. The critical
// invariant is that InteractionID flows through — without it, the
// cross-batch explicit reply bridge silently fails.
func TestMapTelegramMessage(t *testing.T) {
	reply := int32(42)
	interactionID := uuid.New()
	src := repository.TelegramMessage{
		ID:                uuid.New(),
		TelegramMessageID: 9001,
		TelegramChatID:    8675309,
		IsOutgoing:        true,
		ReplyToMsgID:      &reply,
		InteractionID:     &interactionID,
	}

	got := mapTelegramMessage(src)
	assert.Equal(t, src.ID, got.ID)
	assert.Equal(t, strconv.FormatInt(src.TelegramChatID, 10), got.ChatID)
	assert.True(t, got.IsOutgoing)
	assert.Equal(t, strconv.Itoa(int(src.TelegramMessageID)), got.ExternalID)
	require.NotNil(t, got.ReplyTargetID)
	assert.Equal(t, strconv.Itoa(int(reply)), *got.ReplyTargetID)
	require.NotNil(t, got.InteractionID)
	assert.Equal(t, interactionID, *got.InteractionID)

	// Nil reply / nil interaction.
	src2 := repository.TelegramMessage{
		ID:                uuid.New(),
		TelegramMessageID: 1,
		TelegramChatID:    2,
		IsOutgoing:        false,
	}
	got2 := mapTelegramMessage(src2)
	assert.Nil(t, got2.ReplyTargetID)
	assert.Nil(t, got2.InteractionID)
}

// Compile-time guard: telegramMessageStoreAdapter satisfies
// aggregation.MessageStore and interactionFinderAdapter satisfies
// aggregation.InteractionFinder. The test references the interface
// types so go vet flags mismatches at build time.
var _ aggregation.MessageStore = (*telegramMessageStoreAdapter)(nil)
var _ aggregation.InteractionFinder = (*interactionFinderAdapter)(nil)
