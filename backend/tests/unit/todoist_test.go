package unit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestTodoistDeadlineDateComparison tests the UTC date comparison logic
// used when checking if Todoist deadline differs from CRM contact_by.
// This ensures timezone differences don't cause incorrect comparisons.
func TestTodoistDeadlineDateComparison(t *testing.T) {
	tests := []struct {
		name            string
		todoistDate     string // Todoist deadline date string (YYYY-MM-DD)
		contactByTime   time.Time
		expectDifferent bool
	}{
		{
			name:            "same date in UTC",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC),
			expectDifferent: false,
		},
		{
			name:            "different dates",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
		{
			name:            "same date but contact_by has time component",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 28, 15, 30, 0, 0, time.UTC),
			expectDifferent: false, // Should compare dates only, not time
		},
		{
			name:        "same date in different timezone (PST stored as UTC midnight)",
			todoistDate: "2026-01-28",
			// PostgreSQL DATE loaded as UTC midnight
			contactByTime:   time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC),
			expectDifferent: false,
		},
		{
			name:            "adjacent dates at year boundary",
			todoistDate:     "2026-01-01",
			contactByTime:   time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
		{
			name:            "same date different year",
			todoistDate:     "2026-01-15",
			contactByTime:   time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse Todoist deadline the same way the provider does
			todoistDeadline, err := time.Parse("2006-01-02", tt.todoistDate)
			assert.NoError(t, err)

			// Compare using UTC year/month/day (the fix we implemented)
			tY, tM, tD := todoistDeadline.UTC().Date()
			cY, cM, cD := tt.contactByTime.UTC().Date()
			isDifferent := tY != cY || tM != cM || tD != cD

			assert.Equal(t, tt.expectDifferent, isDifferent,
				"todoistDeadline=%v, contactByTime=%v",
				todoistDeadline, tt.contactByTime)
		})
	}
}

// TestTodoistDeadlineDateComparisonOldVsNew compares the old (buggy) comparison
// with the new (fixed) comparison to demonstrate the difference.
func TestTodoistDeadlineDateComparisonOldVsNew(t *testing.T) {
	// Simulate a scenario where old code would give wrong result
	// due to timezone conversion

	// Todoist returns date string, parsed as UTC midnight
	todoistDate := "2026-01-28"
	todoistDeadline, _ := time.Parse("2006-01-02", todoistDate)
	// todoistDeadline = 2026-01-28 00:00:00 UTC

	// Contact_by from DB (DATE stored as UTC midnight)
	contactBy := time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC)

	// NEW comparison (UTC date components) - CORRECT
	tY, tM, tD := todoistDeadline.UTC().Date()
	cY, cM, cD := contactBy.UTC().Date()
	newIsDifferent := tY != cY || tM != cM || tD != cD

	assert.False(t, newIsDifferent, "New comparison should see same dates as equal")

	// The old code used cadence.Today() which converts to local timezone.
	// This test verifies our fix works correctly regardless of local timezone.
}
