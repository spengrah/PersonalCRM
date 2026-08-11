package handlers

import (
	"net/http"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
)

type SystemHandler struct {
	contactRepo *repository.ContactRepository
	runtimeCfg  config.RuntimeConfig
}

func NewSystemHandler(contactRepo *repository.ContactRepository, runtimeCfg config.RuntimeConfig) *SystemHandler {
	return &SystemHandler{
		contactRepo: contactRepo,
		runtimeCfg:  runtimeCfg,
	}
}

type TimeResponse struct {
	CurrentTime        time.Time `json:"current_time"`
	IsAccelerated      bool      `json:"is_accelerated"`
	AccelerationFactor int       `json:"acceleration_factor"`
	Environment        string    `json:"environment"`
	BaseTime           string    `json:"base_time"`
}

type AccelerationSettings struct {
	Factor int `json:"factor" binding:"required"`
}

// GetSystemTime returns the current accelerated time and settings
func (h *SystemHandler) GetSystemTime(c *gin.Context) {
	// One atomic load: current_time and the factor/base/active that produced
	// it can never straddle a concurrent reconfiguration. Do not replace this
	// with accelerated.GetCurrentTime() + accelerated.Snapshot() — that is two
	// loads, and a reconfiguration landing between them would report a
	// current_time computed under the old settings alongside the new
	// factor/base/active, an inconsistent response this call exists to
	// prevent.
	currentTime, accelerationFactor, base, isAccelerated := accelerated.SnapshotWithTime()

	baseTime := ""
	if isAccelerated {
		baseTime = base.Format(time.RFC3339)
	}

	response := TimeResponse{
		CurrentTime:        currentTime,
		IsAccelerated:      isAccelerated,
		AccelerationFactor: accelerationFactor,
		Environment:        h.runtimeCfg.CRMEnvironment,
		BaseTime:           baseTime,
	}

	api.SendSuccess(c, http.StatusOK, response, nil)
}

// SetTimeAcceleration sets the time acceleration factor
func (h *SystemHandler) SetTimeAcceleration(c *gin.Context) {
	var settings AccelerationSettings
	if err := c.ShouldBindJSON(&settings); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": err.Error(),
			},
		})
		return
	}

	// The package owns the wall clock (rule 1); the handler captures none of
	// its own. Setting process state cannot fail, so there is nothing left to
	// 500 on.
	appliedAt := accelerated.ConfigureNow(settings.Factor)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"acceleration_factor": settings.Factor,
			"applied_at":          appliedAt,
		},
	})
}

// ExportData exports all CRM data as JSON
func (h *SystemHandler) ExportData(c *gin.Context) {
	ctx := c.Request.Context()

	// Get all contacts
	contacts, err := h.contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit: 1000, // Large limit to get all
	})
	if err != nil {
		api.LogServerError(c, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch contacts",
			},
		})
		return
	}

	exportData := gin.H{
		"exported_at": accelerated.GetCurrentTime(),
		"version":     "1.0",
		"data": gin.H{
			"contacts": contacts,
		},
	}

	c.Header("Content-Disposition", "attachment; filename=crm_export.json")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    exportData,
	})
}

// ImportData imports CRM data from JSON
func (h *SystemHandler) ImportData(c *gin.Context) {
	// This is a placeholder - full implementation would parse uploaded file
	// and import contacts
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"message": "Import functionality not yet implemented",
		},
	})
}
