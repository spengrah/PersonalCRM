package cadence

import (
	"os"
	"time"

	"personal-crm/backend/internal/accelerated"
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

// ProductionCadenceConfig is the real-world cadence table — the durations
// production and staging run on. It is exported so a caller can state a
// production-duration expectation WITHOUT reading (or mutating) CRM_ENV: under
// CRM_ENV=test annual is two hours and every contact is trivially overdue, so a
// test that reads the ambient environment proves nothing about these semantics,
// and setting the variable would change cadence behaviour for every
// concurrently-running test in the process.
func ProductionCadenceConfig() CadenceConfig {
	return CadenceConfig{
		Weekly:    7 * 24 * time.Hour,   // 1 week
		Biweekly:  14 * 24 * time.Hour,  // 2 weeks
		Monthly:   30 * 24 * time.Hour,  // ~1 month
		Quarterly: 90 * 24 * time.Hour,  // ~3 months
		Biannual:  180 * 24 * time.Hour, // ~6 months
		Annual:    365 * 24 * time.Hour, // 1 year
	}
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
	case "accelerated":
		// Compressed-time preview environments: validate months in hours.
		// CRM_ENV=staging deliberately does NOT take this branch: the
		// persistent staging environment hosts a production-shaped QA world whose
		// rendered dates must agree with production duration semantics, so
		// staging falls through to the production cadences below.
		return CadenceConfig{
			Weekly:    10 * time.Minute, // 10 minutes = 1 week (test week in 10min)
			Biweekly:  20 * time.Minute, // 20 minutes = 2 weeks
			Monthly:   1 * time.Hour,    // 1 hour = 1 month (test month in 1hr)
			Quarterly: 3 * time.Hour,    // 3 hours = 1 quarter (test quarter in 3hrs)
			Biannual:  6 * time.Hour,    // 6 hours = 6 months
			Annual:    12 * time.Hour,   // 12 hours = 1 year
		}
	case "staging", "production", "prod", "":
		// Production semantics: real-world cadences. Staging shares them —
		// its QA world is production-shaped by construction, not by compressed time.
		return ProductionCadenceConfig()
	default:
		// Default to production for safety
		return ProductionCadenceConfig()
	}
}

// GetCadenceDuration returns the duration for a given cadence type
func GetCadenceDuration(cadenceType CadenceType) time.Duration {
	return durationFor(GetCadenceConfig(), cadenceType)
}

// GetProductionCadenceDuration is GetCadenceDuration against the PRODUCTION
// table rather than the ambient environment's. It exists so a caller that must
// reason in real-world durations — a seed asserting an overdue expectation under
// CRM_ENV=test, where annual is two hours — resolves the duration through the
// SAME lookup, including its unrecognized-cadence fallback, instead of
// hand-copying it.
func GetProductionCadenceDuration(cadenceType CadenceType) time.Duration {
	return durationFor(ProductionCadenceConfig(), cadenceType)
}

// durationFor is the single cadence-type → duration lookup. Both accessors above
// route through it, so an unrecognized cadence cannot resolve one way in one
// caller and another way in the next.
func durationFor(config CadenceConfig, cadenceType CadenceType) time.Duration {
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
	return isOverdue(GetCadenceDuration(cadenceType), lastContacted, createdAt, checkTime)
}

// IsOverdueWithProductionConfig is IsOverdueWithConfig evaluated against the
// PRODUCTION cadence table, whatever the ambient environment is. It shares the
// formula rather than restating it, so the two answers cannot diverge — which
// they would, silently, if a caller needing production semantics kept its own
// copy: under CRM_ENV=test annual is two hours and every contact is overdue, so
// such a caller cannot simply use the env-derived helper either.
//
// checkTime is the caller's reference instant, so a prediction and a measurement
// can be made to answer the same question about the same moment.
func IsOverdueWithProductionConfig(cadenceType CadenceType, lastContacted *time.Time, createdAt time.Time, checkTime time.Time) bool {
	return isOverdue(GetProductionCadenceDuration(cadenceType), lastContacted, createdAt, checkTime)
}

// isOverdue is the overdue formula itself: a contact is overdue once checkTime
// has passed its base instant plus the cadence period, where the base is
// last_contacted when set and created_at otherwise.
func isOverdue(duration time.Duration, lastContacted *time.Time, createdAt time.Time, checkTime time.Time) bool {
	lastContactTime := createdAt
	if lastContacted != nil {
		lastContactTime = *lastContacted
	}
	return checkTime.After(lastContactTime.Add(duration))
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

	// When acceleration is OFF but in a compressed-cadence env, use scaled days
	// This allows testing "X days overdue" scenarios in minutes without acceleration.
	// CRM_ENV=staging is NOT scaled: it runs production cadences (see
	// GetCadenceConfig), so its overdue days are real days.
	env := os.Getenv("CRM_ENV")
	switch env {
	case "test", "testing":
		// In test mode, 1 "day" = 2 minutes (weekly cadence / 7)
		scaledDay := 2 * time.Minute / 7
		return int(overdueTime / scaledDay)
	case "accelerated":
		// In accelerated mode, 1 "day" = 10 minutes / 7
		scaledDay := 10 * time.Minute / 7
		return int(overdueTime / scaledDay)
	default:
		// Production and staging: normal days
		return int(overdueTime / (24 * time.Hour))
	}
}

// isAccelerationActive checks if time acceleration is currently enabled
func isAccelerationActive() bool {
	_, _, active := accelerated.Snapshot()
	return active
}
