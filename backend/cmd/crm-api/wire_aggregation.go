package main

import (
	"personal-crm/backend/internal/config"
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
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// aggregationStack holds the messages + gchat + whatsapp aggregation engines,
// consumed by registerMessagingWorkers. ReenqueuerEntries is the same map handed
// to NewAggregatorReenqueuerRegistry — returned so the composition-root parity
// test can invoke a per-source entry.
type aggregationStack struct {
	MessagesEngine    *aggregation.Engine
	GChatEngine       *aggregation.Engine
	WhatsAppEngine    *aggregation.Engine
	ReenqueuerEntries map[string]consumer.AggregatorReenqueuer
}

// buildAggregationEngines constructs the messages + gchat + whatsapp aggregation
// engines and their reenqueuers, registers the gchat rematch handlers (gated on a
// constructed gchat provider) and the whatsapp ones (gated on the WhatsApp
// feature flag), and populates the deferred aggregator-reenqueuer
// registry. Wired unconditionally — the engines are stateless and inert until
// their source rows exist. telegramManager / gchatProvider / gchatSyncStates
// are nil-safe params exactly as the old inline fallbacks.
func buildAggregationEngines(
	cfg *config.Config,
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
	// enablement write comms_message(source='gchat') rows. The burst/reply windows
	// are constants on the engine's own package, so a caller that sizes its input
	// to them reads the same values this wiring passes; env-var overrides are out
	// of scope for now.
	gchatEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	gchatEngine := google.NewGChatAggregationEngine(
		google.GChatBurstWindowHours,
		google.GChatReplyBridgeHours,
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

	// WhatsApp aggregation engine over comms_message. LIVE but INERT on the same
	// terms as gchat: every query is source='whatsapp'-scoped and returns zero
	// rows until ingest writes one. It is deliberately NOT gated on
	// EnableWhatsAppSync — rows staged under an earlier ON boot must still
	// aggregate after a restart with the flag off. Unlike gchat's, the
	// burst/reply windows are operator-tunable env knobs.
	whatsappEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)
	whatsappEngine := wapkg.NewAggregationEngine(
		cfg.WhatsApp.BurstWindowHours,
		cfg.WhatsApp.ReplyBridgeHours,
		commsMessageRepo,
		interactionRepo,
		contactService,
		contactService,
		eventBus,
		database.Pool,
		whatsappEnqueuer,
	)

	// WhatsApp rematch handlers: registered ONLY when WhatsApp sync is enabled.
	// Unlike the engine, these are not inert when off — registering the 'phone'
	// handler unconditionally would make a bare phone method rematch-eligible on
	// deployments with both Telegram and WhatsApp disabled, minting a rematch job
	// where none exists today (RematchService.EligibleMethods).
	if cfg.Features.EnableWhatsAppSync {
		rematchService.Register(wapkg.NewWhatsAppMethodRematchHandler(commsMessageRepo, whatsappEngine))
		rematchService.Register(wapkg.NewPhoneRematchHandler(commsMessageRepo, whatsappEngine))
		logger.Info().Msg("WhatsApp rematch handlers registered (whatsapp + phone contact methods)")
	}

	whatsappReenqueuer := consumer.NewCommsAggregatorReenqueuer(
		whatsappEngine,
		riverClient,
		repository.InteractionSourceWhatsApp,
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
		repository.InteractionSourceWhatsApp: whatsappReenqueuer,
	}
	if telegramManager != nil {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NewTelegramAggregatorReenqueuer(telegramManager.AggregationEngine())
	} else {
		reenqueuerEntries[repository.InteractionSourceTelegram] = consumer.NoopAggregatorReenqueuer{}
	}
	aggregatorReenqueuerHolder.set(consumer.NewAggregatorReenqueuerRegistry(reenqueuerEntries))

	return aggregationStack{
		MessagesEngine:    messagesEngine,
		GChatEngine:       gchatEngine,
		WhatsAppEngine:    whatsappEngine,
		ReenqueuerEntries: reenqueuerEntries,
	}
}
