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
