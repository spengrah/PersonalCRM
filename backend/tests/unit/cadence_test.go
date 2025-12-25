package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/reminder"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCadence tests parsing of cadence strings
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
		{"WEEKLY", "", true}, // Case sensitive
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

// TestCalculateNextDueDate tests calculation of next due dates
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
		{
			name:         "Biannual with last contact",
			cadence:      reminder.CadenceBiannual,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 181, // 6 months from Jan 1
		},
		{
			name:         "Annual with last contact",
			cadence:      reminder.CadenceAnnual,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 366, // 2024 is a leap year
		},
		{
			name:         "Month-end edge case - Jan 31 + 1 month",
			cadence:      reminder.CadenceMonthly,
			lastContact:  timePtr(time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 0, // Go normalizes to Feb 29 (2024 is leap year) or Mar 2/3
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

// TestIsOverdue tests overdue detection
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
			name:        "Biweekly overdue",
			cadence:     reminder.CadenceBiweekly,
			lastContact: timePtr(time.Date(2023, 12, 25, 12, 0, 0, 0, time.UTC)), // 21 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    true, // Should be due Jan 8, now is Jan 15
		},
		{
			name:        "Biweekly not overdue",
			cadence:     reminder.CadenceBiweekly,
			lastContact: timePtr(time.Date(2024, 1, 5, 12, 0, 0, 0, time.UTC)), // 10 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    false, // Should be due Jan 19, now is Jan 15
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
		{
			name:        "Annual not yet due",
			cadence:     reminder.CadenceAnnual,
			lastContact: timePtr(time.Date(2023, 2, 1, 12, 0, 0, 0, time.UTC)), // ~11.5 months ago
			created:     time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.IsOverdue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestGetOverdueDays tests overdue days calculation
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
			name:         "Biweekly 7 days overdue",
			cadence:      reminder.CadenceBiweekly,
			lastContact:  timePtr(time.Date(2023, 12, 25, 12, 0, 0, 0, time.UTC)), // Due Jan 8, now Jan 15
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

// TestGetDaysUntilDue tests days until due calculation
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
			name:         "Biweekly 4 days until due",
			cadence:      reminder.CadenceBiweekly,
			lastContact:  timePtr(time.Date(2024, 1, 5, 12, 0, 0, 0, time.UTC)), // Due Jan 19, now Jan 15
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 4,
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

// TestGenerateReminderTitle tests reminder title generation
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
		{
			name:        "Biweekly cadence",
			contactName: "Bob Johnson",
			cadence:     reminder.CadenceBiweekly,
			expected:    "Follow up with Bob Johnson (biweekly cadence)",
		},
		{
			name:        "Annual cadence",
			contactName: "Alice Brown",
			cadence:     reminder.CadenceAnnual,
			expected:    "Follow up with Alice Brown (annual cadence)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GenerateReminderTitle(test.contactName, test.cadence)
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestGenerateReminderDescription tests reminder description generation
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
		{
			name:                 "Quarterly with moderate gap",
			contactName:          "Bob Johnson",
			cadence:              reminder.CadenceQuarterly,
			daysSinceLastContact: 100,
			expectedToContain:    []string{"Bob Johnson", "quarterly", "100 days"},
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

// TestGetCadenceConfig tests environment-aware cadence configuration
func TestGetCadenceConfig(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		checkWeekly time.Duration
	}{
		{
			name:        "Test environment",
			envValue:    "test",
			checkWeekly: 2 * time.Minute,
		},
		{
			name:        "Testing environment",
			envValue:    "testing",
			checkWeekly: 2 * time.Minute,
		},
		{
			name:        "Staging environment",
			envValue:    "staging",
			checkWeekly: 10 * time.Minute,
		},
		{
			name:        "Accelerated environment",
			envValue:    "accelerated",
			checkWeekly: 10 * time.Minute,
		},
		{
			name:        "Production environment",
			envValue:    "production",
			checkWeekly: 7 * 24 * time.Hour,
		},
		{
			name:        "Prod environment",
			envValue:    "prod",
			checkWeekly: 7 * 24 * time.Hour,
		},
		{
			name:        "Empty defaults to production",
			envValue:    "",
			checkWeekly: 7 * 24 * time.Hour,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.envValue)

			config := reminder.GetCadenceConfig()
			assert.Equal(t, test.checkWeekly, config.Weekly)
		})
	}
}

// TestGetCadenceDuration tests duration retrieval for cadence types
func TestGetCadenceDuration(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	tests := []struct {
		name             string
		cadenceType      reminder.CadenceType
		expectedDuration time.Duration
	}{
		{
			name:             "Weekly in production",
			cadenceType:      reminder.CadenceWeekly,
			expectedDuration: 7 * 24 * time.Hour,
		},
		{
			name:             "Monthly in production",
			cadenceType:      reminder.CadenceMonthly,
			expectedDuration: 30 * 24 * time.Hour,
		},
		{
			name:             "Quarterly in production",
			cadenceType:      reminder.CadenceQuarterly,
			expectedDuration: 90 * 24 * time.Hour,
		},
		{
			name:             "Biannual in production",
			cadenceType:      reminder.CadenceBiannual,
			expectedDuration: 180 * 24 * time.Hour,
		},
		{
			name:             "Annual in production",
			cadenceType:      reminder.CadenceAnnual,
			expectedDuration: 365 * 24 * time.Hour,
		},
		{
			name:             "Unknown defaults to monthly",
			cadenceType:      "unknown",
			expectedDuration: 30 * 24 * time.Hour,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := reminder.GetCadenceDuration(test.cadenceType)
			assert.Equal(t, test.expectedDuration, result)
		})
	}
}

// TestIsOverdueWithConfig tests environment-aware overdue detection
func TestIsOverdueWithConfig(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		cadence     reminder.CadenceType
		lastContact *time.Time
		created     time.Time
		checkTime   time.Time
		expected    bool
	}{
		{
			name:        "Production - weekly overdue",
			env:         "production",
			cadence:     reminder.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // 14 days later
			expected:    true,
		},
		{
			name:        "Test env - weekly overdue (2 min cadence)",
			env:         "test",
			cadence:     reminder.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 1, 12, 3, 0, 0, time.UTC), // 3 minutes later
			expected:    true,
		},
		{
			name:        "Staging - monthly not overdue (1 hour cadence)",
			env:         "staging",
			cadence:     reminder.CadenceMonthly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC), // 30 minutes later
			expected:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.env)

			result := reminder.IsOverdueWithConfig(test.cadence, test.lastContact, test.created, test.checkTime)
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestGetOverdueDaysWithConfig tests environment-scaled overdue days calculation
func TestGetOverdueDaysWithConfig(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		cadence      reminder.CadenceType
		lastContact  *time.Time
		created      time.Time
		checkTime    time.Time
		expectedDays int
	}{
		{
			name:         "Production - 7 days overdue",
			env:          "production",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // Due Jan 8, now Jan 15
			expectedDays: 7,
		},
		{
			name:         "Not overdue returns 0",
			env:          "production",
			cadence:      reminder.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // Due Jan 17, now Jan 15
			expectedDays: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.env)

			result := reminder.GetOverdueDaysWithConfig(test.cadence, test.lastContact, test.created, test.checkTime)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

// TestGetSchedulerCronSpec tests cron specification per environment
func TestGetSchedulerCronSpec(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		expected string
	}{
		{
			name:     "Test environment - every 30s",
			env:      "test",
			expected: "@every 30s",
		},
		{
			name:     "Testing environment - every 30s",
			env:      "testing",
			expected: "@every 30s",
		},
		{
			name:     "Staging environment - every 5m",
			env:      "staging",
			expected: "@every 5m",
		},
		{
			name:     "Accelerated environment - every 5m",
			env:      "accelerated",
			expected: "@every 5m",
		},
		{
			name:     "Production environment - daily at 8 AM",
			env:      "production",
			expected: "0 0 8 * * *",
		},
		{
			name:     "Prod environment - daily at 8 AM",
			env:      "prod",
			expected: "0 0 8 * * *",
		},
		{
			name:     "Empty defaults to production",
			env:      "",
			expected: "0 0 8 * * *",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.env)

			result := reminder.GetSchedulerCronSpec()
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestBiweeklyCadenceComprehensive tests biweekly cadence across all functions
func TestBiweeklyCadenceComprehensive(t *testing.T) {
	now := time.Date(2024, 1, 22, 12, 0, 0, 0, time.UTC)
	lastContact := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC) // 21 days ago
	created := time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC)

	t.Run("Parse biweekly", func(t *testing.T) {
		cadence, err := reminder.ParseCadence("biweekly")
		require.NoError(t, err)
		assert.Equal(t, reminder.CadenceBiweekly, cadence)
	})

	t.Run("Calculate next due date", func(t *testing.T) {
		nextDue := reminder.CalculateNextDueDate(reminder.CadenceBiweekly, &lastContact, created)
		expected := lastContact.AddDate(0, 0, 14) // 14 days after last contact
		assert.Equal(t, expected, nextDue)
	})

	t.Run("Is overdue", func(t *testing.T) {
		overdue := reminder.IsOverdue(reminder.CadenceBiweekly, &lastContact, created, now)
		assert.True(t, overdue) // 21 days > 14 days
	})

	t.Run("Get overdue days", func(t *testing.T) {
		days := reminder.GetOverdueDays(reminder.CadenceBiweekly, &lastContact, created, now)
		assert.Equal(t, 7, days) // 21 - 14 = 7 days overdue
	})

	t.Run("Get days until due", func(t *testing.T) {
		days := reminder.GetDaysUntilDue(reminder.CadenceBiweekly, &lastContact, created, now)
		assert.Equal(t, -7, days) // Negative means overdue
	})
}

// Helper function to create time pointers
func timePtr(t time.Time) *time.Time {
	return &t
}
