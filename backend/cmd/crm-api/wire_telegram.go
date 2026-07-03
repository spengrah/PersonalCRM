package main

import (
	"context"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// telegramStack holds the Telegram manager + handler. A zero telegramStack
// (returned when Telegram sync is disabled) reproduces today's nil manager /
// nil handler exactly.
type telegramStack struct {
	Manager *tgpkg.TelegramManager
	Handler *handlers.TelegramHandler
}

// buildTelegram constructs and starts the Telegram manager and registers its
// rematch handlers. It calls telegramManager.Start(ctx) but does NOT register
// the Stop defer — the caller (run()) owns that as a nil-guarded defer so it
// fires on run() return, not on this function's return. Called only when
// EnableTelegramSync is on and a nonzero API id is configured.
func buildTelegram(
	ctx context.Context,
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	messaging messagingFoundation,
	domain domainServices,
	pubBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) telegramStack {
	interactionRepo := core.Interaction
	rematchService := graph.RematchService
	telegramMessageRepo := messaging.TelegramMessageRepo
	enrichmentService := domain.EnrichmentService

	telegramSessionRepo := repository.NewTelegramSessionRepository(database.Queries)
	telegramUpdateStateRepo := repository.NewTelegramUpdateStateRepository(database.Queries)
	telegramChatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
	// telegramMessageRepo is hoisted above (needed by the consumer
	// wiring); no re-construction here.
	telegramSyncRepo := repository.NewSyncRepository(database.Queries)

	// Phase 4: identity + aggregation dependencies
	tgIdentityRepo := repository.NewIdentityRepository(database.Queries)
	tgIdentityService := service.NewIdentityService(tgIdentityRepo)
	tgExternalContactRepo := repository.NewExternalContactRepository(database.Queries)

	encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize Telegram encryptor (TOKEN_ENCRYPTION_KEY required)")
	}

	// River-backed stale-claim recovery enqueuer. Uses UniqueOpts
	// {ByArgs: true} paired with the InteractionRecorderJobArgs.EventID
	// `river:"unique"` tag so repeated recovery enqueues against the
	// same event coalesce into one in-flight job (spec §3 Race
	// Mechanics).
	tgRecoveryEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(riverClient)

	telegramManager := tgpkg.NewTelegramManager(
		telegramSessionRepo,
		telegramUpdateStateRepo,
		telegramChatConfigRepo,
		telegramMessageRepo,
		telegramSyncRepo,
		encryptor,
		cfg.External.TelegramAPIID,
		cfg.External.TelegramAPIHash,
		&cfg.Telegram,
		tgIdentityService,
		tgExternalContactRepo,
		enrichmentService,
		interactionRepo,
		contactService,
		contactService,
		pubBus,
		database.Pool,
		tgRecoveryEnqueuer,
	)

	if err := telegramManager.Start(ctx); err != nil {
		logger.Warn().Err(err).Msg("failed to start Telegram connection")
	}
	// NOTE: telegramManager.Stop() is deferred by the caller (run()) as a
	// nil-guarded defer — a defer here would fire when THIS function
	// returns, stopping the client at boot.

	telegramHandler := handlers.NewTelegramHandler(telegramManager)

	// Register telegram rematch handlers (telegram + phone identifiers)
	// against the same matcher/aggregator instances the manager owns so
	// rematch behavior is identical to the post-import path.
	rematchService.Register(tgpkg.NewUsernameRematchHandler(telegramMessageRepo, telegramManager.PeerMatcher(), telegramManager.AggregationEngine()))
	rematchService.Register(tgpkg.NewPhoneRematchHandler(telegramMessageRepo, telegramManager.PeerMatcher(), telegramManager.AggregationEngine()))
	logger.Info().Msg("Telegram rematch handlers registered")

	logger.Info().Msg("Telegram integration initialized")

	return telegramStack{
		Manager: telegramManager,
		Handler: telegramHandler,
	}
}
