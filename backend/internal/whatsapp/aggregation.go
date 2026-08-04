package whatsapp

import (
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/messaging/commsadapter"
	"personal-crm/backend/internal/repository"
)

// whatsappLabel is the human product name that appears in interaction
// descriptions ("WhatsApp outreach (3 messages)"). Declared beside the
// constructor because the (source, label) pair is the whole of WhatsApp's
// adapter configuration; the source half is syncSource (manager.go).
const whatsappLabel = "WhatsApp"

// whatsappSourceAdapter returns the shared comms adapter configured for
// WhatsApp. Both NewAggregationEngine and the wire-format test call it, so the
// bytes the test pins are the bytes production writes. Unexported: every
// cross-package consumer reaches the formats through NewAggregationEngine.
func whatsappSourceAdapter() commsadapter.Adapter {
	return commsadapter.NewAdapter(syncSource, whatsappLabel)
}

// NewAggregationEngine constructs the shared aggregator engine configured for
// the WhatsApp source over the comms_message table. Returns the shared
// *aggregation.Engine directly (NO facade): a WhatsApp chat id is the chat JID,
// already a string, so the engine's native
// AggregateForContact(ctx, contactID, chatID string) signature fits.
//
// The burst/reply windows are parameters rather than package constants because
// WhatsApp's are operator-tunable (WHATSAPP_BURST_WINDOW_HOURS /
// WHATSAPP_REPLY_BRIDGE_HOURS, defaults in config).
//
// nil bus / pool / enqueuer are safe (see commsadapter.NewEngine).
func NewAggregationEngine(
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
		whatsappSourceAdapter(),
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
