package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
)

func TestDateOnly(t *testing.T) {
	tests := []struct {
		name  string
		input time.Time
	}{
		{
			name:  "Truncates time to date in local timezone",
			input: time.Date(2024, 1, 15, 14, 30, 45, 123456789, time.Local),
		},
		{
			name:  "Midnight stays at date",
			input: time.Date(2024, 3, 20, 0, 0, 0, 0, time.Local),
		},
		{
			name:  "End of day truncates",
			input: time.Date(2024, 12, 31, 23, 59, 59, 999999999, time.Local),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.DateOnly(test.input)
			// DateOnly should preserve the date but zero out the time components
			local := test.input.In(time.Local)
			assert.Equal(t, local.Year(), result.Year())
			assert.Equal(t, local.Month(), result.Month())
			assert.Equal(t, local.Day(), result.Day())
			assert.Equal(t, 0, result.Hour())
			assert.Equal(t, 0, result.Minute())
			assert.Equal(t, 0, result.Second())
			assert.Equal(t, 0, result.Nanosecond())
			assert.Equal(t, time.Local, result.Location())
		})
	}
}

func TestToday(t *testing.T) {
	now := time.Date(2024, 6, 15, 16, 45, 30, 0, time.UTC)
	today := cadence.Today(now)

	// Should be the date portion with time zeroed
	assert.Equal(t, 0, today.Hour())
	assert.Equal(t, 0, today.Minute())
	assert.Equal(t, 0, today.Second())
	assert.Equal(t, time.Local, today.Location())
}

func TestCadenceDays(t *testing.T) {
	tests := []struct {
		cadence      cadence.CadenceType
		expectedDays int
	}{
		{cadence.CadenceWeekly, 7},
		{cadence.CadenceBiweekly, 14},
		{cadence.CadenceMonthly, 30},
		{cadence.CadenceQuarterly, 90},
		{cadence.CadenceBiannual, 180},
		{cadence.CadenceAnnual, 365},
		{"invalid", 0},
		{"", 0},
	}

	for _, test := range tests {
		t.Run(string(test.cadence), func(t *testing.T) {
			result := cadence.CadenceDays(test.cadence)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}

func TestCalculateContactBy(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 30, 0, 0, time.Local)

	tests := []struct {
		name         string
		cadence      cadence.CadenceType
		expectedDate time.Time
	}{
		{
			name:         "Weekly adds 7 days",
			cadence:      cadence.CadenceWeekly,
			expectedDate: time.Date(2024, 1, 22, 0, 0, 0, 0, time.Local),
		},
		{
			name:         "Biweekly adds 14 days",
			cadence:      cadence.CadenceBiweekly,
			expectedDate: time.Date(2024, 1, 29, 0, 0, 0, 0, time.Local),
		},
		{
			name:         "Monthly adds 30 days",
			cadence:      cadence.CadenceMonthly,
			expectedDate: time.Date(2024, 2, 14, 0, 0, 0, 0, time.Local),
		},
		{
			name:         "Quarterly adds 90 days",
			cadence:      cadence.CadenceQuarterly,
			expectedDate: time.Date(2024, 4, 14, 0, 0, 0, 0, time.Local),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.CalculateContactBy(base, test.cadence)
			assert.Equal(t, test.expectedDate, result)
		})
	}
}

func TestIsContactByOverdue(t *testing.T) {
	tests := []struct {
		name      string
		contactBy time.Time
		now       time.Time
		expected  bool
	}{
		{
			name:      "Contact due yesterday is overdue",
			contactBy: time.Date(2024, 1, 14, 0, 0, 0, 0, time.Local),
			now:       time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expected:  true,
		},
		{
			name:      "Contact due today is not overdue",
			contactBy: time.Date(2024, 1, 15, 0, 0, 0, 0, time.Local),
			now:       time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expected:  false,
		},
		{
			name:      "Contact due tomorrow is not overdue",
			contactBy: time.Date(2024, 1, 16, 0, 0, 0, 0, time.Local),
			now:       time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expected:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.IsContactByOverdue(test.contactBy, test.now)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestGetContactByOverdueDays(t *testing.T) {
	tests := []struct {
		name         string
		contactBy    time.Time
		now          time.Time
		expectedDays int
	}{
		{
			name:         "1 day overdue",
			contactBy:    time.Date(2024, 1, 14, 0, 0, 0, 0, time.Local),
			now:          time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expectedDays: 1,
		},
		{
			name:         "7 days overdue",
			contactBy:    time.Date(2024, 1, 8, 0, 0, 0, 0, time.Local),
			now:          time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expectedDays: 7,
		},
		{
			name:         "Not overdue returns 0",
			contactBy:    time.Date(2024, 1, 15, 0, 0, 0, 0, time.Local),
			now:          time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expectedDays: 0,
		},
		{
			name:         "Future date returns 0",
			contactBy:    time.Date(2024, 1, 20, 0, 0, 0, 0, time.Local),
			now:          time.Date(2024, 1, 15, 10, 0, 0, 0, time.Local),
			expectedDays: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.GetContactByOverdueDays(test.contactBy, test.now)
			assert.Equal(t, test.expectedDays, result)
		})
	}
}
