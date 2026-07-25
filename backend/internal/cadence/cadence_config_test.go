package cadence

import (
	"testing"
	"time"
)

// TestProductionCadenceConfigMatchesEnvironmentBranches proves the exported
// production table IS what GetCadenceConfig returns on every branch that runs
// production semantics — staging, production, prod, the empty env, and any
// unrecognized value (which defaults to production for safety).
//
// Without it the extraction could silently drift from the branch it replaced,
// and the drift would only show up as a wrong overdue classification in a caller
// that deliberately does not read the environment.
func TestProductionCadenceConfigMatchesEnvironmentBranches(t *testing.T) {
	want := ProductionCadenceConfig()

	for _, env := range []string{"staging", "production", "prod", "", "some-unrecognized-env"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("CRM_ENV", env)
			if got := GetCadenceConfig(); got != want {
				t.Fatalf("GetCadenceConfig() with CRM_ENV=%q = %+v, want the production table %+v", env, got, want)
			}
		})
	}
}

// TestProductionCadenceConfigCarriesRealWorldDurations pins the actual values.
// The accessor exists so a caller can assert an overdue expectation in real
// durations; a table that quietly became the compressed test one would make
// every such assertion vacuous while still "matching GetCadenceConfig".
func TestProductionCadenceConfigCarriesRealWorldDurations(t *testing.T) {
	got := ProductionCadenceConfig()
	want := CadenceConfig{
		Weekly:    7 * 24 * time.Hour,
		Biweekly:  14 * 24 * time.Hour,
		Monthly:   30 * 24 * time.Hour,
		Quarterly: 90 * 24 * time.Hour,
		Biannual:  180 * 24 * time.Hour,
		Annual:    365 * 24 * time.Hour,
	}
	if got != want {
		t.Fatalf("ProductionCadenceConfig() = %+v, want %+v", got, want)
	}
}

// TestGetCadenceConfigKeepsCompressedEnvironments guards the other direction:
// the extraction must not have collapsed the compressed branches onto the
// production table. A test/accelerated environment that started returning real
// durations would make the whole cadence suite run in real weeks.
func TestGetCadenceConfigKeepsCompressedEnvironments(t *testing.T) {
	production := ProductionCadenceConfig()
	for _, env := range []string{"test", "testing", "accelerated"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("CRM_ENV", env)
			if got := GetCadenceConfig(); got == production {
				t.Fatalf("CRM_ENV=%q must keep its compressed durations, got the production table %+v", env, got)
			}
		})
	}
}

// TestIsOverdueWithProductionConfigAgreesWithTheEnvHelper pins the production
// overdue helper against its environment-driven sibling: under a production-alias
// CRM_ENV the two must agree for EVERY cadence type, including an unrecognized
// one (which both resolve through the same Monthly fallback).
//
// The agreement is what makes it safe for a caller to reach for the production
// variant. A hand-copied formula in another package would pass a spot check and
// then drift on exactly this edge — an unrecognized cadence resolving to Monthly
// on one side and to "never overdue" on the other.
func TestIsOverdueWithProductionConfigAgreesWithTheEnvHelper(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	ref := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	types := []CadenceType{
		CadenceWeekly, CadenceBiweekly, CadenceMonthly,
		CadenceQuarterly, CadenceBiannual, CadenceAnnual,
		CadenceType("not-a-cadence"), CadenceType(""),
	}
	// Ages that straddle every period boundary in the table, plus both bases.
	ages := []time.Duration{
		0, 6 * 24 * time.Hour, 8 * 24 * time.Hour, 15 * 24 * time.Hour,
		31 * 24 * time.Hour, 91 * 24 * time.Hour, 181 * 24 * time.Hour, 366 * 24 * time.Hour,
	}

	for _, cadenceType := range types {
		for _, age := range ages {
			at := ref.Add(-age)
			if got, want := IsOverdueWithProductionConfig(cadenceType, nil, at, ref), IsOverdueWithConfig(cadenceType, nil, at, ref); got != want {
				t.Fatalf("cadence %q at created age %s: production helper = %t, env helper = %t", cadenceType, age, got, want)
			}
			if got, want := IsOverdueWithProductionConfig(cadenceType, &at, ref.Add(-400*24*time.Hour), ref), IsOverdueWithConfig(cadenceType, &at, ref.Add(-400*24*time.Hour), ref); got != want {
				t.Fatalf("cadence %q at last-contacted age %s: production helper = %t, env helper = %t", cadenceType, age, got, want)
			}
		}
	}
}

// TestIsOverdueWithProductionConfigIgnoresTheAmbientEnvironment is the other
// half: under the COMPRESSED test durations, where a two-day-old annual contact
// is long overdue, the production helper must still say it is not.
func TestIsOverdueWithProductionConfigIgnoresTheAmbientEnvironment(t *testing.T) {
	t.Setenv("CRM_ENV", "test")

	ref := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	createdAt := ref.Add(-2 * 24 * time.Hour)

	if !IsOverdueWithConfig(CadenceAnnual, nil, createdAt, ref) {
		t.Fatal("precondition: under CRM_ENV=test an annual cadence is two hours, so a two-day-old contact is overdue")
	}
	if IsOverdueWithProductionConfig(CadenceAnnual, nil, createdAt, ref) {
		t.Fatal("the production helper must read the production table, not the ambient environment")
	}
}
