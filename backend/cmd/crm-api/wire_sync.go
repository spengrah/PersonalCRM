package main

import (
	"context"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/push"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// syncStack holds every handler + service the external-sync branch builds.
// A zero syncStack (returned when EnableExternalSync is false) reproduces
// today's all-nil handler set exactly.
type syncStack struct {
	SyncService             *service.SyncService
	SyncHandler             *handlers.SyncHandler
	IdentityHandler         *handlers.IdentityHandler
	OAuthHandler            *handlers.OAuthHandler
	ImportHandler           *handlers.ImportHandler
	SuggestionHandler       *handlers.SuggestionHandler
	AnarlogDiscoveryHandler *handlers.AnarlogDiscoveryHandler
	CalendarHandler         *handlers.CalendarHandler
	TodoistHandler          *handlers.TodoistHandler
	ContactTaskHandler      *handlers.ContactTaskHandler
	GoogleOAuthService      *google.OAuthService
	ExternalContactRepo     *repository.ExternalContactRepository
	GChatProvider           *google.GChatSyncProvider
	GChatSyncStates         google.GChatSyncStateLister
}

// buildExternalSync constructs the entire external-sync branch: OAuth
// services, the provider registry + Google/Todoist/push providers, the sync
// service + boot reconciliations, and the import/suggestion/anarlog handlers.
// pubBus is threaded as a CONCRETE *events.Bus (nil in interaction-mode off)
// per INV-3. Called by run() only when cfg.Features.EnableExternalSync.
func buildExternalSync(
	ctx context.Context,
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	ingest ingestRepos,
	messaging messagingFoundation,
	consumers eventConsumers,
	domain domainServices,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
	pubBus *events.Bus,
) syncStack {
	contactRepo := core.Contact
	contactMethodRepo := core.ContactMethod
	contactTaskRepo := core.ContactTask
	rematchService := graph.RematchService
	commsMessageRepo := messaging.CommsMessageRepo
	cadenceUpdater := consumers.CadenceUpdater
	todoistClientFactory := consumers.TodoistClientFactory
	followUpSettingsHolder := consumers.FollowUpSettingsHolder
	enrichmentService := domain.EnrichmentService
	importMatchService := domain.ImportMatchService
	addressBookReconcileService := domain.AddressBookReconcileService
	externalContactRepoForIngest := ingest.ExternalContact
	identityServiceForIngest := ingest.IdentityService

	var stack syncStack

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	oauthRepo := repository.NewOAuthRepository(database.Queries)
	providerRegistry := sync.NewProviderRegistry()

	// Initialize Google + Todoist OAuth services (each nil when unconfigured).
	googleOAuthService, oauthHandler := buildGoogleOAuth(cfg, oauthRepo, syncRepo)
	todoistOAuthService, oauthHandler, todoistHandler := buildTodoistOAuth(cfg, oauthRepo, syncRepo, oauthHandler, followUpSettingsHolder)
	stack.GoogleOAuthService = googleOAuthService
	stack.OAuthHandler = oauthHandler
	stack.TodoistHandler = todoistHandler

	// External contact repository — reuse the instance constructed
	// at outer scope for the IngestService so all code paths share
	// the same repo (no behavioral difference today; future stateful
	// caching is centralized).
	externalContactRepo := externalContactRepoForIngest
	stack.ExternalContactRepo = externalContactRepo

	// Initialize identity service (enrichmentService is constructed at outer scope
	// so the Telegram block can share it).
	identityService := service.NewIdentityService(identityRepo)

	// Calendar repo + handler + rematch handler are wired whenever external
	// sync is enabled, regardless of OAuth configuration. Rematch over
	// calendar_event is pure DB work and must run in test/local environments
	// that don't have Google OAuth set up.
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	stack.CalendarHandler = handlers.NewCalendarHandler(calendarRepo)
	rematchService.Register(google.NewCalendarRematchHandler(calendarRepo, externalContactRepo, pubBus))
	logger.Info().Msg("Calendar rematch handler registered")

	// Register Google Contacts / Calendar / Gmail / GChat providers if OAuth is configured
	if googleOAuthService != nil {
		stack.GChatProvider, stack.GChatSyncStates = registerGoogleProviders(googleProviderDeps{
			GoogleOAuthService:          googleOAuthService,
			ProviderRegistry:            providerRegistry,
			RematchService:              rematchService,
			CalendarRepo:                calendarRepo,
			ContactRepo:                 contactRepo,
			ExternalContactRepo:         externalContactRepo,
			EnrichmentService:           enrichmentService,
			IdentityService:             identityService,
			AddressBookReconcileService: addressBookReconcileService,
			CommsMessageRepo:            commsMessageRepo,
			SyncRepo:                    syncRepo,
			PubBus:                      pubBus,
			Pool:                        database.Pool,
		})
	}

	// Register Todoist Cadence provider if OAuth is configured
	if todoistOAuthService != nil {
		stack.ContactTaskHandler = registerTodoistProvider(todoistProviderDeps{
			TodoistOAuthService:  todoistOAuthService,
			ProviderRegistry:     providerRegistry,
			ContactTaskRepo:      contactTaskRepo,
			ContactRepo:          contactRepo,
			SyncRepo:             syncRepo,
			Config:               cfg,
			EventBus:             eventBus,
			CadenceUpdater:       cadenceUpdater,
			Pool:                 database.Pool,
			TodoistClientFactory: todoistClientFactory,
			RiverClient:          riverClient,
		})
	}

	// Register every Mac-daemon push-source provider. Each is
	// push-only — data lands via /api/v1/ingest/events, never via the
	// scheduler (ListDueAccounts skips push strategy) — so Sync() is a
	// no-op. The registration lives in one helper so the daemonFamily
	// agreement test can cross-check it against the descriptor table.
	push.RegisterPushProviders(providerRegistry)
	logger.Info().Msg("Push providers registered (messages, icloud_contacts, phone_calls)")

	syncService := service.NewSyncService(syncRepo, contactRepo, providerRegistry)
	stack.SyncService = syncService

	// Email enablement reconciliation (Gmail go-live). Only meaningful in
	// cutover mode with a connected Google account: the Gmail provider is
	// registered only when pubBus != nil, so there is no point reconciling
	// email states no registered provider can serve. Wire the account
	// lister + OAuth-connect hook, then run the idempotent boot
	// reconciliation BEFORE riverClient.Start so the RunOnStart tick already
	// sees the freshly-enabled email states.
	if googleOAuthService != nil && pubBus != nil {
		syncService.SetEmailAccountLister(googleOAuthService)
		if oauthHandler != nil {
			oauthHandler.SetEmailStateReconciler(func(ctx context.Context) error {
				return syncService.ReconcileEmailSyncStates(ctx)
			})
		}
		if err := syncService.ReconcileEmailSyncStates(ctx); err != nil {
			// Non-fatal: the scheduler simply has nothing to do for email
			// until states exist; the next connect or next boot retries.
			logger.Warn().Err(err).Msg("boot email sync reconciliation failed (non-fatal)")
		}
	}

	// GChat enablement reconciliation (Chat go-live). Guarded ONLY by
	// googleOAuthService != nil — NOT pubBus: GChat is store-only + event-free,
	// so its registration + reconciliation are independent of the event-bus
	// interaction mode (gating on pubBus would wrongly disable GChat in
	// off-mode). The provider is registered above whenever Google OAuth is
	// configured; here we wire the account lister + OAuth-connect hook, then
	// run the idempotent, chat-scope-gated boot reconciliation BEFORE
	// riverClient.Start so the RunOnStart tick already sees any freshly-enabled
	// gchat states. No state is created until a connected account carries the
	// chat scopes (re-consent), keeping the feature inert until the operator
	// completes the Chat App config + re-consent.
	if googleOAuthService != nil {
		syncService.SetGChatAccountLister(googleOAuthService)
		if oauthHandler != nil {
			oauthHandler.SetGChatStateReconciler(func(ctx context.Context) error {
				return syncService.ReconcileGChatSyncStates(ctx)
			})
		}
		if err := syncService.ReconcileGChatSyncStates(ctx); err != nil {
			// Non-fatal: the scheduler simply has nothing to do for gchat
			// until states exist; the next connect or next boot retries.
			logger.Warn().Err(err).Msg("boot gchat sync reconciliation failed (non-fatal)")
		}
	}

	stack.SyncHandler = handlers.NewSyncHandler(syncService)
	stack.IdentityHandler = handlers.NewIdentityHandler(identityService)

	// Suggestion service composes the method-suggestion group with the
	// confidence-ranked candidate list and runs resolve/dismiss. Shared
	// by the import handler (its candidate sort) and the suggestion
	// handler (the new People-tab surface).
	suggestionService := service.NewSuggestionService(
		externalContactRepo,
		contactRepo,
		contactMethodRepo,
		enrichmentService,
		importMatchService,
		database,
	)
	stack.SuggestionHandler = handlers.NewSuggestionHandler(suggestionService)

	// Initialize import handler
	stack.ImportHandler = handlers.NewImportHandler(externalContactRepo, identityServiceForIngest, contactService, importMatchService, enrichmentService, suggestionService)

	// Anarlog-title discovery surface (People-tab grouped weak
	// candidates + token-group resolve). Reuses the external_contact
	// repo and ContactService — both already constructed above.
	anarlogDiscoveryService := service.NewAnarlogDiscoveryService(externalContactRepo, contactService)
	stack.AnarlogDiscoveryHandler = handlers.NewAnarlogDiscoveryHandler(anarlogDiscoveryService)

	logger.Info().Msg("external sync infrastructure enabled")

	return stack
}

// buildExternalSyncIfEnabled applies the external-sync feature gate: the full
// sync stack when cfg.Features.EnableExternalSync is set, a zero syncStack
// otherwise. This is the single gate seam shared by run() and the OAuth
// route-wiring boundary test — a regression that drops or inverts the gate
// fails the test because the test exercises this exact function.
func buildExternalSyncIfEnabled(
	ctx context.Context,
	cfg *config.Config,
	database *db.Database,
	core coreRepos,
	contactService *service.ContactService,
	graph graphCore,
	ingest ingestRepos,
	messaging messagingFoundation,
	consumers eventConsumers,
	domain domainServices,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
	pubBus *events.Bus,
) syncStack {
	if !cfg.Features.EnableExternalSync {
		return syncStack{}
	}
	return buildExternalSync(ctx, cfg, database, core, contactService, graph, ingest, messaging, consumers, domain, eventBus, riverClient, pubBus)
}
