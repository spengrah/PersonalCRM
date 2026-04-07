package service

import (
	"testing"

	"personal-crm/backend/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestWatchdogDaysForCadence(t *testing.T) {
	cfg := config.WatchdogConfig{
		WeeklyDays:    3,
		BiweeklyDays:  5,
		MonthlyDays:   7,
		QuarterlyDays: 14,
		BiannualDays:  21,
		AnnualDays:    21,
	}

	tests := []struct {
		cadence  string
		expected int
	}{
		{"weekly", 3},
		{"biweekly", 5},
		{"monthly", 7},
		{"quarterly", 14},
		{"biannual", 21},
		{"annual", 21},
		{"", 0},
		{"unknown", 0},
	}

	for _, tc := range tests {
		t.Run(tc.cadence, func(t *testing.T) {
			days := watchdogDaysForCadence(tc.cadence, cfg)
			assert.Equal(t, tc.expected, days)
		})
	}
}

func TestWatchdogDaysForCadence_CustomConfig(t *testing.T) {
	cfg := config.WatchdogConfig{
		WeeklyDays:    1,
		BiweeklyDays:  2,
		MonthlyDays:   3,
		QuarterlyDays: 4,
		BiannualDays:  5,
		AnnualDays:    6,
	}

	assert.Equal(t, 1, watchdogDaysForCadence("weekly", cfg))
	assert.Equal(t, 6, watchdogDaysForCadence("annual", cfg))
}
