//go:build integration_testdb

package tests

import (
	"testing"

	"personal-crm/backend/internal/synthetic"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyntheticProfile_DispatchesEveryValidProfile is FIRST-ORDER coverage of the
// operator seed path: `crm-admin --seed --profile <p>` reaches RunProfile, and
// RunProfile has to actually SEED for each valid profile.
//
// It is not a test of what either world contains — the declared world's content
// is asserted by the E2E specs that cite each behavior. It is a test that the
// dispatch itself works, which nothing else covers: crm-admin's own tests inject
// a fake seed runner (so RunProfile is never invoked) and cover only the
// unknown-profile error branch, and the profile unit tests validate parameters
// rather than seeding. A broken dispatch would therefore surface for the first
// time in `make dev-seed` or a staging reset.
//
// Table-driven over the valid profiles so a newly added one is a compile-time
// prompt to state its expectation rather than a silently uncovered branch.
func TestSyntheticProfile_DispatchesEveryValidProfile(t *testing.T) {
	for _, profile := range []synthetic.Profile{synthetic.ProfileMinimalScoped, synthetic.ProfileStandard} {
		t.Run(string(profile), func(t *testing.T) {
			database, ctx := newSyntheticDB(t)

			params, err := synthetic.ProfileParams(profile)
			require.NoError(t, err)
			params.Namespace = syntheticNS(t)

			h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
			res, err := synthetic.RunProfile(ctx, h, params)
			require.NoError(t, err, "profile %q must dispatch and seed", profile)

			assert.Equal(t, profile, res.Profile, "the result must report the profile it dispatched")
			assert.Positive(t, res.Contacts, "a dispatched profile must seed contacts")
			assert.NotEmpty(t, res.Timings.Phases, "a dispatched profile must record its phases")

			// Seeded through the real service layer, so the rows are really there.
			remaining, err := h.ContactsRemaining(ctx)
			require.NoError(t, err)
			assert.EqualValues(t, res.Contacts, remaining,
				"the reported contact count must match what the namespace actually holds")
		})
	}
}
