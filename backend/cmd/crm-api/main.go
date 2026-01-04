// @title Personal CRM API
// @version 1.0
// @description A personal customer relationship management API
// @termsOfService http://swagger.io/terms/

// @contact.name API Support
// @contact.url http://www.swagger.io/support
// @contact.email support@swagger.io

// @license.name MIT
// @license.url https://opensource.org/licenses/MIT

// @host localhost:8080
// @BasePath /api/v1
// @schemes http https

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Type "Bearer" followed by a space and JWT token.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "personal-crm/backend/docs" // Import generated docs
)

func main() {
	// Load and validate configuration first (before logger)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize structured logger with configuration
	logger.Init(cfg.Logger)

	logger.Info().
		Str("environment", cfg.Logger.Environment).
		Str("log_level", cfg.Logger.Level).
		Msg("configuration loaded successfully")

	// Run migrations before connecting to database
	logger.Info().Msg("running database migrations")
	if err := db.RunMigrations(cfg.Database.URL, cfg.Database.MigrationsPath); err != nil {
		logger.Fatal().Err(err).Msg("failed to run migrations")
	}

	// Initialize database
	ctx := context.Background()
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer database.Close()

	logger.Info().Msg("database connected successfully")

	// Initialize repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	timeEntryRepo := repository.NewTimeEntryRepository(database.Queries)

	// Initialize services
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, reminderRepo)
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	// Initialize external sync components (feature-flagged)
	var syncService *service.SyncService
	var syncHandler *handlers.SyncHandler
	var identityHandler *handlers.IdentityHandler
	var oauthHandler *handlers.OAuthHandler
	var importHandler *handlers.ImportHandler
	var googleOAuthService *google.OAuthService

	if cfg.Features.EnableExternalSync {
		syncRepo := repository.NewSyncRepository(database.Queries)
		identityRepo := repository.NewIdentityRepository(database.Queries)
		oauthRepo := repository.NewOAuthRepository(database.Queries)
		providerRegistry := sync.NewProviderRegistry()

		// Initialize Google OAuth service if configured
		if cfg.Google.ClientID != "" && cfg.Google.ClientSecret != "" {
			var err error
			googleOAuthService, err = google.NewOAuthService(cfg, oauthRepo)
			if err != nil {
				logger.Warn().Err(err).Msg("failed to initialize Google OAuth service")
			} else {
				oauthHandler = handlers.NewOAuthHandler(googleOAuthService, cfg.CORS.FrontendURL)
				logger.Info().Msg("Google OAuth service initialized")
			}
		} else {
			logger.Info().Msg("Google OAuth not configured (GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET required)")
		}

		// Initialize external contact and enrichment repositories
		externalContactRepo := repository.NewExternalContactRepository(database.Queries)
		enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

		// Initialize identity and enrichment services
		identityService := service.NewIdentityService(identityRepo)
		enrichmentService := service.NewEnrichmentService(contactRepo, contactMethodRepo, enrichmentRepo)

		// Register Google Contacts provider if OAuth is configured
		if googleOAuthService != nil {
			gcontactsProvider := google.NewContactsProvider(
				googleOAuthService,
				externalContactRepo,
				enrichmentService,
				identityService,
			)
			providerRegistry.Register(gcontactsProvider)
			logger.Info().Msg("Google Contacts sync provider registered")
		}

		syncService = service.NewSyncService(syncRepo, contactRepo, providerRegistry)

		syncHandler = handlers.NewSyncHandler(syncService)
		identityHandler = handlers.NewIdentityHandler(identityService)

		// Initialize import handler
		importHandler = handlers.NewImportHandler(externalContactRepo, contactService, enrichmentService)

		logger.Info().Msg("external sync infrastructure enabled")
	}

	// Initialize handlers
	contactHandler := handlers.NewContactHandler(contactService)
	reminderHandler := handlers.NewReminderHandler(reminderService)
	systemHandler := handlers.NewSystemHandler(contactRepo, reminderRepo, cfg.Runtime)
	timeEntryHandler := handlers.NewTimeEntryHandler(timeEntryRepo)

	// Initialize and start scheduler
	cronScheduler := scheduler.NewScheduler(reminderService, syncService, cfg.Features.EnableExternalSync)
	if err := cronScheduler.Start(); err != nil {
		logger.Fatal().Err(err).Msg("failed to start scheduler")
	}
	defer cronScheduler.Stop()

	// Set up Gin router
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Add middleware
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	router.Use(api.ErrorHandlerMiddleware())

	// Health check endpoint
	healthChecker := health.NewHealthChecker(database, cfg.Database.HealthTimeout)
	router.GET("/health", healthChecker.Handler)

	// OAuth callback route (no auth - called by Google redirect)
	if oauthHandler != nil {
		router.GET("/api/v1/auth/google/callback", oauthHandler.GoogleCallback)
	}

	// API routes
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		// Contact routes
		contacts := v1.Group("/contacts")
		{
			contacts.POST("", contactHandler.CreateContact)
			contacts.GET("/overdue", contactHandler.ListOverdueContacts)
			contacts.GET("", contactHandler.ListContacts)
			contacts.GET("/:id", contactHandler.GetContact)
			contacts.PUT("/:id", contactHandler.UpdateContact)
			contacts.DELETE("/:id", contactHandler.DeleteContact)
			contacts.PATCH("/:id/last-contacted", contactHandler.UpdateContactLastContacted)
			contacts.GET("/:id/reminders", reminderHandler.GetRemindersByContact)
		}

		// Reminder routes
		reminders := v1.Group("/reminders")
		{
			reminders.POST("", reminderHandler.CreateReminder)
			reminders.GET("", reminderHandler.GetReminders)
			reminders.GET("/stats", reminderHandler.GetReminderStats)
			reminders.PATCH("/:id/complete", reminderHandler.CompleteReminder)
			reminders.DELETE("/:id", reminderHandler.DeleteReminder)
		}

		// System routes
		system := v1.Group("/system")
		{
			system.GET("/time", systemHandler.GetSystemTime)
			system.POST("/time/acceleration", systemHandler.SetTimeAcceleration)
		}

		// OAuth routes (feature-flagged with external sync)
		if oauthHandler != nil {
			authRoutes := v1.Group("/auth")
			{
				// Google OAuth
				authRoutes.GET("/google", oauthHandler.GetGoogleAuthURL)
				authRoutes.GET("/google/accounts", oauthHandler.ListGoogleAccounts)
				authRoutes.GET("/google/accounts/:id/status", oauthHandler.GetGoogleAccountStatus)
				authRoutes.POST("/google/accounts/:id/revoke", oauthHandler.RevokeGoogleAccount)
			}
		}

		// Time entry routes (feature-flagged)
		if cfg.Features.EnableTimeTracking {
			timeEntries := v1.Group("/time-entries")
			{
				timeEntries.POST("", timeEntryHandler.CreateTimeEntry)
				timeEntries.GET("", timeEntryHandler.ListTimeEntries)
				timeEntries.GET("/running", timeEntryHandler.GetRunningTimeEntry)
				timeEntries.GET("/stats", timeEntryHandler.GetTimeEntryStats)
				timeEntries.GET("/:id", timeEntryHandler.GetTimeEntry)
				timeEntries.PUT("/:id", timeEntryHandler.UpdateTimeEntry)
				timeEntries.DELETE("/:id", timeEntryHandler.DeleteTimeEntry)
			}
		}

		// External sync routes (feature-flagged)
		if cfg.Features.EnableExternalSync && syncHandler != nil {
			syncRoutes := v1.Group("/sync")
			{
				syncRoutes.GET("/status", syncHandler.GetSyncStatus)
				syncRoutes.GET("/providers", syncHandler.GetAvailableProviders)
				syncRoutes.GET("/logs", syncHandler.GetRecentSyncLogs)
				// Source-based routes (by source name like "gmail", "calendar")
				syncRoutes.GET("/sources/:source/status", syncHandler.GetSyncState)
				syncRoutes.POST("/sources/:source/trigger", syncHandler.TriggerSync)
				// State-based routes (by sync state UUID)
				syncRoutes.PATCH("/states/:id/enable", syncHandler.EnableSync)
				syncRoutes.GET("/states/:id/logs", syncHandler.GetSyncLogs)
			}

			// Identity matching routes
			identities := v1.Group("/identities")
			{
				identities.GET("/unmatched", identityHandler.ListUnmatchedIdentities)
				identities.GET("/:id", identityHandler.GetIdentity)
				identities.POST("/:id/link", identityHandler.LinkIdentity)
				identities.POST("/:id/unlink", identityHandler.UnlinkIdentity)
				identities.DELETE("/:id", identityHandler.DeleteIdentity)
			}

			// Add identity route to contacts
			contacts.GET("/:id/identities", identityHandler.ListIdentitiesForContact)

			// Import candidates routes
			if importHandler != nil {
				imports := v1.Group("/imports")
				{
					imports.GET("/candidates", importHandler.ListImportCandidates)
					imports.GET("/:id", importHandler.GetImportCandidate)
					imports.POST("/:id/import", importHandler.ImportContact)
					imports.POST("/:id/link", importHandler.LinkContact)
					imports.POST("/:id/ignore", importHandler.IgnoreContact)
				}
			}
		}

		// Export/Import routes
		v1.POST("/export", systemHandler.ExportData)
		v1.POST("/import", systemHandler.ImportData)
	}

	// Swagger documentation
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Start server with configured bind address
	addr := cfg.GetBindAddress()
	// Use a listener so we can discover the selected port when PORT=0
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Fatal().Err(err).Str("addr", addr).Msg("failed to bind listener")
	}

	// Discover the actual port (useful when PORT=0)
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		logger.Fatal().Msg("failed to determine TCP address")
	}
	selectedPort := tcpAddr.Port

	srv := &http.Server{
		Addr:    ln.Addr().String(),
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		logger.Info().
			Int("port", selectedPort).
			Str("addr", cfg.Server.Host).
			Msg("starting server")
		logger.Info().
			Str("url", fmt.Sprintf("http://%s:%d/swagger/index.html", cfg.Server.Host, selectedPort)).
			Msg("API documentation available")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("failed to start server")
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info().Msg("shutting down server")

	// Give outstanding requests a configured timeout to complete
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal().Err(err).Msg("server forced to shutdown")
	}

	logger.Info().Msg("server exited")

	// Print the selected port on graceful exit for supervising processes
	fmt.Printf("PORT=%d\n", selectedPort) //nolint:forbidigo // Intentional stdout output for supervisor
}
