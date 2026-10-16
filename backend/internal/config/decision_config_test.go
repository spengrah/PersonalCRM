package config

import (
	"os"
	"testing"
)

// Loading the configuration designates the production decision method only
// when DECISION_PRODUCTION_METHOD names one.
// spec: KNW-045.designation-from-setting
func TestConfig_DecisionProductionMethod(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  string
	}{
		{name: "unset designates none"},
		{name: "empty designates none", set: true},
		{name: "a method key designates it", set: true, value: "clef-masked", want: "clef-masked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
			WithEnv(t, "DECISION_PRODUCTION_METHOD", tc.value)
			if !tc.set {
				if err := os.Unsetenv("DECISION_PRODUCTION_METHOD"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Decision.ProductionMethod != tc.want {
				t.Errorf("production method = %q, want %q", cfg.Decision.ProductionMethod, tc.want)
			}
		})
	}
}
