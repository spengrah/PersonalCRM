package factory

import (
	"testing"
	"time"
)

// lastContactedAnchor is a fixed anchor so the backdated instants are predictable.
var lastContactedAnchor = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

// The backdated cohorts mirror the create handler, which no longer seeds
// last_contacted (CON-001): the column records a two-way connection (CAD-006), and
// a generated contact with an empty timeline has had none. Stamping it here was how
// the seed manufactured a production-impossible world once before (#641) — a
// last_contacted with no interaction behind it. That is now a COMPILE-TIME guarantee:
// ContactSpec carries no LastContacted field, so no cohort can set it; this test pins
// the surviving behavior, that both cohorts backdate created_at.
func TestBackdatedCohorts_BackdateCreatedAt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opt  ContactOption
	}{
		{"WithCreatedAge", WithCreatedAge(90 * 24 * time.Hour)},
		{"WithRecentCreation", WithRecentCreation(48 * time.Hour)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := NewGeneratorAt(DefaultSeed, "lastcontacted", lastContactedAnchor)
			spec := g.Contact(WithEmail(), WithCadence("weekly"), tc.opt)

			if spec.CreatedAt == nil {
				t.Fatalf("%s should backdate created_at, got nil", tc.name)
			}
		})
	}
}
