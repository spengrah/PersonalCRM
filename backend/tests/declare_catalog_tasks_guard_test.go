//go:build integration_testdb

package tests

import (
	"context"
	"strings"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyntheticDeclareCatalogTasksGuard pins the zero direction of the catalog
// profile's `Counts.SeededTasks > 0` gate on the awaiting-reply fixture.
//
// The gate lives at the CALL SITE, not inside the recipe. That distinction is
// what this test exists to protect: the follow-up recipe is now a shared helper
// (so the declared world can seed the same fixture), and absorbing the gate into
// that helper would be invisible to the existing DEFAULT-profile coverage
// (TestSyntheticProfile_DevCoversCatalog already proves the positive direction:
// exactly one live pending fixture) while silently changing what a zero-tasks
// catalog run produces.
//
// It is deleted along with the profiles it guards.
func TestSyntheticDeclareCatalogTasksGuard(t *testing.T) {
	testsupport.RequireLongTests(t)
	t.Parallel()

	res, names := runGuardedCatalog(t, 0)
	assert.Equal(t, 0, res.SeededPendingFollowUps, "no follow-up may be seeded when tasks are off")
	assert.Equal(t, 0, res.SeededTasks)
	assert.False(t, containsMarker(names, synthetic.FixtureMarkerPending),
		"the awaiting-reply fixture rides the task gate — with tasks off it must not exist")
}

// runGuardedCatalog seeds a MINIMAL dev catalog with the task count under test
// and returns the profile accounting plus every contact name in the namespace.
// The volume is bounded hard: the guard is a property of the call site, not of
// the scale.
func runGuardedCatalog(t *testing.T, seededTasks int) (synthetic.ProfileResult, []string) {
	t.Helper()

	database, ctx := declareTestDB(t)
	params, err := synthetic.ProfileParams(synthetic.ProfileDev)
	require.NoError(t, err)
	params.Namespace = declareNS(t)
	params.Counts = synthetic.Counts{
		SeededContacts:     3,
		MessagesPerContact: 1,
		SeededTasks:        seededTasks,
	}

	h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(
		ctx, database, params.Namespace, params.Seed, accelerated.GetCurrentTime())
	require.NoError(t, err)

	res, err := synthetic.RunProfile(ctx, h, params)
	if err != nil {
		_ = teardown(context.Background())
		require.NoError(t, err, "seed the guarded catalog")
	}
	require.NoError(t, h.Quiesce(ctx))

	support := repository.NewSyntheticSupportRepository(database.Queries)
	ids, err := support.SelectContactIDsByFullNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		c, err := contactRepo.GetContact(ctx, id)
		require.NoError(t, err)
		names = append(names, c.FullName)
	}
	return res, names
}

func containsMarker(names []string, marker string) bool {
	for _, name := range names {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
