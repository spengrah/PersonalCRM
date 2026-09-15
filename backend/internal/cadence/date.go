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

// CalendarDate returns the date in t's own location represented at UTC midnight.
// Date-based expiry comparisons are inclusive; a DATE decodes as UTC midnight,
// while local midnight differs from it by the zone offset, so comparing instants
// would misclassify the expiry date.
func CalendarDate(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// AwaitingReplyUntil returns the local-midnight expiry date at the outreach's
// calendar date plus watchdogDays. The expiry day is inclusive, and the date
// must be compared by calendar date because a DATE decodes as UTC midnight while
// local midnight differs by the zone offset.
func AwaitingReplyUntil(occurredAt time.Time, watchdogDays int) time.Time {
	return Today(occurredAt).AddDate(0, 0, watchdogDays)
}

// IsAwaitingReply is true only when outreach is strictly later than the last
// response and now is on or before the stored expiry date. It compares calendar
// dates because a DATE decodes as UTC midnight while local midnight differs by
// the zone offset; the expiry date itself remains inside the window.
func IsAwaitingReply(lastOutreachAt, lastResponseAt, awaitingReplyUntil *time.Time, now time.Time) bool {
	if lastOutreachAt == nil || awaitingReplyUntil == nil {
		return false
	}
	if lastResponseAt != nil && !lastOutreachAt.After(*lastResponseAt) {
		return false
	}
	return !CalendarDate(Today(now)).After(CalendarDate(*awaitingReplyUntil))
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
// In production: uses date comparison where contact_by < today.
// In testing mode: uses timestamp comparison (now > contact_by) because accelerated
// cadences are sub-day, and DATE column precision is insufficient.
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
