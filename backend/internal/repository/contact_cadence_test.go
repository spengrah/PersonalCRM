package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestShouldApplyContactBy pins the gate that decides whether an interaction
// recomputes contact_by. The nil-prev row is the behavior CON-001 now depends on:
// creation leaves last_contacted NULL, so a subsequently-applied backdated automated
// interaction re-derives contact_by from that past instant. Before last_contacted
// started NULL, prev was the creation stamp (~now) and a past-dated automated
// interaction was rejected (past.After(now) == false) — the pinned contrast below.
func TestShouldApplyContactBy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	past := now.Add(-60 * 24 * time.Hour)
	future := now.Add(24 * time.Hour)

	cases := []struct {
		name       string
		prev       *time.Time
		occurredAt time.Time
		isManual   bool
		hasCadence bool
		want       bool
	}{
		{
			name:       "nil prev, backdated automated interaction applies (CON-001 NULL start)",
			prev:       nil,
			occurredAt: past,
			isManual:   false,
			hasCadence: true,
			want:       true,
		},
		{
			name:       "creation-stamp prev blocks the same backdated automated interaction (pre-change behavior)",
			prev:       &now,
			occurredAt: past,
			isManual:   false,
			hasCadence: true,
			want:       false,
		},
		{
			name:       "automated interaction strictly newer than prev applies",
			prev:       &past,
			occurredAt: now,
			isManual:   false,
			hasCadence: true,
			want:       true,
		},
		{
			name:       "manual entry always applies, even backdated below prev (user correction)",
			prev:       &now,
			occurredAt: past,
			isManual:   true,
			hasCadence: true,
			want:       true,
		},
		{
			name:       "no cadence never applies, even for a newer interaction",
			prev:       &past,
			occurredAt: future,
			isManual:   false,
			hasCadence: false,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ShouldApplyContactBy(tc.prev, tc.occurredAt, tc.isManual, tc.hasCadence)
			assert.Equal(t, tc.want, got)
		})
	}
}
