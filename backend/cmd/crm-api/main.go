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
	"log"
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
	// Initialize database
	ctx := context.Background()
	database, err := db.NewDatabase(ctx)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer database.Close()

	// Run migrations if needed
	// Note: In production, migrations should be run separately
	log.Println("Database connected successfully")

	// Initialize repositories
	contactRepo := repository.NewContactRepository(database.Queries)
	reminderRepo := repository.NewReminderRepository(database.Queries)

	// Initialize services
	reminderService := service.NewReminderService(reminderRepo, contactRepo)

	// Initialize handlers
	contactHandler := handlers.NewContactHandler(contactRepo)
	reminderHandler := handlers.NewReminderHandler(reminderService)

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
	}

	// Swagger documentation
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		log.Printf("Starting server on port %s", port)
		log.Printf("API documentation available at http://localhost:%s/swagger/index.html", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
}
