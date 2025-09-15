package reminder

import (
	"os"
	"time"
)

// CadenceConfig manages different cadence mappings for testing vs production
type CadenceConfig struct {
	Weekly    time.Duration
	Monthly   time.Duration
	Quarterly time.Duration
	Biannual  time.Duration
	Annual    time.Duration
}

// GetCadenceConfig returns appropriate cadence configuration based on environment
func GetCadenceConfig() CadenceConfig {
	env := os.Getenv("CRM_ENV")

	switch env {
	case "test", "testing":
		// Ultra-fast testing: validate weeks in minutes
		return CadenceConfig{
			Weekly:    2 * time.Minute,  // Test weekly cadence every 2 minutes
			Monthly:   10 * time.Minute, // Test monthly cadence every 10 minutes
			Quarterly: 30 * time.Minute, // Test quarterly every 30 minutes
			Biannual:  1 * time.Hour,    // Test biannual every hour
			Annual:    2 * time.Hour,    // Test annual every 2 hours
		}
	case "staging", "accelerated":
		// Fast staging: validate months in hours
		return CadenceConfig{
			Weekly:    10 * time.Minute, // 10 minutes = 1 week (test week in 10min)
			Monthly:   1 * time.Hour,    // 1 hour = 1 month (test month in 1hr)
			Quarterly: 3 * time.Hour,    // 3 hours = 1 quarter (test quarter in 3hrs)
			Biannual:  6 * time.Hour,    // 6 hours = 6 months
			Annual:    12 * time.Hour,   // 12 hours = 1 year
		}
	case "production", "prod", "":
		// Production: real-world cadences
		return CadenceConfig{
			Weekly:    7 * 24 * time.Hour,   // 1 week
			Monthly:   30 * 24 * time.Hour,  // ~1 month
			Quarterly: 90 * 24 * time.Hour,  // ~3 months
			Biannual:  180 * 24 * time.Hour, // ~6 months
			Annual:    365 * 24 * time.Hour, // 1 year
		}
	default:
		// Default to production for safety
		return GetCadenceConfig() // Will hit production case
	}
}

// GetCadenceDuration returns the duration for a given cadence type
func GetCadenceDuration(cadenceType CadenceType) time.Duration {
	config := GetCadenceConfig()

	switch cadenceType {
	case CadenceWeekly:
		return config.Weekly
	case CadenceMonthly:
		return config.Monthly
	case CadenceQuarterly:
		return config.Quarterly
	case CadenceBiannual:
		return config.Biannual
	case CadenceAnnual:
		return config.Annual
	default:
		return config.Monthly // Default fallback
	}
}

// IsOverdueWithConfig checks if contact is overdue using environment-specific cadences
func IsOverdueWithConfig(cadenceType CadenceType, lastContacted *time.Time, createdAt time.Time, checkTime time.Time) bool {
	duration := GetCadenceDuration(cadenceType)

	var lastContactTime time.Time
	if lastContacted != nil {
		lastContactTime = *lastContacted
	} else {
		lastContactTime = createdAt
	}

	nextContactDue := lastContactTime.Add(duration)
	return checkTime.After(nextContactDue)
}

// GetSchedulerCronSpec returns the cron specification for the scheduler based on environment
func GetSchedulerCronSpec() string {
	env := os.Getenv("CRM_ENV")

	switch env {
	case "test", "testing":
		// Ultra-fast testing: every 30 seconds
		return "@every 30s"
	case "staging", "accelerated":
		// Fast staging: every 5 minutes
		return "@every 5m"
	case "production", "prod", "":
		// Production: daily at 8:00 AM
		return "0 0 8 * * *"
	default:
		// Default to production for safety
		return "0 0 8 * * *"
	}
}

// GetOverdueDaysWithConfig returns how many "days" overdue (scaled for environment)
func GetOverdueDaysWithConfig(cadenceType CadenceType, lastContacted *time.Time, createdAt time.Time, checkTime time.Time) int {
	duration := GetCadenceDuration(cadenceType)

	var lastContactTime time.Time
	if lastContacted != nil {
		lastContactTime = *lastContacted
	} else {
		lastContactTime = createdAt
	}

	nextContactDue := lastContactTime.Add(duration)
	if !checkTime.After(nextContactDue) {
		return 0
	}

	overdueTime := checkTime.Sub(nextContactDue)

	// Scale overdue calculation based on environment
	// In accelerated mode, "days" are proportionally shorter
	env := os.Getenv("CRM_ENV")
	switch env {
	case "test", "testing":
		// In test mode, 1 "day" = 2 minutes (weekly cadence / 7)
		scaledDay := 2 * time.Minute / 7
		return int(overdueTime / scaledDay)
	case "staging", "accelerated":
		// In staging mode, 1 "day" = 10 minutes / 7
		scaledDay := 10 * time.Minute / 7
		return int(overdueTime / scaledDay)
	default:
		// Production: normal days
		return int(overdueTime / (24 * time.Hour))
	}
}
