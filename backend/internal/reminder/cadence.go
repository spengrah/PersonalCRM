package reminder

import (
	"fmt"
	"time"
)

// CadenceType represents different reminder cadences
type CadenceType string

const (
	CadenceWeekly    CadenceType = "weekly"
	CadenceBiweekly  CadenceType = "biweekly"
	CadenceMonthly   CadenceType = "monthly"
	CadenceQuarterly CadenceType = "quarterly"
	CadenceBiannual  CadenceType = "biannual"
	CadenceAnnual    CadenceType = "annual"
)

// ParseCadence parses a cadence string into a CadenceType
func ParseCadence(cadence string) (CadenceType, error) {
	switch cadence {
	case "weekly":
		return CadenceWeekly, nil
	case "biweekly":
		return CadenceBiweekly, nil
	case "monthly":
		return CadenceMonthly, nil
	case "quarterly":
		return CadenceQuarterly, nil
	case "biannual":
		return CadenceBiannual, nil
	case "annual":
		return CadenceAnnual, nil
	default:
		return "", fmt.Errorf("unknown cadence: %s", cadence)
	}
}

// CalculateNextDueDate calculates the next due date based on cadence and last contact
// If lastContacted is nil, uses createdAt as the baseline
func CalculateNextDueDate(cadence CadenceType, lastContacted *time.Time, createdAt time.Time) time.Time {
	baseDate := createdAt
	if lastContacted != nil {
		baseDate = *lastContacted
	}

	switch cadence {
	case CadenceWeekly:
		return baseDate.AddDate(0, 0, 7)
	case CadenceBiweekly:
		return baseDate.AddDate(0, 0, 14)
	case CadenceMonthly:
		return baseDate.AddDate(0, 1, 0)
	case CadenceQuarterly:
		return baseDate.AddDate(0, 3, 0)
	case CadenceBiannual:
		return baseDate.AddDate(0, 6, 0)
	case CadenceAnnual:
		return baseDate.AddDate(1, 0, 0)
	default:
		// Default to monthly if unknown cadence
		return baseDate.AddDate(0, 1, 0)
	}
}

// IsOverdue checks if a contact is overdue for contact based on their cadence
func IsOverdue(cadence CadenceType, lastContacted *time.Time, createdAt time.Time, now time.Time) bool {
	if cadence == "" {
		return false // No cadence set, so never overdue
	}

	nextDue := CalculateNextDueDate(cadence, lastContacted, createdAt)
	return now.After(nextDue)
}

// GetOverdueDays returns the number of days a contact is overdue (0 if not overdue)
func GetOverdueDays(cadence CadenceType, lastContacted *time.Time, createdAt time.Time, now time.Time) int {
	if !IsOverdue(cadence, lastContacted, createdAt, now) {
		return 0
	}

	nextDue := CalculateNextDueDate(cadence, lastContacted, createdAt)
	duration := now.Sub(nextDue)
	return int(duration.Hours() / 24)
}

// GetDaysUntilDue returns the number of days until the next contact is due
// Returns negative number if overdue
func GetDaysUntilDue(cadence CadenceType, lastContacted *time.Time, createdAt time.Time, now time.Time) int {
	if cadence == "" {
		return 0 // No cadence set
	}

	nextDue := CalculateNextDueDate(cadence, lastContacted, createdAt)
	duration := nextDue.Sub(now)
	return int(duration.Hours() / 24)
}

// GenerateReminderTitle creates a descriptive title for a reminder
func GenerateReminderTitle(contactName string, cadence CadenceType) string {
	return fmt.Sprintf("Follow up with %s (%s cadence)", contactName, string(cadence))
}

// GenerateReminderDescription creates a descriptive text for a reminder
func GenerateReminderDescription(contactName string, cadence CadenceType, daysSinceLastContact int) string {
	if daysSinceLastContact <= 0 {
		return fmt.Sprintf("Time to reach out to %s as part of your %s follow-up schedule.", contactName, string(cadence))
	}
	return fmt.Sprintf("Time to reach out to %s as part of your %s follow-up schedule. It's been %d days since your last contact.", contactName, string(cadence), daysSinceLastContact)
}
