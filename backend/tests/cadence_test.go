package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
)

func TestParseCadence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected cadence.CadenceType
		hasError bool
	}{
		{"weekly", cadence.CadenceWeekly, false},
		{"biweekly", cadence.CadenceBiweekly, false},
		{"monthly", cadence.CadenceMonthly, false},
		{"quarterly", cadence.CadenceQuarterly, false},
		{"biannual", cadence.CadenceBiannual, false},
		{"annual", cadence.CadenceAnnual, false},
		{"invalid", "", true},
		{"", "", true},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result, err := cadence.ParseCadence(test.input)

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
	t.Parallel()
	baseDate := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		cadence      cadence.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int // Days from base date
	}{
		{
			name:         "Weekly with last contact",
			cadence:      cadence.CadenceWeekly,
			lastContact:  &baseDate,
			created:      baseDate.AddDate(0, 0, -30),
			expectedDays: 7,
		},
		{
			name:         "Monthly without last contact",
			cadence:      cadence.CadenceMonthly,
			lastContact:  nil,
			created:      baseDate,
			expectedDays: 31, // January has 31 days
		},
		{
			name:         "Biweekly with last contact",
			cadence:      cadence.CadenceBiweekly,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 14,
		},
		{
			name:         "Quarterly with last contact",
			cadence:      cadence.CadenceQuarterly,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 90, // Approximately 3 months
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.CalculateNextDueDate(test.cadence, test.lastContact, test.created)

			var expectedDate time.Time
			if test.lastContact != nil {
				expectedDate = *test.lastContact
			} else {
				expectedDate = test.created
			}

			// Calculate expected date based on cadence
			switch test.cadence {
			case cadence.CadenceWeekly:
				expectedDate = expectedDate.AddDate(0, 0, 7)
			case cadence.CadenceBiweekly:
				expectedDate = expectedDate.AddDate(0, 0, 14)
			case cadence.CadenceMonthly:
				expectedDate = expectedDate.AddDate(0, 1, 0)
			case cadence.CadenceQuarterly:
				expectedDate = expectedDate.AddDate(0, 3, 0)
			case cadence.CadenceBiannual:
				expectedDate = expectedDate.AddDate(0, 6, 0)
			case cadence.CadenceAnnual:
				expectedDate = expectedDate.AddDate(1, 0, 0)
			}

			assert.Equal(t, expectedDate, result)
		})
	}
}

func TestIsOverdue(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name        string
		cadence     cadence.CadenceType
		lastContact *time.Time
		created     time.Time
		expected    bool
	}{
		{
			name:        "Weekly overdue",
			cadence:     cadence.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)), // 14 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    true, // Should be due Jan 8, now is Jan 15
		},
		{
			name:        "Weekly not overdue",
			cadence:     cadence.CadenceWeekly,
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
			cadence:     cadence.CadenceMonthly,
			lastContact: nil,
			created:     time.Date(2023, 11, 1, 12, 0, 0, 0, time.UTC), // 2+ months ago
			expected:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.IsOverdue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestGetOverdueDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name         string
		cadence      cadence.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int
	}{
		{
			name:         "Weekly 7 days overdue",
			cadence:      cadence.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)), // Due Jan 8, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 7,
		},
		{
			name:         "Not overdue returns 0",
			cadence:      cadence.CadenceWeekly,
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
			result := cadence.GetOverdueDays(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

func TestGetDaysUntilDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) // Jan 15, 2024

	tests := []struct {
		name         string
		cadence      cadence.CadenceType
		lastContact  *time.Time
		created      time.Time
		expectedDays int
	}{
		{
			name:         "Weekly 2 days until due",
			cadence:      cadence.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)), // Due Jan 17, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 2,
		},
		{
			name:         "Weekly overdue returns negative",
			cadence:      cadence.CadenceWeekly,
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
			result := cadence.GetDaysUntilDue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

// Helper function to create time pointers
func timePtr(t time.Time) *time.Time {
	return &t
}
