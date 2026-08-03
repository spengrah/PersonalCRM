package google

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
)

// TestGChatSourceAdapter_WireFormat pins the GChat-specific bytes the shared
// commsadapter.Adapter produces once configured with GChat's source name and
// label. The projection, LIKE-escape and nil-propagation behaviour now live in
// backend/internal/messaging/commsadapter and are tested there; what stays
// GChat's own is WHICH source string and label the engine is wired with, since
// changing either rewrites interaction.source_ref and interaction.description
// for this source.
//
// It asserts against gchatSourceAdapter(), the same call
// NewGChatAggregationEngine makes, so the adapter under test is the adapter in
// production.
func TestGChatSourceAdapter_WireFormat(t *testing.T) {
	a := gchatSourceAdapter()

	assert.Equal(t, "gchat", a.SourceName())
	assert.Equal(t, repository.InteractionSourceGChat, a.SourceName())
	assert.Equal(t,
		"gchat:spaces/AAA:spaces/AAA/messages/1",
		a.SourceRef("spaces/AAA", "spaces/AAA/messages/1"),
	)
	assert.Equal(t, `gchat:spaces/AAA:%`, a.SourceRefPrefix("spaces/AAA"))
	assert.Equal(t, "gchat:spaces/AAA", a.PeerRef("spaces/AAA"))

	assert.Equal(t, "GChat outreach (3 messages)",
		a.Description(repository.InteractionDirectionOutbound, 3))
	assert.Equal(t, "GChat response (1 messages)",
		a.Description(repository.InteractionDirectionInbound, 1))
	assert.Equal(t, "GChat exchange (5 messages)",
		a.Description(repository.InteractionDirectionMutual, 5))
}

// TestNewGChatAggregationEngine_AcceptsNilDependencies is the construction-level
// smoke test for the migrated constructor: nil repos / bus / pool / enqueuer
// must not panic, matching the nil-safety contract the wiring and the gchat
// tests rely on.
func TestNewGChatAggregationEngine_AcceptsNilDependencies(t *testing.T) {
	e := NewGChatAggregationEngine(
		GChatBurstWindowHours, GChatReplyBridgeHours,
		nil, nil, nil, nil, nil, nil, nil,
	)
	assert.NotNil(t, e)
}
