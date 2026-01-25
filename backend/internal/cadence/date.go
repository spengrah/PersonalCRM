package cadence

import (
	"time"
)

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
func CalculateContactBy(base time.Time, cadenceType CadenceType) time.Time {
	days := CadenceDays(cadenceType)
	baseDate := DateOnly(base)
	return baseDate.AddDate(0, 0, days)
}

// IsContactByOverdue checks if a contact_by date is overdue relative to the given time.
// Returns true if contact_by < today (in server timezone).
func IsContactByOverdue(contactBy time.Time, now time.Time) bool {
	contactByDate := DateOnly(contactBy)
	todayDate := Today(now)
	return contactByDate.Before(todayDate)
}

// GetContactByOverdueDays returns how many days overdue a contact is based on contact_by.
// Returns 0 if not overdue.
func GetContactByOverdueDays(contactBy time.Time, now time.Time) int {
	contactByDate := DateOnly(contactBy)
	todayDate := Today(now)

	if !contactByDate.Before(todayDate) {
		return 0
	}

	// Calculate days difference
	duration := todayDate.Sub(contactByDate)
	return int(duration.Hours() / 24)
}
