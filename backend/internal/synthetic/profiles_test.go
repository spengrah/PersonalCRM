package synthetic

import (
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
)

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

// TestProfileParams asserts each named profile returns its expected Profile +
// non-zero contact count, and an unknown name (including "") errors.
func TestProfileParams(t *testing.T) {
	for _, name := range []Profile{ProfileMinimalScoped, ProfileDev, ProfileProdShaped} {
		t.Run(string(name), func(t *testing.T) {
			p, err := ProfileParams(name)
			require.NoError(t, err)
			require.Equal(t, name, p.Profile)
			require.Greater(t, p.Counts.SeededContacts, 0, "profile %s should seed at least one contact", name)
			require.NotEmpty(t, p.Namespace)
			require.Equal(t, factory.DefaultSeed, p.Seed)
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

	// prod-shaped must carry the producible pending-state knobs so the coverage
	// check has something to assert against.
	prod, err := ProfileParams(ProfileProdShaped)
	require.NoError(t, err)
	require.Greater(t, prod.Counts.UnmatchedExternal, 0)
	require.Greater(t, prod.Counts.StrandedTelegram, 0)
	require.Greater(t, prod.Counts.StrandedMessages, 0)
	require.Greater(t, prod.Counts.UnmatchedCalendar, 0)
	require.Greater(t, prod.Counts.OrphanMeetingNote, 0)
}

// TestDefaultParamsUnchanged pins DefaultParams field-for-field so the
// golden-scenario regression (Element 2) keeps passing: minimal-scoped ==
// today's SeedAll + DefaultParams, byte-stable. The Profile field is set
// EXPLICITLY (the zero value "" is an error profile, not minimal-scoped).
func TestDefaultParamsUnchanged(t *testing.T) {
	p := DefaultParams()
	require.Equal(t, "seedall", p.Namespace)
	require.Equal(t, factory.DefaultSeed, p.Seed)
	require.Equal(t, ProfileMinimalScoped, p.Profile)
	require.Equal(t, 1, p.Counts.SeededContacts)
	require.Equal(t, 0, p.Counts.UnmatchedExternal)
	require.Equal(t, 0, p.Counts.StrandedTelegram)
	require.Equal(t, 0, p.Counts.StrandedMessages)
	require.Equal(t, 0, p.Counts.UnmatchedCalendar)
	require.Equal(t, 0, p.Counts.OrphanMeetingNote)
}
