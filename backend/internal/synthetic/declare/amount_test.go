package declare

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAmountResolveProduction(t *testing.T) {
	t.Setenv("CRM_ENV", "production")
	assert.Equal(t, 3*24*time.Hour, Days(3).resolve("weekly"))
	assert.Equal(t, 14*24*time.Hour, Periods(2).resolve("weekly"))
	assert.Equal(t, 60*24*time.Hour, Periods(2).resolve("monthly"))
	assert.Equal(t, 33*24*time.Hour, Periods(1).Plus(Days(3)).resolve("monthly"))
	assert.Equal(t, time.Duration(0), Amount{}.resolve("weekly"))
}

// The G-C case: under CRM_ENV=testing the cadence table is NOT proportional, so
// a period-stated amount must come from the monthly ENTRY (10min), never from
// 30 × dayLength (≈8.6min). Getting this wrong makes a "monthly overdue" fixture
// read as NOT overdue in the E2E environment.
func TestAmountResolveTestingIsNonProportional(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	assert.Equal(t, 10*time.Minute, Periods(1).resolve("monthly"))
	assert.NotEqual(t, 30*dayLength(), Periods(1).resolve("monthly"))
	assert.Equal(t, 3*(2*time.Minute/7), Days(3).resolve("weekly"))
	assert.Equal(t, 2*time.Hour, Periods(1).resolve("annual"))
}

func TestAmountShapeHelpers(t *testing.T) {
	assert.True(t, Periods(1).needsCadence())
	assert.False(t, Days(9).needsCadence())
	assert.True(t, Days(-1).negative())
	assert.True(t, Periods(-1).negative())
	assert.False(t, Days(0).negative())

	assert.Equal(t, "3d", Days(3).String())
	assert.Equal(t, "2p", Periods(2).String())
	assert.Equal(t, "3d+2p", Days(3).Plus(Periods(2)).String())
	assert.Equal(t, "0d", Amount{}.String())
}
