package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

type HealthResponse struct {
	Status string `json:"status"`
}

func healthHandler(c *gin.Context) {
	response := HealthResponse{Status: "ok"}
	c.JSON(http.StatusOK, response)
}

func main() {
	// Set Gin mode based on environment
	if os.Getenv("NODE_ENV") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.Default()

	// Health endpoint
	r.GET("/health", healthHandler)

	// Get port from environment or default to 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting server on port %s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}
