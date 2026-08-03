package google

import (
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/messaging/commsadapter"
	"personal-crm/backend/internal/repository"
)

// GChatSourceName is the source string written into interaction.source,
// event.source, and the river job args for the Google Chat source. It MUST
// agree with the interaction_source_check CHECK value and the
// repository.InteractionSourceGChat constant.
const GChatSourceName = repository.InteractionSourceGChat

// gchatLabel is the human product name that appears in interaction
// descriptions ("GChat outreach (3 messages)"). Declared beside the source name
// because the pair is the whole of GChat's adapter configuration.
const gchatLabel = "GChat"

// GChatBurstWindowHours / GChatReplyBridgeHours are the aggregation windows the
// GChat engine runs on: consecutive same-direction messages inside the burst
// window form one session, and an inbound session that follows an outbound one
// inside the reply-bridge window merges into it and turns it mutual.
//
// They are declared HERE, beside the constructor, because they are not free
// parameters in practice: every caller passes the same pair, and a caller that
// SIZES ITS INPUT to them — the synthetic seed spaces a burst's opening messages
// to fall inside the burst window, and predicts how many rows a conversation
// collapses into — is silently wrong the moment a wiring literal moves. Read
// them; do not re-declare them.
const (
	GChatBurstWindowHours = 2
	GChatReplyBridgeHours = 48
)

// gchatSourceAdapter returns the shared comms adapter configured for Google
// Chat. Both NewGChatAggregationEngine and the wire-format test call it, so the
// bytes the test pins are the bytes production writes.
func gchatSourceAdapter() commsadapter.Adapter {
	return commsadapter.NewAdapter(GChatSourceName, gchatLabel)
}

// NewGChatAggregationEngine constructs a shared aggregator engine configured
// for the Google Chat source over the comms_message table. Returns the shared
// *aggregation.Engine directly (NO facade): GChat's chat ID is the opaque space
// resource name (a string), so the engine's native
// AggregateForContact(ctx, contactID, chatID string) signature already takes a
// string. Production wiring passes all of (pool, eventBus, enqueuer); nil bus /
// pool / enqueuer are safe (see commsadapter.NewEngine).
func NewGChatAggregationEngine(
	burstWindowHours, replyBridgeHours int,
	commsRepo *repository.CommsMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter aggregation.InteractionPromoter,
	extender aggregation.InteractionExtender,
	eventBus *events.Bus,
	pool aggregation.TxBeginner,
	enqueuer aggregation.ConsumerJobEnqueuer,
) *aggregation.Engine {
	return commsadapter.NewEngine(
		gchatSourceAdapter(),
		burstWindowHours,
		replyBridgeHours,
		commsRepo,
		interactionRepo,
		promoter,
		extender,
		eventBus,
		pool,
		enqueuer,
	)
}
