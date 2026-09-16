package service

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

func TestPlanSkip_RequiresCadence(t *testing.T) {
	now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
	for _, cadenceValue := range []*string{nil, stringPtrForSkip("")} {
		_, err := planSkip(&repository.Contact{Cadence: cadenceValue}, now)
		require.ErrorIs(t, err, ErrSkipRequiresCadence)
	}
}

func TestPlanSkip_UnknownCadence(t *testing.T) {
	cadenceValue := "fortnightly"
	_, err := planSkip(&repository.Contact{Cadence: &cadenceValue}, time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSkipRequiresCadence)
}

func TestPlanSkip_LaterOf(t *testing.T) {
	now := time.Date(2026, 9, 15, 9, 0, 0, 0, time.Local)
	cadenceValue := "monthly"
	cadenceType := cadence.CadenceMonthly
	cases := []struct {
		name      string
		contactBy *time.Time
	}{
		{name: "past", contactBy: timePtrForSkip(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))},
		{name: "future", contactBy: timePtrForSkip(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planSkip(&repository.Contact{Cadence: &cadenceValue, ContactBy: tc.contactBy}, now)
			require.NoError(t, err)
			want := cadence.NextAfterSkip(tc.contactBy, cadenceType, now)
			require.Equal(t, cadence.CalendarDate(want), cadence.CalendarDate(got))
		})
	}
}

func stringPtrForSkip(value string) *string { return &value }

func timePtrForSkip(value time.Time) *time.Time { return &value }
