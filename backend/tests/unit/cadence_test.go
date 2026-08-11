package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCadence tests parsing of cadence strings
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
		{"WEEKLY", "", true}, // Case sensitive
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

// TestCalculateNextDueDate tests calculation of next due dates
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
		{
			name:         "Biannual with last contact",
			cadence:      cadence.CadenceBiannual,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 181, // 6 months from Jan 1
		},
		{
			name:         "Annual with last contact",
			cadence:      cadence.CadenceAnnual,
			lastContact:  &baseDate,
			created:      baseDate,
			expectedDays: 366, // 2024 is a leap year
		},
		{
			name:         "Month-end edge case - Jan 31 + 1 month",
			cadence:      cadence.CadenceMonthly,
			lastContact:  timePtr(time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expectedDays: 0, // Go normalizes to Feb 29 (2024 is leap year) or Mar 2/3
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

// TestIsOverdue tests overdue detection
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
			name:        "Biweekly overdue",
			cadence:     cadence.CadenceBiweekly,
			lastContact: timePtr(time.Date(2023, 12, 25, 12, 0, 0, 0, time.UTC)), // 21 days ago
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			expected:    true, // Should be due Jan 8, now is Jan 15
		},
		{
			name:        "Biweekly not overdue",
			cadence:     cadence.CadenceBiweekly,
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
			cadence:     cadence.CadenceMonthly,
			lastContact: nil,
			created:     time.Date(2023, 11, 1, 12, 0, 0, 0, time.UTC), // 2+ months ago
			expected:    true,
		},
		{
			name:        "Annual not yet due",
			cadence:     cadence.CadenceAnnual,
			lastContact: timePtr(time.Date(2023, 2, 1, 12, 0, 0, 0, time.UTC)), // ~11.5 months ago
			created:     time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.IsOverdue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestGetOverdueDays tests overdue days calculation
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
			name:         "Biweekly 7 days overdue",
			cadence:      cadence.CadenceBiweekly,
			lastContact:  timePtr(time.Date(2023, 12, 25, 12, 0, 0, 0, time.UTC)), // Due Jan 8, now Jan 15
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

// TestGetDaysUntilDue tests days until due calculation
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
			name:         "Biweekly 4 days until due",
			cadence:      cadence.CadenceBiweekly,
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
			result := cadence.GetDaysUntilDue(test.cadence, test.lastContact, test.created, now)
			assert.Equal(t, test.expectedDays, result)
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
			name:        "Staging environment shares production durations",
			envValue:    "staging",
			checkWeekly: 7 * 24 * time.Hour,
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

			config := cadence.GetCadenceConfig()
			assert.Equal(t, test.checkWeekly, config.Weekly)
		})
	}
}

// TestGetCadenceDuration tests duration retrieval for cadence types
func TestGetCadenceDuration(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	tests := []struct {
		name             string
		cadenceType      cadence.CadenceType
		expectedDuration time.Duration
	}{
		{
			name:             "Weekly in production",
			cadenceType:      cadence.CadenceWeekly,
			expectedDuration: 7 * 24 * time.Hour,
		},
		{
			name:             "Monthly in production",
			cadenceType:      cadence.CadenceMonthly,
			expectedDuration: 30 * 24 * time.Hour,
		},
		{
			name:             "Quarterly in production",
			cadenceType:      cadence.CadenceQuarterly,
			expectedDuration: 90 * 24 * time.Hour,
		},
		{
			name:             "Biannual in production",
			cadenceType:      cadence.CadenceBiannual,
			expectedDuration: 180 * 24 * time.Hour,
		},
		{
			name:             "Annual in production",
			cadenceType:      cadence.CadenceAnnual,
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
			result := cadence.GetCadenceDuration(test.cadenceType)
			assert.Equal(t, test.expectedDuration, result)
		})
	}
}

// TestIsOverdueWithConfig tests environment-aware overdue detection
func TestIsOverdueWithConfig(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		cadence     cadence.CadenceType
		lastContact *time.Time
		created     time.Time
		checkTime   time.Time
		expected    bool
	}{
		{
			name:        "Production - weekly overdue",
			env:         "production",
			cadence:     cadence.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // 14 days later
			expected:    true,
		},
		{
			name:        "Test env - weekly overdue (2 min cadence)",
			env:         "test",
			cadence:     cadence.CadenceWeekly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 1, 12, 3, 0, 0, time.UTC), // 3 minutes later
			expected:    true,
		},
		{
			name:        "Accelerated - monthly not overdue (1 hour cadence)",
			env:         "accelerated",
			cadence:     cadence.CadenceMonthly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC), // 30 minutes later
			expected:    false,
		},
		{
			name:        "Staging - monthly not overdue 30 minutes later (production durations)",
			env:         "staging",
			cadence:     cadence.CadenceMonthly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC), // 30 minutes later
			expected:    false,
		},
		{
			name:        "Staging - monthly overdue 31 days later (production durations)",
			env:         "staging",
			cadence:     cadence.CadenceMonthly,
			lastContact: timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:     time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:   time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC), // 31 days later
			expected:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.env)

			result := cadence.IsOverdueWithConfig(test.cadence, test.lastContact, test.created, test.checkTime)
			assert.Equal(t, test.expected, result)
		})
	}
}

// TestGetOverdueDaysWithConfig tests environment-scaled overdue days calculation
func TestGetOverdueDaysWithConfig(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		cadence      cadence.CadenceType
		lastContact  *time.Time
		created      time.Time
		checkTime    time.Time
		expectedDays int
	}{
		{
			name:         "Production - 7 days overdue",
			env:          "production",
			cadence:      cadence.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // Due Jan 8, now Jan 15
			expectedDays: 7,
		},
		{
			name:         "Not overdue returns 0",
			env:          "production",
			cadence:      cadence.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // Due Jan 17, now Jan 15
			expectedDays: 0,
		},
		{
			name:         "Staging - 7 real days overdue (no scaled days)",
			env:          "staging",
			cadence:      cadence.CadenceWeekly,
			lastContact:  timePtr(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
			created:      time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC),
			checkTime:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC), // Due Jan 8, now Jan 15
			expectedDays: 7,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRM_ENV", test.env)

			result := cadence.GetOverdueDaysWithConfig(test.cadence, test.lastContact, test.created, test.checkTime)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

// TestGetOverdueDaysWithConfig_Accelerated covers the CRM_ENV=accelerated scaled-day
// branch (1 "day" = 10 minutes / 7), which the table test above does not reach. That
// branch is guarded: GetOverdueDaysWithConfig returns real 24h days whenever
// TIME_ACCELERATION is active, so the test neutralizes the process clock state (reset →
// isAccelerationActive() false) to fall through to the CRM_ENV switch. checkTime is
// placed exactly overdueDays scaled-days past the due point, so the branch's truncating
// division yields exactly overdueDays. Serial (the clock is package-global state) so it
// does not race sibling cadence tests; deliberately does NOT touch IsTestingMode (#645).
func TestGetOverdueDaysWithConfig_Accelerated(t *testing.T) {
	t.Setenv("CRM_ENV", "accelerated")
	accelerated.Reset()
	t.Cleanup(accelerated.Reset)

	const overdueDays = 5
	scaledDay := 10 * time.Minute / 7 // the accelerated branch's "1 day"

	lastContact := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC)
	// nextContactDue = lastContact + the accelerated weekly duration (read under the
	// same env the SUT sees); checkTime sits exactly overdueDays scaled-days past it.
	nextDue := lastContact.Add(cadence.GetCadenceDuration(cadence.CadenceWeekly))
	checkTime := nextDue.Add(overdueDays * scaledDay)

	result := cadence.GetOverdueDaysWithConfig(cadence.CadenceWeekly, &lastContact, created, checkTime)
	assert.Equal(t, overdueDays, result)
}

// TestBiweeklyCadenceComprehensive tests biweekly cadence across all functions
func TestBiweeklyCadenceComprehensive(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 22, 12, 0, 0, 0, time.UTC)
	lastContact := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC) // 21 days ago
	created := time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC)

	t.Run("Parse biweekly", func(t *testing.T) {
		cad, err := cadence.ParseCadence("biweekly")
		require.NoError(t, err)
		assert.Equal(t, cadence.CadenceBiweekly, cad)
	})

	t.Run("Calculate next due date", func(t *testing.T) {
		nextDue := cadence.CalculateNextDueDate(cadence.CadenceBiweekly, &lastContact, created)
		expected := lastContact.AddDate(0, 0, 14) // 14 days after last contact
		assert.Equal(t, expected, nextDue)
	})

	t.Run("Is overdue", func(t *testing.T) {
		overdue := cadence.IsOverdue(cadence.CadenceBiweekly, &lastContact, created, now)
		assert.True(t, overdue) // 21 days > 14 days
	})

	t.Run("Get overdue days", func(t *testing.T) {
		days := cadence.GetOverdueDays(cadence.CadenceBiweekly, &lastContact, created, now)
		assert.Equal(t, 7, days) // 21 - 14 = 7 days overdue
	})

	t.Run("Get days until due", func(t *testing.T) {
		days := cadence.GetDaysUntilDue(cadence.CadenceBiweekly, &lastContact, created, now)
		assert.Equal(t, -7, days) // Negative means overdue
	})
}

// Helper function to create time pointers
func timePtr(t time.Time) *time.Time {
	return &t
}
