package health

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type HealthResponse struct {
	Status string `json:"status"`
}

func HealthHandler(c *gin.Context) {
	response := HealthResponse{Status: "ok"}
	c.JSON(http.StatusOK, response)
}
