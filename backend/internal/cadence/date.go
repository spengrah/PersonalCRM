package cadence

import (
	"os"
	"time"
)

// IsTestingMode checks if we're in a testing environment where cadences are accelerated.
func IsTestingMode() bool {
	env := os.Getenv("CRM_ENV")
	return env == "test" || env == "testing"
}

// DateOnly truncates a timestamp to a date in server timezone (time.Local).
// This helper should be used everywhere we write or compare contact_by.
func DateOnly(t time.Time) time.Time {
	// Convert to local timezone and truncate to date
	local := t.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

// Today returns the current date in server timezone (time.Local).
// This helper should be used everywhere we compare against contact_by.
func Today(now time.Time) time.Time {
	return DateOnly(now)
}

// CadenceDays returns the number of days for a given cadence type.
// These are fixed day counts used for contact_by calculation:
// weekly: 7, biweekly: 14, monthly: 30, quarterly: 90, biannual: 180, annual: 365
func CadenceDays(cadenceType CadenceType) int {
	switch cadenceType {
	case CadenceWeekly:
		return 7
	case CadenceBiweekly:
		return 14
	case CadenceMonthly:
		return 30
	case CadenceQuarterly:
		return 90
	case CadenceBiannual:
		return 180
	case CadenceAnnual:
		return 365
	default:
		return 0
	}
}

// CalculateContactBy computes the contact_by date from a base timestamp and cadence.
// The base should typically be last_contacted or created_at.
// Returns the date when the contact should be reached next.
// Uses environment-aware cadence durations (accelerated in testing mode).
// In testing mode, returns the full timestamp; in production, returns date-only.
func CalculateContactBy(base time.Time, cadenceType CadenceType) time.Time {
	duration := GetCadenceDuration(cadenceType)
	nextDue := base.Add(duration)
	// In testing mode, keep full timestamp precision for accelerated cadences
	if IsTestingMode() {
		return nextDue.In(time.Local)
	}
	return DateOnly(nextDue)
}

// IsContactByOverdue checks if a contact_by date is overdue relative to the given time.
// In production: returns true if contact_by < today (date comparison)
// In testing mode: returns true if now > contact_by (timestamp comparison for accelerated time)
func IsContactByOverdue(contactBy time.Time, now time.Time) bool {
	if IsTestingMode() {
		// In testing mode, use timestamp comparison for accelerated cadences
		return now.After(contactBy)
	}
	// In production, use date comparison
	contactByDate := DateOnly(contactBy)
	todayDate := Today(now)
	return contactByDate.Before(todayDate)
}

// GetContactByOverdueDays returns how many days overdue a contact is based on contact_by.
// Returns 0 if not overdue.
// In testing mode, uses scaled day calculation for accelerated cadences.
func GetContactByOverdueDays(contactBy time.Time, now time.Time) int {
	if !IsContactByOverdue(contactBy, now) {
		return 0
	}

	if IsTestingMode() {
		// In testing mode, calculate "days" based on how much time has passed
		// relative to the weekly cadence duration (which is 2 minutes in test mode)
		overdueTime := now.Sub(contactBy)
		weeklyDuration := GetCadenceDuration(CadenceWeekly)
		scaledDayDuration := weeklyDuration / 7
		return int(overdueTime / scaledDayDuration)
	}

	// In production, use actual day difference
	contactByDate := DateOnly(contactBy)
	todayDate := Today(now)
	duration := todayDate.Sub(contactByDate)
	return int(duration.Hours() / 24)
}
