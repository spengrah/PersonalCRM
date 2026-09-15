package unit

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"

	"github.com/stretchr/testify/require"
)

func TestIsAwaitingReply(t *testing.T) {
	base := time.Date(2025, time.January, 10, 12, 0, 0, 0, time.Local)
	outreach := base
	openUntil := time.Date(2025, time.January, 17, 0, 0, 0, 0, time.Local)
	responseBefore := outreach.Add(-time.Second)
	responseEqual := outreach
	responseAfter := outreach.Add(time.Second)
	expiryDay := time.Date(2025, time.January, 17, 18, 0, 0, 0, time.Local)
	nextDay := time.Date(2025, time.January, 18, 0, 1, 0, 0, time.Local)

	tests := []struct {
		name     string
		outreach *time.Time
		response *time.Time
		until    *time.Time
		now      time.Time
		want     bool
	}{
		{name: "nil outreach", response: &responseBefore, until: &openUntil, now: base, want: false},
		{name: "nil expiry", outreach: &outreach, response: &responseBefore, now: base, want: false},
		{name: "response equals outreach", outreach: &outreach, response: &responseEqual, until: &openUntil, now: base, want: false},
		{name: "response before outreach", outreach: &outreach, response: &responseBefore, until: &openUntil, now: base, want: true},
		{name: "response after outreach", outreach: &outreach, response: &responseAfter, until: &openUntil, now: base, want: false},
		{name: "expiry date is included", outreach: &outreach, response: &responseBefore, until: &openUntil, now: expiryDay, want: true},
		{name: "day after expiry", outreach: &outreach, response: &responseBefore, until: &openUntil, now: nextDay, want: false},
		{name: "nil response with open window", outreach: &outreach, until: &openUntil, now: base, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, cadence.IsAwaitingReply(test.outreach, test.response, test.until, test.now))
		})
	}
}

func TestIsAwaitingReply_BoundaryIsCalendarDate(t *testing.T) {
	previousLocal := time.Local
	t.Cleanup(func() { time.Local = previousLocal })

	chicago, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)
	time.Local = chicago

	until := time.Date(2025, time.January, 10, 0, 0, 0, 0, time.UTC)
	outreach := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.Local)
	assertExpiryDate := func(location *time.Location) {
		time.Local = location
		t.Run(location.String(), func(t *testing.T) {
			require.True(t, cadence.IsAwaitingReply(&outreach, nil, &until, time.Date(2025, time.January, 10, 23, 59, 0, 0, time.Local)))
			require.False(t, cadence.IsAwaitingReply(&outreach, nil, &until, time.Date(2025, time.January, 11, 0, 1, 0, 0, time.Local)))
		})
	}
	assertExpiryDate(chicago)
	assertExpiryDate(time.UTC)
}

func TestAwaitingReplyUntil(t *testing.T) {
	occurredAt := time.Date(2025, time.January, 10, 12, 30, 0, 0, time.Local)
	today := cadence.Today(occurredAt)

	require.Equal(t, today.AddDate(0, 0, 7), cadence.AwaitingReplyUntil(occurredAt, 7))
	require.Equal(t, today, cadence.AwaitingReplyUntil(occurredAt, 0))
}

func TestWatchdogConfig_DaysForCadence(t *testing.T) {
	watchdog := config.TestConfig().Watchdog
	tests := []struct {
		cadence string
		want    int
	}{
		{cadence: "weekly", want: 3},
		{cadence: "biweekly", want: 5},
		{cadence: "monthly", want: 7},
		{cadence: "quarterly", want: 14},
		{cadence: "biannual", want: 21},
		{cadence: "annual", want: 21},
		{cadence: "", want: 0},
		{cadence: "gibberish", want: 0},
	}

	for _, test := range tests {
		t.Run(test.cadence, func(t *testing.T) {
			require.Equal(t, test.want, watchdog.DaysForCadence(test.cadence))
		})
	}
}
