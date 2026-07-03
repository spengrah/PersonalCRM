package main

import (
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"
	"personal-crm/backend/internal/todoist"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// buildTodoistOAuth initializes the Todoist OAuth service when
// TODOIST_CLIENT_ID/SECRET are configured. It shares/creates the OAuth
// handler with Google (if oauthHandler is already non-nil it appends Todoist
// to it, otherwise it creates a Google-less handler), builds the Todoist
// settings handler, and populates the FollowUpManager's deferred settings
// ref. Returns (todoistOAuthService, oauthHandler, todoistHandler) — all nil
// (except the passed-through oauthHandler) when unconfigured or on failure.
func buildTodoistOAuth(
	cfg *config.Config,
	oauthRepo *repository.OAuthRepository,
	syncRepo *repository.SyncRepository,
	oauthHandler *handlers.OAuthHandler,
	followUpSettingsHolder *followUpSettingsRef,
) (*todoist.OAuthService, *handlers.OAuthHandler, *handlers.TodoistHandler) {
	if cfg.Todoist.ClientID != "" && cfg.Todoist.ClientSecret != "" {
		todoistOAuthService, err := todoist.NewOAuthService(cfg, oauthRepo, syncRepo)
		if err != nil {
			logger.Warn().Err(err).Msg("failed to initialize Todoist OAuth service")
			return nil, oauthHandler, nil
		}
		// If OAuth handler exists (from Google), add Todoist to it
		// Otherwise create a new handler with nil Google service
		if oauthHandler != nil {
			oauthHandler.SetTodoistOAuth(todoistOAuthService)
		} else {
			oauthHandler = handlers.NewOAuthHandler(nil, cfg.CORS.FrontendURL)
			oauthHandler.SetTodoistOAuth(todoistOAuthService)
		}

		// Initialize Todoist settings handler
		todoistHandler := handlers.NewTodoistHandler(todoistOAuthService, syncRepo)

		// Populate the FollowUpManager's deferred Todoist settings
		// ref so the cutover consumer can resolve settings via the
		// real OAuth service for its post-commit refresh / close /
		// retry workers.
		followUpSettingsHolder.oauth = todoistOAuthService
		followUpSettingsHolder.sync = syncRepo

		logger.Info().Msg("Todoist OAuth service initialized")
		return todoistOAuthService, oauthHandler, todoistHandler
	}
	logger.Info().Msg("Todoist OAuth not configured (TODOIST_CLIENT_ID and TODOIST_CLIENT_SECRET required)")
	return nil, oauthHandler, nil
}

// todoistProviderDeps carries the collaborators registerTodoistProvider needs.
type todoistProviderDeps struct {
	TodoistOAuthService  *todoist.OAuthService
	ProviderRegistry     *sync.ProviderRegistry
	ContactTaskRepo      *repository.ContactTaskRepository
	ContactRepo          *repository.ContactRepository
	SyncRepo             *repository.SyncRepository
	Config               *config.Config
	EventBus             *events.Bus
	CadenceUpdater       *consumer.CadenceUpdater
	Pool                 *pgxpool.Pool
	TodoistClientFactory todoist.ClientFactory
	RiverClient          *river.Client[pgx.Tx]
}

// registerTodoistProvider registers the Todoist Cadence sync provider and
// builds the contact-task service + handler. Called only when
// todoistOAuthService != nil. Returns the contact-task handler.
func registerTodoistProvider(deps todoistProviderDeps) *handlers.ContactTaskHandler {
	todoistProvider := todoist.NewCadenceSyncProvider(
		deps.TodoistOAuthService,
		deps.ContactTaskRepo,
		deps.ContactRepo,
		deps.SyncRepo,
		deps.Config,
		deps.EventBus,
		deps.CadenceUpdater,
		deps.Pool,
		deps.TodoistClientFactory,
		deps.RiverClient,
		deps.Config.EventBus.FollowUpMode == config.EventBusFollowUpModeCutover,
	)
	deps.ProviderRegistry.Register(todoistProvider)
	logger.Info().Msg("Todoist Cadence sync provider registered")

	// Follow-up lifecycle is handled by consumer.FollowUpManager
	// (wired above via SetFollowUpConsumer). The Todoist
	// dependency (settings + client factory) routes through
	// followUpSettingsHolder which was populated when Todoist
	// OAuth initialized. No follow-up service is constructed
	// here — FollowUpManager is the sole writer.

	// Initialize contact task service and handler for action tasks
	contactTaskService := service.NewContactTaskService(
		deps.ContactTaskRepo,
		deps.ContactRepo,
		deps.SyncRepo,
		deps.TodoistOAuthService,
		deps.Config,
	)
	contactTaskHandler := handlers.NewContactTaskHandler(contactTaskService)
	logger.Info().Msg("Contact task handler initialized")
	return contactTaskHandler
}
