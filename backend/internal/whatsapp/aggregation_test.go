package whatsapp

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWhatsAppSourceAdapter_WireFormat pins the exact bytes the WhatsApp
// aggregation engine writes into interaction.source, interaction.source_ref,
// the event PeerRef and interaction.description. In-package so it calls the
// same unexported constructor NewAggregationEngine uses — the bytes pinned here
// are the bytes production writes.
//
// spec: WHA-040.source-is-whatsapp
// spec: WHA-040.source-ref-is-chat-scoped
// spec: WHA-040.description-names-whatsapp
func TestWhatsAppSourceAdapter_WireFormat(t *testing.T) {
	t.Parallel()

	adapter := whatsappSourceAdapter()

	require.Equal(t, "whatsapp", adapter.SourceName())
	require.Equal(t, repository.InteractionSourceWhatsApp, adapter.SourceName())

	assert.Equal(t, "whatsapp:123-456@g.us:ABC", adapter.SourceRef("123-456@g.us", "ABC"))
	assert.Equal(t, "whatsapp:12045550107@s.whatsapp.net:ABC", adapter.SourceRef("12045550107@s.whatsapp.net", "ABC"))

	assert.Equal(t, "WhatsApp outreach (3 messages)", adapter.Description(repository.InteractionDirectionOutbound, 3))
	assert.Equal(t, "WhatsApp response (2 messages)", adapter.Description(repository.InteractionDirectionInbound, 2))
	assert.Equal(t, "WhatsApp exchange (5 messages)", adapter.Description("mutual", 5))
}

// TestWhatsAppSourceAdapter_PeerRefRoundTrip guards R5.1: the comms reenqueuer
// recovers the chat id by stripping "<source>:" from the event PeerRef
// (consumer.parseCommsPeerRef), a literal prefix strip with no escaping. A chat
// JID containing a ':' would truncate. The ingest path normalizes to non-AD
// form before staging, which strips the ':<device>' suffix, so every JID that
// can reach the engine round-trips — this pins that for the two forms whose
// user part is not a bare subscriber number (group and LID).
func TestWhatsAppSourceAdapter_PeerRefRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := whatsappSourceAdapter()
	const prefix = "whatsapp:"

	for _, chatJID := range []string{
		"12045550107@s.whatsapp.net",
		"123456789-987654321@g.us",
		"88776655443322@lid",
	} {
		peerRef := adapter.PeerRef(chatJID)
		require.True(t, strings.HasPrefix(peerRef, prefix), "PeerRef %q must carry the source prefix", peerRef)
		// Mirrors consumer/comms_aggregator_reenqueuer.go's parseCommsPeerRef.
		assert.Equal(t, chatJID, peerRef[len(prefix):], "PeerRef must round-trip through the reenqueuer's strip rule")
	}
}
