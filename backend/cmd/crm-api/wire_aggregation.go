package main

import (
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// aggregationStack holds the messages + gchat aggregation engines, consumed
// by registerMessagingWorkers.
type aggregationStack struct {
	MessagesEngine *aggregation.Engine
	GChatEngine    *aggregation.Engine
}

// buildAggregationEngines constructs the messages + gchat aggregation engines
// and their reenqueuers, registers the gchat rematch handlers (gated on a
// constructed gchat provider), and populates the deferred aggregator-reenqueuer
// registry. Wired unconditionally — the engines are stateless and inert until
// their source rows exist. telegramManager / gchatProvider / gchatSyncStates
// are nil-safe params exactly as the old inline fallbacks.
func buildAggregationEngines(
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	ingest ingestRepos,
	messaging messagingFoundation,
	consumers eventConsumers,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
	telegramManager *tgpkg.TelegramManager,
	gchatProvider *google.GChatSyncProvider,
	gchatSyncStates google.GChatSyncStateLister,
) aggregationStack {
	interactionRepo := core.Interaction
	rematchService := graph.RematchService
	messagesMessageRepo := ingest.MessagesMessage
	commsMessageRepo := messaging.CommsMessageRepo
	aggregatorReenqueuerHolder := consumers.AggregatorReenqueuerHolder

	// Messages aggregator engine + reenqueuer + worker + sweeper.
	// Wired unconditionally — the Mac daemon push pipeline accepts
	// raw_message.* envelopes regardless of any feature flag, and the
	// engine is a stateless function over messagesMessageRepo (no
	// daemon-side connection or background loop).
	//
	// The chat-aware AggregateForContact path is what preserves the
	// engine's extend/bridge/coalesce contract. The
	// MessagingAggregateForContactWorker iterates over the contact's
	// distinct unprocessed chats and invokes it per chat; the periodic
	// sweeper provides a 5-min safety net for the never-claimed
	// stranded-row gap.
	messagesEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	const messagesBurstWindowHours = 4
	const messagesReplyBridgeHours = 48
	messagesEngine := messages.NewAggregationEngine(
		messagesBurstWindowHours,
		messagesReplyBridgeHours,
		messagesMessageRepo,
		interactionRepo,
		contactService,
		contactService,
		eventBus,
		database.Pool,
		messagesEnqueuer,
	)
	messagesReenqueuer := consumer.NewMessagesAggregatorReenqueuer(
		messagesEngine,
		riverClient,
		repository.InteractionSourceMessages,
	)

	// GChat aggregation engine over comms_message. LIVE but INERT: the
	// engine/worker/sweeper/reenqueuer for gchat run on every tick, but every
	// query is source='gchat'-scoped and returns zero rows until a provider +
	// enablement write comms_message(source='gchat') rows. Burst/reply windows
	// are hard-coded here (matching how messages hard-codes its constants);
	// env-var overrides are out of scope for now.
	gchatEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	const gchatBurstWindowHours = 2
	const gchatReplyBridgeHours = 48
	gchatEngine := google.NewGChatAggregationEngine(
		gchatBurstWindowHours,
		gchatReplyBridgeHours,
		commsMessageRepo,
		interactionRepo,
		contactService,
		contactService,
		eventBus,
		database.Pool,
		gchatEnqueuer,
	)

	// GChat rematch handlers: registered when the provider was constructed
	// (Google OAuth configured) AND external sync is enabled. They are provably
	// inert until an enabled gchat sync state exists — each gates FIRST on
	// ListEnabledSyncStates filtered to source='gchat' and returns (0, nil) when
	// that set is empty. The email handler co-registers under "email" alongside
	// Gmail/Calendar; its gchat-scoped gate means it no-ops while the others do
	// their real work. The provider itself is NOT registered into
	// providerRegistry (the scheduler never runs it).
	if gchatProvider != nil && gchatSyncStates != nil {
		rematchService.Register(google.NewGChatHandleRematchHandler(gchatProvider, gchatSyncStates, commsMessageRepo, gchatEngine))
		rematchService.Register(google.NewGChatEmailRematchHandler(gchatProvider, gchatSyncStates, commsMessageRepo, gchatEngine))
		logger.Info().Msg("GChat rematch handlers registered (inert until a gchat sync state is enabled)")
	}

	gchatReenqueuer := consumer.NewCommsAggregatorReenqueuer(
		gchatEngine,
		riverClient,
		repository.InteractionSourceGChat,
	)

	// Wire the per-source aggregator reenqueuer registry. The
	// InteractionRecorderWorker holds the deferred holder; this
	// assignment makes the post-commit reenqueue path live for both
	// telegram-source and messages-source events. When Telegram is
	// disabled the telegram entry is a no-op reenqueuer (so calls for
	// telegram-source envelopes — which won't be produced anyway —
	// degrade cleanly).
	reenqueuerEntries := map[string]consumer.AggregatorReenqueuer{
		repository.InteractionSourceMessages: messagesReenqueuer,
		repository.InteractionSourceGChat:    gchatReenqueuer,
	}
	if telegramManager != nil {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NewTelegramAggregatorReenqueuer(telegramManager.AggregationEngine())
	} else {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NoopAggregatorReenqueuer{}
	}
	aggregatorReenqueuerHolder.set(consumer.NewAggregatorReenqueuerRegistry(reenqueuerEntries))

	return aggregationStack{
		MessagesEngine: messagesEngine,
		GChatEngine:    gchatEngine,
	}
}
