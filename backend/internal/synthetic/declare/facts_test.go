package declare

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the TRIPWIRE for the independence rule: declare states the
// cadence periods locally (facts.go) so a regression in the app's cadence math
// makes fixtures fail loudly instead of tracking the regression. That
// duplication is only safe if an INTENTIONAL product change is forced to be a
// conscious two-sided edit — which is what these assertions do. The import of
// internal/cadence here is legal precisely because this is the test binary;
// imports_test.go proves no non-test file in the tree imports it.

func configAsMap(c cadence.CadenceConfig) map[string]time.Duration {
	return map[string]time.Duration{
		"weekly":    c.Weekly,
		"biweekly":  c.Biweekly,
		"monthly":   c.Monthly,
		"quarterly": c.Quarterly,
		"biannual":  c.Biannual,
		"annual":    c.Annual,
	}
}

func TestFactsMatchProductionCadenceTable(t *testing.T) {
	assert.Equal(t, configAsMap(cadence.ProductionCadenceConfig()), productionPeriods,
		"declare's production period table drifted from cadence.ProductionCadenceConfig()")
}

func TestFactsMatchPerEnvCadenceTables(t *testing.T) {
	// Serial by construction: t.Setenv forbids t.Parallel, which is what we
	// want — CRM_ENV is process-global and these cases read it.
	cases := []struct {
		env   string
		local map[string]time.Duration
	}{
		{"testing", testingPeriods},
		{"test", testingPeriods},
		{"accelerated", acceleratedPeriods},
		{"staging", productionPeriods},
		{"production", productionPeriods},
		{"", productionPeriods},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("CRM_ENV", tc.env)
			assert.Equal(t, configAsMap(cadence.GetCadenceConfig()), tc.local,
				"declare's table for CRM_ENV=%q drifted from cadence.GetCadenceConfig()", tc.env)
			assert.Equal(t, tc.local, activePeriods(), "activePeriods() selected the wrong table for CRM_ENV=%q", tc.env)
		})
	}
}

// The testing table is deliberately NOT ratio-proportional (monthly is 10min,
// not 30/7 × 2min ≈ 8.6min). A declare layer that scaled one ratio table by a
// day length would under-shoot every non-weekly backdate in the E2E env, so
// this asserts the non-proportionality is real and stated.
func TestTestingTableIsNotRatioProportional(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	proportionalMonthly := 30 * dayLength()
	require.NotEqual(t, proportionalMonthly, period("monthly"))
	assert.Equal(t, 10*time.Minute, period("monthly"))
	assert.Equal(t, 2*time.Minute/7, dayLength())
}

func TestDayLengthMatchesOverdueDayScaling(t *testing.T) {
	// The scaled-day arithmetic the overdue-days display uses, per env.
	cases := map[string]time.Duration{
		"testing":     2 * time.Minute / 7,
		"accelerated": 10 * time.Minute / 7,
		"production":  24 * time.Hour,
	}
	for env, want := range cases {
		t.Run(env, func(t *testing.T) {
			t.Setenv("CRM_ENV", env)
			assert.Equal(t, want, dayLength())
		})
	}
}

func TestCadenceVocabulary(t *testing.T) {
	assert.Equal(t, []string{"annual", "biannual", "biweekly", "monthly", "quarterly", "weekly"}, Cadences())
	assert.True(t, knownCadence("weekly"))
	assert.False(t, knownCadence("fortnightly"))
}
