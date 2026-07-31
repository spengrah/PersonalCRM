//go:build integration_testdb

package tests

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/synthetic"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyntheticProfile_DispatchesEveryValidProfile is FIRST-ORDER coverage of the
// operator seed path: `crm-admin --seed --profile <p>` reaches RunProfile, and
// RunProfile has to actually SEED THE PROFILE IT WAS ASKED FOR.
//
// It is not a test of what either world contains — the declared world's content
// is asserted by the E2E specs that cite each behavior. It is a test that the
// dispatch itself works, which nothing else covers: crm-admin's own tests inject
// a fake seed runner (so RunProfile is never invoked) and cover only the
// unknown-profile error branch, and the profile unit tests validate parameters
// rather than seeding. A broken dispatch would therefore surface for the first
// time in `make dev-seed` or a staging reset.
//
// Each profile is distinguished by something ONLY IT produces. "Contacts were
// seeded" is true of both, so a dispatch that routed `standard` to the
// minimal-scoped branch would satisfy it while this test claimed to cover every
// valid profile. The phase SHAPE is the discriminator — minimal-scoped brackets a
// single `seed-all` phase, the declared world brackets one phase per world step
// and ends on its tail — which identifies the branch actually taken without
// pinning a contact count that every new declaration would change.
//
// Table-driven so a newly added profile is a compile-time prompt to state its
// discriminator rather than a silently uncovered branch.
func TestSyntheticProfile_DispatchesEveryValidProfile(t *testing.T) {
	cases := []struct {
		profile synthetic.Profile
		// distinguish asserts a property the OTHER valid profile cannot satisfy.
		distinguish func(t *testing.T, res synthetic.ProfileResult)
	}{
		{
			profile: synthetic.ProfileMinimalScoped,
			distinguish: func(t *testing.T, res synthetic.ProfileResult) {
				// minimal-scoped is exactly SeedAll + DefaultParams: one settled
				// contact per source, bracketed as a single phase.
				require.Len(t, res.Timings.Phases, 1, "minimal-scoped brackets exactly one phase")
				assert.Equal(t, "seed-all", res.Timings.Phases[0].Name)
				assert.Equal(t, 2, res.Contacts, "one gmail-settled + one telegram-settled contact")
				assert.Equal(t, 1, res.GmailSettled)
				assert.Equal(t, 1, res.TelegramSettled)
			},
		},
		{
			profile: synthetic.ProfileStandard,
			distinguish: func(t *testing.T, res synthetic.ProfileResult) {
				// The declared world brackets one phase per world step, so its shape
				// is structurally unlike minimal-scoped's single bracket. Bounds
				// rather than exact counts: a new declaration must not break this.
				require.Greater(t, len(res.Timings.Phases), 1,
					"the declared world brackets one phase per world step")
				for _, phase := range res.Timings.Phases {
					assert.NotEqual(t, "seed-all", phase.Name,
						"a `seed-all` phase means this dispatched through the minimal-scoped branch")
				}
				last := res.Timings.Phases[len(res.Timings.Phases)-1].Name
				assert.True(t, strings.HasPrefix(last, "tail:"),
					"the declared world ends on its pinned-fixture tail, got %q", last)
				assert.Greater(t, res.Contacts, 50, "the declared world's own page-overflow floor")
			},
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.profile), func(t *testing.T) {
			database, ctx := newSyntheticDB(t)

			params, err := synthetic.ProfileParams(tc.profile)
			require.NoError(t, err)
			params.Namespace = syntheticNS(t)

			h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
			res, err := synthetic.RunProfile(ctx, h, params)
			require.NoError(t, err, "profile %q must dispatch and seed", tc.profile)

			assert.Equal(t, tc.profile, res.Profile, "the result must report the profile it dispatched")
			require.NotEmpty(t, res.Timings.Phases, "a dispatched profile must record its phases")
			tc.distinguish(t, res)

			// Seeded through the real service layer, so the rows are really there.
			remaining, err := h.ContactsRemaining(ctx)
			require.NoError(t, err)
			assert.EqualValues(t, res.Contacts, remaining,
				"the reported contact count must match what the namespace actually holds")
		})
	}
}
