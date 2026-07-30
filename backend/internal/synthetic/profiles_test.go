package synthetic

import (
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
)

// validCadences is the migration-005 CHECK constraint's cadence allowlist; every
// cadence a fixture or a declaration names may only be drawn from it.
var validCadences = map[string]bool{
	"weekly": true, "biweekly": true, "monthly": true,
	"quarterly": true, "biannual": true, "annual": true,
}

// TestSeedAllowed asserts the prod gate denies the two production aliases and
// allows every other valid CRM_ENV (the validCRMEnvs set in config.go).
func TestSeedAllowed(t *testing.T) {
	cases := []struct {
		env     string
		allowed bool
	}{
		{"production", false},
		{"prod", false},
		{"staging", true},
		{"accelerated", true},
		{"test", true},
		{"testing", true},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Runtime.CRMEnvironment = tc.env
			err := SeedAllowed(cfg)
			if tc.allowed {
				require.NoError(t, err, "CRM_ENV=%s should be allowed", tc.env)
			} else {
				require.Error(t, err, "CRM_ENV=%s should be denied", tc.env)
			}
		})
	}
}

// TestProfileParams asserts the TWO surviving profiles resolve, and that every
// other name errors — including the two retired catalog profile literals, which
// are named explicitly. An operator (or an un-updated script) passing `dev` or
// `prod-shaped` must get a loud refusal rather than a silently different world.
func TestProfileParams(t *testing.T) {
	t.Run(string(ProfileMinimalScoped), func(t *testing.T) {
		p, err := ProfileParams(ProfileMinimalScoped)
		require.NoError(t, err)
		require.Equal(t, ProfileMinimalScoped, p.Profile)
		require.Equal(t, "seedall", p.Namespace)
		require.Equal(t, factory.DefaultSeed, p.Seed)
		require.Greater(t, p.Counts.SeededContacts, 0, "minimal-scoped must seed at least one contact")
	})

	t.Run(string(ProfileStandard), func(t *testing.T) {
		p, err := ProfileParams(ProfileStandard)
		require.NoError(t, err)
		require.Equal(t, ProfileStandard, p.Profile)
		require.Equal(t, "standard", p.Namespace)
		require.Equal(t, factory.DefaultSeed, p.Seed)
		require.Equal(t, Counts{}, p.Counts,
			"standard is registry-defined and intentionally has no volume knobs")
	})

	for _, retired := range []Profile{"dev", "prod-shaped"} {
		t.Run("retired/"+string(retired), func(t *testing.T) {
			_, err := ProfileParams(retired)
			require.Error(t, err, "the retired catalog profile %q must not resolve", retired)
			require.Contains(t, err.Error(), "unknown profile")
		})
	}

	t.Run("unknown", func(t *testing.T) {
		_, err := ProfileParams("nope")
		require.Error(t, err)
	})
	t.Run("empty", func(t *testing.T) {
		_, err := ProfileParams("")
		require.Error(t, err)
	})
}

// TestDefaultParamsUnchanged pins DefaultParams field-for-field so the
// golden-scenario regression keeps passing: minimal-scoped == today's SeedAll +
// DefaultParams, byte-stable. The Profile field is set EXPLICITLY (the zero value
// "" is an error profile, not minimal-scoped).
func TestDefaultParamsUnchanged(t *testing.T) {
	p := DefaultParams()
	require.Equal(t, "seedall", p.Namespace)
	require.Equal(t, factory.DefaultSeed, p.Seed)
	require.Equal(t, ProfileMinimalScoped, p.Profile)
	require.Equal(t, Counts{SeededContacts: 1}, p.Counts)
}
