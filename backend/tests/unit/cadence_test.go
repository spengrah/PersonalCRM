package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
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

// TestGetOverdueDaysWithConfig_AccelerationActive covers the branch the test
// above deliberately neutralizes: when the process clock IS active,
// GetOverdueDaysWithConfig must take the real-24h-days branch and skip the
// CRM_ENV scaled-day switch entirely — even with CRM_ENV set to "accelerated",
// which would otherwise select the compressed 10-minutes-per-week "day". An
// isAccelerationActive implementation that never actually observes the
// process clock (e.g. a body that unconditionally returns false) would fall
// through to the scaled-day branch instead and produce a count off by three
// orders of magnitude, so this test — not just its inactive/scaled sibling
// above — is what proves the active branch is reachable and correct.
func TestGetOverdueDaysWithConfig_AccelerationActive(t *testing.T) {
	t.Setenv("CRM_ENV", "accelerated")
	accelerated.Configure(60, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	t.Cleanup(accelerated.Reset)

	const overdueDays = 5
	lastContact := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2023, 12, 1, 12, 0, 0, 0, time.UTC)
	// nextContactDue = lastContact + the accelerated weekly duration
	// (GetCadenceDuration itself still reads CRM_ENV regardless of process
	// acceleration state); checkTime sits exactly overdueDays REAL 24h days
	// past it, since that's the unit the active branch must use.
	nextDue := lastContact.Add(cadence.GetCadenceDuration(cadence.CadenceWeekly))
	checkTime := nextDue.Add(overdueDays * 24 * time.Hour)

	result := cadence.GetOverdueDaysWithConfig(cadence.CadenceWeekly, &lastContact, created, checkTime)
	assert.Equal(t, overdueDays, result)
}

// Helper function to create time pointers
func timePtr(t time.Time) *time.Time {
	return &t
}
