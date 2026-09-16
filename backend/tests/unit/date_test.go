package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
)

func TestDateOnly(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	now := time.Date(2024, 6, 15, 16, 45, 30, 0, time.UTC)
	today := cadence.Today(now)

	// Should be the date portion with time zeroed
	assert.Equal(t, 0, today.Hour())
	assert.Equal(t, 0, today.Minute())
	assert.Equal(t, 0, today.Second())
	assert.Equal(t, time.Local, today.Location())
}

func TestNextAfterSkip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
	past := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	future := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	today := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		current     *time.Time
		cadenceType cadence.CadenceType
		expected    time.Time
	}{
		{
			name:        "monthly with no current date uses today",
			cadenceType: cadence.CadenceMonthly,
			expected:    time.Date(2026, 10, 15, 0, 0, 0, 0, time.Local),
		},
		{
			name:        "monthly past date uses today branch",
			current:     &past,
			cadenceType: cadence.CadenceMonthly,
			expected:    time.Date(2026, 10, 15, 0, 0, 0, 0, time.Local),
		},
		{
			name:        "monthly future date uses current branch",
			current:     &future,
			cadenceType: cadence.CadenceMonthly,
			expected:    time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local),
		},
		{
			name:        "monthly tie uses today branch",
			current:     &today,
			cadenceType: cadence.CadenceMonthly,
			expected:    time.Date(2026, 10, 15, 0, 0, 0, 0, time.Local),
		},
		{
			name:        "weekly future date uses current branch",
			current:     &future,
			cadenceType: cadence.CadenceWeekly,
			expected:    time.Date(2026, 9, 27, 0, 0, 0, 0, time.Local),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := cadence.NextAfterSkip(test.current, test.cadenceType, now)
			assert.Equal(t, cadence.CalendarDate(test.expected), cadence.CalendarDate(result))
			assert.Equal(t, time.Local, result.Location())
		})
	}
}

func TestNextAfterSkip_DateShapedCurrentUsesCalendarDay(t *testing.T) {
	previousLocal := time.Local
	t.Cleanup(func() { time.Local = previousLocal })

	chicago, err := time.LoadLocation("America/Chicago")
	if !assert.NoError(t, err) {
		return
	}
	current := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	t.Run("Chicago preserves the DATE calendar day", func(t *testing.T) {
		time.Local = chicago
		now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
		result := cadence.NextAfterSkip(&current, cadence.CadenceMonthly, now)
		expected := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
		assert.Equal(t, cadence.CalendarDate(expected), cadence.CalendarDate(result))
	})

	t.Run("UTC preserves the DATE calendar day", func(t *testing.T) {
		time.Local = time.UTC
		now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
		result := cadence.NextAfterSkip(&current, cadence.CadenceMonthly, now)
		expected := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
		assert.Equal(t, cadence.CalendarDate(expected), cadence.CalendarDate(result))
	})
}

func TestUndoSkipAvailable(t *testing.T) {
	previousLocal := time.Local
	t.Cleanup(func() { time.Local = previousLocal })

	chicago, err := time.LoadLocation("America/Chicago")
	if !assert.NoError(t, err) {
		return
	}

	for _, zone := range []struct {
		name     string
		location *time.Location
	}{
		{name: "UTC", location: time.UTC},
		{name: "America Chicago", location: chicago},
	} {
		t.Run(zone.name, func(t *testing.T) {
			time.Local = zone.location
			now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
			lastSkippedAt := now.Add(-time.Hour)
			today := cadence.Today(now)
			tomorrowDate := today.AddDate(0, 0, 1)
			yesterdayDate := today.AddDate(0, 0, -1)
			contactByTomorrow := time.Date(tomorrowDate.Year(), tomorrowDate.Month(), tomorrowDate.Day(), 0, 0, 0, 0, time.UTC)
			contactByToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
			contactByYesterday := time.Date(yesterdayDate.Year(), yesterdayDate.Month(), yesterdayDate.Day(), 0, 0, 0, 0, time.UTC)

			assert.False(t, cadence.UndoSkipAvailable(nil, &contactByTomorrow, now))
			assert.False(t, cadence.UndoSkipAvailable(&lastSkippedAt, nil, now))
			assert.True(t, cadence.UndoSkipAvailable(&lastSkippedAt, &contactByTomorrow, now))
			assert.False(t, cadence.UndoSkipAvailable(&lastSkippedAt, &contactByToday, now))
			assert.False(t, cadence.UndoSkipAvailable(&lastSkippedAt, &contactByYesterday, now))
		})
	}
}

func TestCalculateContactBy(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
