package handlers

import (
	"context"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
)

// StalenessReader is the narrow read surface the handler needs. In production
// this is *service.StalenessService.
type StalenessReader interface {
	ListActiveBreaches(ctx context.Context) ([]repository.StalenessBreach, error)
}

// StalenessHandler serves the active sync-staleness breaches.
type StalenessHandler struct {
	reader StalenessReader
}

// NewStalenessHandler constructs the handler over the given reader.
func NewStalenessHandler(reader StalenessReader) *StalenessHandler {
	return &StalenessHandler{reader: reader}
}

// GetActiveBreaches returns the currently-open staleness breaches.
// @Summary List active sync-staleness breaches
// @Description Get the sync sources currently breaching their freshness thresholds
// @Tags sync
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]repository.StalenessBreach}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /sync/staleness [get]
func (h *StalenessHandler) GetActiveBreaches(c *gin.Context) {
	breaches, err := h.reader.ListActiveBreaches(c.Request.Context())
	if err != nil {
		api.SendInternalError(c, err.Error())
		return
	}
	api.SendSuccess(c, http.StatusOK, breaches, nil)
}
