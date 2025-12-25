package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/reminder"

	"github.com/stretchr/testify/assert"
)

func TestParseCadence(t *testing.T) {
	tests := []struct {
		input    string
		expected reminder.CadenceType
		hasError bool
	}{
		{"weekly", reminder.CadenceWeekly, false},
		{"biweekly", reminder.CadenceBiweekly, false},
		{"monthly", reminder.CadenceMonthly, false},
		{"quarterly", reminder.CadenceQuarterly, false},
		{"biannual", reminder.CadenceBiannual, false},
		{"annual", reminder.CadenceAnnual, false},
		{"invalid", "", true},
		{"", "", true},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result, err := reminder.ParseCadence(test.input)

			if test.hasError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, test.expected, result)
			}
		})
	}
}

func TestCalculateNextDueDate(t *testing.T) {
	baseDate := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		cadence      reminder.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int // Days from base date
	}{
		{
			name:         "Weekly with last contact",
			cadence:      reminder.CadenceWeekly,
			lastContact:  &baseDate,
			created:      baseDate.AddDate(0, 0, -30),
			expectedDays: 7,
		},
		{
			name:         "Monthly without last contact",
			cadence:      reminder.CadenceMonthly,
			lastContact:  nil,
			created:      baseDate,
			expectedDays: 31, // January has 31 days
		},
		{
			name:         "Biweekly with last contact",
			cadence:      reminder.CadenceBiweekly,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 14,
		},
		{
			name:         "Quarterly with last contact",
			cadence:      reminder.CadenceQuarterly,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 90, // Approximately 3 months
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.CalculateNextDueDate(test.cadence, test.lastContact, test.created)

			var expectedDate time.Time
			if test.lastContact != nil {
				expectedDate = *test.lastContact
			} else {
				expectedDate = test.created
			}

			// Calculate expected date based on cadence
			switch test.cadence {
			case reminder.CadenceWeekly:
				expectedDate = expectedDate.AddDate(0, 0, 7)
			case reminder.CadenceBiweekly:
				expectedDate = expectedDate.AddDate(0, 0, 14)
			case reminder.CadenceMonthly:
				expectedDate = expectedDate.AddDate(0, 1, 0)
			case reminder.CadenceQuarterly:
				expectedDate = expectedDate.AddDate(0, 3, 0)
			case reminder.CadenceBiannual:
				expectedDate = expectedDate.AddDate(0, 6, 0)
			case reminder.CadenceAnnual:
				expectedDate = expectedDate.AddDate(1, 0, 0)
			}

			assert.Equal(t, expectedDate, result)
		})
	}
}

func TestIsOverdue(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name        string
		cadence     reminder.CadenceType
		lastContact *time.Time
		created     time.Time
		expected    bool
	}{
		{
			name:        "Weekly overdue",
			cadence:     reminder.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)), // 14 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    true, // Should be due Jan 8, now is Jan 15
		},
		{
			name:        "Weekly not overdue",
			cadence:     reminder.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)), // 5 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    false, // Should be due Jan 17, now is Jan 15
		},
		{
			name:        "No cadence never overdue",
			cadence:     "",
			lastContact: timePtr(time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)), // 1 year ago
			created:     time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:    false,
		},
		{
			name:        "Monthly overdue with no last contact",
			cadence:     reminder.CadenceMonthly,
			lastContact: nil,
			created:     time.Date(2023, 11, 1, 12, 0, 0, 0, time.UTC), // 2+ months ago
			expected:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.IsOverdue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestGetOverdueDays(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name         string
		cadence      reminder.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int
	}{
		{
			name:         "Weekly 7 days overdue",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)), // Due Jan 8, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 7,
		},
		{
			name:         "Not overdue returns 0",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)), // Due Jan 17, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 0,
		},
		{
			name:         "No cadence returns 0",
			cadence:      "",
			lastContact:  timePtr(time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GetOverdueDays(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

func TestGetDaysUntilDue(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name         string
		cadence      reminder.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int
	}{
		{
			name:         "Weekly 2 days until due",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)), // Due Jan 17, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 2,
		},
		{
			name:         "Weekly overdue returns negative",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)), // Due Jan 8, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: -7,
		},
		{
			name:         "No cadence returns 0",
			cadence:      "",
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GetDaysUntilDue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

func TestGenerateReminderTitle(t *testing.T) {
	tests := []struct {
		name        string
		contactName string
		cadence     reminder.CadenceType
		expected    string
	}{
		{
			name:        "Weekly cadence",
			contactName: "John Doe",
			cadence:     reminder.CadenceWeekly,
			expected:    "Follow up with John Doe (weekly cadence)",
		},
		{
			name:        "Monthly cadence",
			contactName: "Jane Smith",
			cadence:     reminder.CadenceMonthly,
			expected:    "Follow up with Jane Smith (monthly cadence)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GenerateReminderTitle(test.contactName, test.cadence)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestGenerateReminderDescription(t *testing.T) {
	tests := []struct {
		name                 string
		contactName          string
		cadence              reminder.CadenceType
		daysSinceLastContact int
		expectedToContain    []string
	}{
		{
			name:                 "Recent contact",
			contactName:          "John Doe",
			cadence:              reminder.CadenceWeekly,
			daysSinceLastContact: 0,
			expectedToContain:    []string{"John Doe", "weekly", "reach out"},
		},
		{
			name:                 "Old contact",
			contactName:          "Jane Smith",
			cadence:              reminder.CadenceMonthly,
			daysSinceLastContact: 45,
			expectedToContain:    []string{"Jane Smith", "monthly", "45 days"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GenerateReminderDescription(test.contactName, test.cadence, test.daysSinceLastContact)

			for _, expected := range test.expectedToContain {
				assert.Contains(t, result, expected)
			}
		})
	}
}

// Helper function to create time pointers
func timePtr(t time.Time) *time.Time {
	return &t
}
