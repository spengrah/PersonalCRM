package main

import (
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// buildGoogleOAuth initializes the Google OAuth service (and its OAuth
// handler) when GOOGLE_CLIENT_ID/SECRET are configured. Returns (nil, nil)
// when unconfigured or when construction fails (logged warn) — exactly the
// original nil semantics.
func buildGoogleOAuth(cfg *config.Config, oauthRepo *repository.OAuthRepository, syncRepo *repository.SyncRepository) (*google.OAuthService, *handlers.OAuthHandler) {
	if cfg.Google.ClientID != "" && cfg.Google.ClientSecret != "" {
		googleOAuthService, err := google.NewOAuthService(cfg, oauthRepo, syncRepo)
		if err != nil {
			logger.Warn().Err(err).Msg("failed to initialize Google OAuth service")
			return nil, nil
		}
		oauthHandler := handlers.NewOAuthHandler(googleOAuthService, cfg.CORS.FrontendURL)
		logger.Info().Msg("Google OAuth service initialized")
		return googleOAuthService, oauthHandler
	}
	logger.Info().Msg("Google OAuth not configured (GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET required)")
	return nil, nil
}

// googleProviderDeps carries the collaborators registerGoogleProviders needs.
// pubBus is a CONCRETE *events.Bus (nil in interaction-mode off) — never an
// interface — so the Gmail typed-nil trap (INV-3) cannot be introduced.
type googleProviderDeps struct {
	GoogleOAuthService          *google.OAuthService
	ProviderRegistry            *sync.ProviderRegistry
	RematchService              *service.RematchService
	CalendarRepo                *repository.CalendarEventRepository
	ContactRepo                 *repository.ContactRepository
	ExternalContactRepo         *repository.ExternalContactRepository
	EnrichmentService           *service.EnrichmentService
	IdentityService             *service.IdentityService
	AddressBookReconcileService *service.AddressBookReconcileService
	CommsMessageRepo            *repository.CommsMessageRepository
	SyncRepo                    *repository.SyncRepository
	PubBus                      *events.Bus
	Pool                        *pgxpool.Pool
}

// registerGoogleProviders registers the Google Contacts, Calendar, Gmail
// (cutover-only), and GChat sync providers into the registry, plus their
// rematch handlers. Called only when googleOAuthService != nil. Returns the
// GChat provider + enabled-state lister (hoisted for the late gchat rematch
// block outside the external-sync branch).
func registerGoogleProviders(deps googleProviderDeps) (*google.GChatSyncProvider, google.GChatSyncStateLister) {
	gcontactsProvider := google.NewContactsProvider(
		deps.GoogleOAuthService,
		deps.ExternalContactRepo,
		deps.EnrichmentService,
		deps.IdentityService,
		deps.AddressBookReconcileService,
	)
	deps.ProviderRegistry.Register(gcontactsProvider)
	logger.Info().Msg("Google Contacts sync provider registered")

	// Register Google Calendar provider
	gcalProvider := google.NewCalendarSyncProvider(
		deps.GoogleOAuthService,
		deps.CalendarRepo,
		deps.ContactRepo,
		deps.IdentityService,
		deps.ExternalContactRepo,
		deps.PubBus,
		deps.Pool,
	)
	deps.ProviderRegistry.Register(gcalProvider)
	logger.Info().Msg("Google Calendar sync provider registered")

	// Gmail provider + rematch handler: publisher-driven, so register
	// ONLY in cutover mode. In off-mode pubBus is a nil *events.Bus;
	// passing it into the provider's busTx interface field would create
	// a non-nil-interface-wrapping-typed-nil and bypass the provider's
	// own `bus == nil` guard, panicking on the first PublishTx. Off-mode
	// is an emergency rollback posture where no publisher should run.
	// commsMessageRepo is reused from the email-consumer wiring above.
	if deps.PubBus != nil {
		gmailProvider := google.NewGmailSyncProvider(
			deps.GoogleOAuthService,
			deps.CommsMessageRepo,
			deps.PubBus,
			deps.Pool,
		)
		deps.ProviderRegistry.Register(gmailProvider)
		// syncRepo is the enabled-email-states lister: the rematch scan
		// runs only over accounts whose email sync is enabled (the same
		// gate the scheduler uses), not every connected OAuth account.
		deps.RematchService.Register(google.NewGmailRematchHandler(
			gmailProvider,
			deps.SyncRepo,
			deps.CommsMessageRepo,
		))
		// Correspondence discovery: an in-sync hook that runs the link-only
		// candidate gate over every fetched message's From/To/Cc
		// participants (between fetch and storage), so multi-party threads
		// the storage gate drops still surface unknown addresses that
		// strong-match an existing contact. Wired into the provider via a
		// setter (nil-safe; the hook is a no-op when unset). No periodic
		// job — discovery piggybacks the existing sync fetch.
		gmailProvider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(
			deps.ContactRepo,
			deps.ExternalContactRepo,
		))
		logger.Info().Msg("Gmail sync provider + rematch handler + correspondence discovery registered")
	} else {
		logger.Warn().Msg("Gmail provider NOT registered: event-bus interaction mode=off (pubBus nil)")
	}

	// GChat provider: registered into providerRegistry so the scheduler
	// can run it — this is the go-live switch (PR 3). It stays inert
	// until enablement reconciliation creates an enabled gchat sync state
	// (no state → ListDueSyncStates filters enabled=TRUE → nothing to
	// dispatch). It is store-only + event-free, so unlike Gmail it does
	// NOT gate on pubBus. gchatSyncStates = syncRepo is the enabled-state
	// lister the gchat rematch handlers gate on (registered in the late
	// depth-0 block).
	gchatProvider := google.NewGChatSyncProvider(
		deps.GoogleOAuthService,
		deps.CommsMessageRepo,
		deps.SyncRepo,
	)
	var gchatSyncStates google.GChatSyncStateLister = deps.SyncRepo
	deps.ProviderRegistry.Register(gchatProvider)
	logger.Info().Msg("GChat sync provider registered (inert until a gchat sync state is enabled)")

	return gchatProvider, gchatSyncStates
}
