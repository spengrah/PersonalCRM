package reminder

import (
	"os"
	"strconv"
	"time"
)

// CadenceConfig manages different cadence mappings for testing vs production
type CadenceConfig struct {
	Weekly    time.Duration
	Biweekly  time.Duration
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
			Biweekly:  4 * time.Minute,  // Test biweekly cadence every 4 minutes
			Monthly:   10 * time.Minute, // Test monthly cadence every 10 minutes
			Quarterly: 30 * time.Minute, // Test quarterly every 30 minutes
			Biannual:  1 * time.Hour,    // Test biannual every hour
			Annual:    2 * time.Hour,    // Test annual every 2 hours
		}
	case "staging", "accelerated":
		// Fast staging: validate months in hours
		return CadenceConfig{
			Weekly:    10 * time.Minute, // 10 minutes = 1 week (test week in 10min)
			Biweekly:  20 * time.Minute, // 20 minutes = 2 weeks
			Monthly:   1 * time.Hour,    // 1 hour = 1 month (test month in 1hr)
			Quarterly: 3 * time.Hour,    // 3 hours = 1 quarter (test quarter in 3hrs)
			Biannual:  6 * time.Hour,    // 6 hours = 6 months
			Annual:    12 * time.Hour,   // 12 hours = 1 year
		}
	case "production", "prod", "":
		// Production: real-world cadences
		return CadenceConfig{
			Weekly:    7 * 24 * time.Hour,   // 1 week
			Biweekly:  14 * 24 * time.Hour,  // 2 weeks
			Monthly:   30 * 24 * time.Hour,  // ~1 month
			Quarterly: 90 * 24 * time.Hour,  // ~3 months
			Biannual:  180 * 24 * time.Hour, // ~6 months
			Annual:    365 * 24 * time.Hour, // 1 year
		}
	default:
		// Default to production for safety
		return CadenceConfig{
			Weekly:    7 * 24 * time.Hour,   // 1 week
			Biweekly:  14 * 24 * time.Hour,  // 2 weeks
			Monthly:   30 * 24 * time.Hour,  // ~1 month
			Quarterly: 90 * 24 * time.Hour,  // ~3 months
			Biannual:  180 * 24 * time.Hour, // ~6 months
			Annual:    365 * 24 * time.Hour, // 1 year
		}
	}
}

// GetCadenceDuration returns the duration for a given cadence type
func GetCadenceDuration(cadenceType CadenceType) time.Duration {
	config := GetCadenceConfig()

	switch cadenceType {
	case CadenceWeekly:
		return config.Weekly
	case CadenceBiweekly:
		return config.Biweekly
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

// CalculateNextDueDateWithConfig calculates the next due date using environment-specific cadences
func CalculateNextDueDateWithConfig(cadenceType CadenceType, lastContacted *time.Time, createdAt time.Time) time.Time {
	duration := GetCadenceDuration(cadenceType)

	var baseDate time.Time
	if lastContacted != nil {
		baseDate = *lastContacted
	} else {
		baseDate = createdAt
	}

	return baseDate.Add(duration)
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

// GetOverdueDaysWithConfig returns how many "days" overdue
// When acceleration is ON: use real 24-hour days (overdueTime is already in accelerated time)
// When acceleration is OFF: use scaled days based on environment (for testing compressed cadences)
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

	// When acceleration is ON, overdueTime is already in accelerated (display) time,
	// so use real 24-hour days to show meaningful numbers like "5 days overdue"
	if isAccelerationActive() {
		return int(overdueTime / (24 * time.Hour))
	}

	// When acceleration is OFF but in testing/staging env, use scaled days
	// This allows testing "X days overdue" scenarios in minutes without acceleration
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

// isAccelerationActive checks if time acceleration is currently enabled
func isAccelerationActive() bool {
	accelerationStr := os.Getenv("TIME_ACCELERATION")
	if accelerationStr == "" {
		return false
	}
	acceleration, err := strconv.Atoi(accelerationStr)
	return err == nil && acceleration > 1
}
