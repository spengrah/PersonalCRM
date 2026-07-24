//go:build integration_testdb

// Proves the seed FAILURE lifecycle end-to-end at the adapter level: a run that
// errors returns its PARTIAL ProfileResult alongside the error instead of
// discarding it. The unit tests above cover the rendering and the entrypoints'
// printing, but both drive a fake seedRunner — only this test proves the real
// seedAdapter carries the partial result out, which is what makes the whole
// diagnostic path real rather than decorative.
//
// The harness starts a live River client, so this owns an ISOLATED per-test
// clone rather than the shared package DB.
package main

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"

	"github.com/stretchr/testify/require"
)

func TestSeedAdapterRunProfileCarriesPartialResultOnFailure(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	database, _ := newJobsTestDB(t, ctx)

	adapter := seedAdapter{
		database: database,
		support:  repository.NewSyntheticSupportRepository(database.Queries),
	}
	// An unknown profile fails inside RunProfile — after the harness is built, so
	// the run is real and the result is genuinely partial — without needing a
	// mid-seed injection seam in the profile itself.
	params := synthetic.SeedParams{
		Namespace: "partialfail",
		Seed:      1,
		Profile:   synthetic.Profile("no-such-profile"),
	}

	res, err := adapter.runProfile(ctx, params)

	require.Error(t, err, "the profile failure is still surfaced")
	// A genuine profile failure must stay in the PARTIAL branch: the world was
	// torn down, so labelling it world-intact would tell an operator to trust
	// counts for rows that no longer exist.
	require.NotErrorIs(t, err, errSeedWorldIntact)
	// A discarded result would be the zero ProfileResult; these fields are the
	// discriminator that the partial one came back.
	require.Equal(t, params.Profile, res.Profile)
	require.Equal(t, params.Namespace, res.Namespace)
	require.Equal(t, params.Seed, res.Seed)
	require.NotZero(t, res.Timings.Total, "a failed run still reports how long it took")
}
