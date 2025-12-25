package handlers

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
)

type SystemHandler struct {
	contactRepo  *repository.ContactRepository
	reminderRepo *repository.ReminderRepository
	runtimeCfg   config.RuntimeConfig
}

func NewSystemHandler(contactRepo *repository.ContactRepository, reminderRepo *repository.ReminderRepository, runtimeCfg config.RuntimeConfig) *SystemHandler {
	return &SystemHandler{
		contactRepo:  contactRepo,
		reminderRepo: reminderRepo,
		runtimeCfg:   runtimeCfg,
	}
}

type TimeResponse struct {
	CurrentTime        time.Time `json:"current_time"`
	IsAccelerated      bool      `json:"is_accelerated"`
	AccelerationFactor int       `json:"acceleration_factor"`
	Environment        string    `json:"environment"`
}

type AccelerationSettings struct {
	Factor int `json:"factor" binding:"required"`
}

// GetSystemTime returns the current accelerated time and settings
func (h *SystemHandler) GetSystemTime(c *gin.Context) {
	currentTime := accelerated.GetCurrentTime()
	accelerationStr := os.Getenv("TIME_ACCELERATION")
	accelerationFactor := 1
	isAccelerated := false

	if accelerationStr != "" {
		if factor, err := strconv.Atoi(accelerationStr); err == nil {
			accelerationFactor = factor
			isAccelerated = factor > 1
		}
	}

	response := TimeResponse{
		CurrentTime:        currentTime,
		IsAccelerated:      isAccelerated,
		AccelerationFactor: accelerationFactor,
		Environment:        h.runtimeCfg.CRMEnvironment,
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

	// Set environment variables (note: this only affects current process)
	os.Setenv("TIME_ACCELERATION", strconv.Itoa(settings.Factor))
	if settings.Factor > 1 {
		os.Setenv("TIME_BASE", strconv.FormatInt(time.Now().Unix(), 10)) //nolint:forbidigo // Need wall-clock base for acceleration
	} else {
		os.Unsetenv("TIME_BASE")
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"acceleration_factor": settings.Factor,
			"applied_at":          accelerated.GetCurrentTime(),
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch contacts",
			},
		})
		return
	}

	// Get all reminders
	reminders, err := h.reminderRepo.ListReminders(ctx, repository.ListRemindersParams{
		Limit: 1000, // Large limit to get all
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch reminders",
			},
		})
		return
	}

	exportData := gin.H{
		"exported_at": accelerated.GetCurrentTime(),
		"version":     "1.0",
		"data": gin.H{
			"contacts":  contacts,
			"reminders": reminders,
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
	// and import contacts/reminders
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"message": "Import functionality not yet implemented",
		},
	})
}
