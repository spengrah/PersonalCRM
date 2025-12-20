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
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/health"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "personal-crm/backend/docs" // Import generated docs
)

func main() {
	// Run migrations before connecting to database
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	migrationsPath := os.Getenv("MIGRATIONS_PATH")
	if migrationsPath == "" {
		migrationsPath = "migrations"
	}

	log.Println("Running database migrations...")
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		log.Fatal("Failed to run migrations:", err)
	}

	// Initialize database
	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer database.Close()

	log.Println("Database connected successfully")

	// Initialize repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)
	timeEntryRepo := repository.NewTimeEntryRepository(database.Queries)

	// Initialize services
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	// Initialize handlers
	contactHandler := handlers.NewContactHandler(contactRepo)
	reminderHandler := handlers.NewReminderHandler(reminderService)
	systemHandler := handlers.NewSystemHandler(contactRepo, reminderRepo)
	timeEntryHandler := handlers.NewTimeEntryHandler(timeEntryRepo)

	// Initialize and start scheduler
	cronScheduler := scheduler.NewScheduler(reminderService)
	if err := cronScheduler.Start(); err != nil {
		log.Fatal("Failed to start scheduler:", err)
	}
	defer cronScheduler.Stop()

	// Set up Gin router
	if os.Getenv("NODE_ENV") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Add middleware
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware())
	router.Use(api.ErrorHandlerMiddleware())

	// Health check endpoint
	router.GET("/health", health.HealthHandler)

	// API routes
	v1 := router.Group("/api/v1")
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

		// Time entry routes
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

		// Export/Import routes
		v1.POST("/export", systemHandler.ExportData)
		v1.POST("/import", systemHandler.ImportData)
	}

	// Swagger documentation
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Start server (bind to 127.0.0.1; support dynamic port when PORT=0)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := "127.0.0.1:" + port
	// Use a listener so we can discover the selected port when PORT=0
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("Failed to bind listener:", err)
	}

	// Discover the actual port (useful when PORT=0)
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		log.Fatal("Failed to determine TCP address")
	}
	selectedPort := tcpAddr.Port

	srv := &http.Server{
		Addr:    ln.Addr().String(),
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		log.Printf("Starting server on 127.0.0.1:%d", selectedPort)
		log.Printf("API documentation available at http://127.0.0.1:%d/swagger/index.html", selectedPort)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal("Failed to start server:", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	// Give outstanding requests a 30 second timeout to complete
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exited")

	// Print the selected port on graceful exit for supervising processes
	fmt.Printf("PORT=%d\n", selectedPort)
}
