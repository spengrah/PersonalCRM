//go:build integration_testdb

package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSample inserts one job_exec_sample row. created_at defaults to
// attempted_at (decision queries don't filter on created_at); trim tests pass a
// distinct created_at explicitly via seedSampleAt.
func seedSample(t *testing.T, ctx context.Context, repo *repository.JobSampleRepository, id int64, kind string, attempted, finalized time.Time, waitMs int64, state string) {
	t.Helper()
	seedSampleAt(t, ctx, repo, id, kind, attempted, finalized, waitMs, state, attempted)
}

func seedSampleAt(t *testing.T, ctx context.Context, repo *repository.JobSampleRepository, id int64, kind string, attempted, finalized time.Time, waitMs int64, state string, createdAt time.Time) {
	t.Helper()
	require.NoError(t, repo.InsertJobExecSample(ctx, repository.JobExecSampleRow{
		RiverJobID:  id,
		Kind:        kind,
		Queue:       "default",
		AttemptedAt: attempted,
		FinalizedAt: finalized,
		Attempt:     1,
		State:       state,
		QueueWaitMs: waitMs,
		CreatedAt:   createdAt,
	}))
}

// TestJobSample_MaxConcurrencyAndSaturatedSeconds exercises the metric-1
// sweep-line queries on an isolated clone (deterministic across repeated runs;
// the kind-agnostic sweep would otherwise accumulate rows on the shared DB).
// Each case still uses a distinct era (year) + kind so its window sees only its
// own rows.
func TestJobSample_MaxConcurrencyAndSaturatedSeconds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	t.Run("peak_at_threshold", func(t *testing.T) {
		base := time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC)
		from, to := base.Add(-time.Hour), base.Add(time.Hour)
		k := "jsc-" + uuid.NewString()[:8]
		// Three intervals overlapping to peak at concurrency 3 during [t4,t8].
		seedSample(t, ctx, repo, 74_010_001, k, base.Add(0*time.Second), base.Add(10*time.Second), 0, "completed")
		seedSample(t, ctx, repo, 74_010_002, k, base.Add(2*time.Second), base.Add(12*time.Second), 0, "completed")
		seedSample(t, ctx, repo, 74_010_003, k, base.Add(4*time.Second), base.Add(8*time.Second), 0, "completed")

		maxC, err := repo.MaxConcurrency(ctx, from, to)
		require.NoError(t, err)
		assert.Equal(t, 3, maxC)

		sat, err := repo.SaturatedSeconds(ctx, from, to, 3)
		require.NoError(t, err)
		assert.InDelta(t, 4.0, sat, 0.001, "concurrency>=3 held for [t4,t8] = 4s")
	})

	t.Run("non_overlapping", func(t *testing.T) {
		base := time.Date(2105, 1, 1, 0, 0, 0, 0, time.UTC)
		from, to := base.Add(-time.Hour), base.Add(time.Hour)
		k := "jsc-" + uuid.NewString()[:8]
		seedSample(t, ctx, repo, 74_050_001, k, base.Add(0*time.Second), base.Add(5*time.Second), 0, "completed")
		seedSample(t, ctx, repo, 74_050_002, k, base.Add(100*time.Second), base.Add(105*time.Second), 0, "completed")

		maxC, err := repo.MaxConcurrency(ctx, from, to)
		require.NoError(t, err)
		assert.Equal(t, 1, maxC)

		sat, err := repo.SaturatedSeconds(ctx, from, to, 2)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, sat, 0.001)
	})

	t.Run("tied_endpoints_no_false_peak", func(t *testing.T) {
		base := time.Date(2106, 1, 1, 0, 0, 0, 0, time.UTC)
		from, to := base.Add(-time.Hour), base.Add(time.Hour)
		k := "jsc-" + uuid.NewString()[:8]
		// One interval ends exactly where the next begins → netting must cancel
		// the release+acquire at t=base+5s so there is NO false peak of 2.
		seedSample(t, ctx, repo, 74_060_001, k, base.Add(0*time.Second), base.Add(5*time.Second), 0, "completed")
		seedSample(t, ctx, repo, 74_060_002, k, base.Add(5*time.Second), base.Add(10*time.Second), 0, "completed")

		maxC, err := repo.MaxConcurrency(ctx, from, to)
		require.NoError(t, err)
		assert.Equal(t, 1, maxC, "tied endpoints must net to zero (no false peak)")
	})

	t.Run("interval_spanning_window_counts", func(t *testing.T) {
		base := time.Date(2107, 1, 1, 0, 0, 0, 0, time.UTC)
		from, to := base, base.Add(20*time.Second)
		k := "jsc-" + uuid.NewString()[:8]
		// A job that starts before `from` and ends after `to` — no endpoint
		// inside the window. Clipping must still count it as concurrency 1.
		seedSample(t, ctx, repo, 74_070_001, k, base.Add(-100*time.Second), base.Add(200*time.Second), 0, "completed")
		// A second job fully inside → peak becomes 2 for its span.
		seedSample(t, ctx, repo, 74_070_002, k, base.Add(5*time.Second), base.Add(10*time.Second), 0, "completed")

		maxC, err := repo.MaxConcurrency(ctx, from, to)
		require.NoError(t, err)
		assert.Equal(t, 2, maxC, "spanning interval must contribute across the window")

		sat, err := repo.SaturatedSeconds(ctx, from, to, 2)
		require.NoError(t, err)
		assert.InDelta(t, 5.0, sat, 0.001, "concurrency>=2 held for [t5,t10] = 5s")
	})
}

func TestJobSample_RunDurationByKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	base := time.Date(2102, 1, 1, 0, 0, 0, 0, time.UTC)
	from, to := base.Add(-time.Hour), base.Add(time.Hour)
	kx := "jsr-x-" + uuid.NewString()[:8]
	ky := "jsr-y-" + uuid.NewString()[:8]

	// kx: runs of 2s and 4s → p50=3, max=4.
	seedSample(t, ctx, repo, 74_020_001, kx, base.Add(0*time.Second), base.Add(2*time.Second), 0, "completed")
	seedSample(t, ctx, repo, 74_020_002, kx, base.Add(10*time.Second), base.Add(14*time.Second), 0, "completed")
	// ky: single 10s run.
	seedSample(t, ctx, repo, 74_020_003, ky, base.Add(0*time.Second), base.Add(10*time.Second), 0, "completed")

	rows, err := repo.RunDurationByKind(ctx, from, to)
	require.NoError(t, err)

	byKind := map[string]repository.KindDurationStats{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	require.Contains(t, byKind, kx)
	require.Contains(t, byKind, ky)
	assert.Equal(t, int64(2), byKind[kx].N)
	assert.InDelta(t, 3.0, byKind[kx].P50, 0.001)
	assert.InDelta(t, 4.0, byKind[kx].Max, 0.001)
	assert.Equal(t, int64(1), byKind[ky].N)
	assert.InDelta(t, 10.0, byKind[ky].P50, 0.001)
}

func TestJobSample_WaitByKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	base := time.Date(2103, 1, 1, 0, 0, 0, 0, time.UTC)
	from, to := base.Add(-time.Hour), base.Add(time.Hour)
	kx := "jsw-x-" + uuid.NewString()[:8]
	ky := "jsw-y-" + uuid.NewString()[:8]

	// Percentiles must come from the STORED queue_wait_ms, not any timestamp
	// subtraction (attempted_at - finalized_at here would be nonsense).
	seedSample(t, ctx, repo, 74_030_001, kx, base.Add(0*time.Second), base.Add(1*time.Second), 1000, "completed")
	seedSample(t, ctx, repo, 74_030_002, kx, base.Add(5*time.Second), base.Add(6*time.Second), 3000, "completed")
	seedSample(t, ctx, repo, 74_030_003, ky, base.Add(0*time.Second), base.Add(1*time.Second), 500, "completed")

	rows, err := repo.WaitByKind(ctx, from, to)
	require.NoError(t, err)
	byKind := map[string]repository.KindDurationStats{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	require.Contains(t, byKind, kx)
	assert.InDelta(t, 2.0, byKind[kx].P50, 0.001, "p50 of {1s,3s} = 2s from stored wait")
	assert.InDelta(t, 3.0, byKind[kx].Max, 0.001)
	assert.InDelta(t, 0.5, byKind[ky].P50, 0.001)
}

// TestJobSample_WaitDuringSaturationAndBlame exercises the two decisive
// metric-2 queries: the saturation gate and the slot-blame attribution,
// including the misattribution guard (a disjoint saturated period held by a
// third kind must not be blamed for a consumer kind's wait).
func TestJobSample_WaitDuringSaturationAndBlame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	base := time.Date(2104, 1, 1, 0, 0, 0, 0, time.UTC)
	from, to := base.Add(-100*time.Second), base.Add(3000*time.Second)
	kA := "jss-a-" + uuid.NewString()[:8] // consumer waiter
	kB := "jss-b-" + uuid.NewString()[:8] // external holder (saturation 1)
	kC := "jss-c-" + uuid.NewString()[:8] // external holder (saturation 2, disjoint)
	const threshold = 2

	s0 := base
	// Saturation period 1: two B jobs concurrent for 20s → c=2 during [s0,s0+20].
	seedSample(t, ctx, repo, 74_040_001, kB, s0, s0.Add(20*time.Second), 0, "completed")
	seedSample(t, ctx, repo, 74_040_002, kB, s0, s0.Add(20*time.Second), 0, "completed")
	// A waiter whose wait window [eligible=s0, attempted=s0+10] sits inside the
	// saturated span; it then runs [s0+10, s0+15].
	seedSample(t, ctx, repo, 74_040_003, kA, s0.Add(10*time.Second), s0.Add(15*time.Second), 10_000, "completed")
	// A second A waiter far outside any saturation (only itself running).
	u0 := s0.Add(2000 * time.Second)
	seedSample(t, ctx, repo, 74_040_004, kA, u0, u0.Add(3*time.Second), 5_000, "completed")

	// Saturation period 2 (disjoint): two C jobs concurrent, NO A waiter here.
	v0 := s0.Add(1000 * time.Second)
	seedSample(t, ctx, repo, 74_040_005, kC, v0, v0.Add(20*time.Second), 0, "completed")
	seedSample(t, ctx, repo, 74_040_006, kC, v0, v0.Add(20*time.Second), 0, "completed")

	// --- Gate query: A's saturated wait ≈ 10s, over a 15s in-window wait. ---
	satRows, err := repo.WaitDuringSaturationByKind(ctx, from, to, threshold)
	require.NoError(t, err)
	var aRow *repository.WaitSaturationRow
	for i := range satRows {
		if satRows[i].Kind == kA {
			aRow = &satRows[i]
		}
	}
	require.NotNil(t, aRow, "kind A must appear as a waiter")
	assert.Equal(t, int64(2), aRow.NWaiters)
	assert.InDelta(t, 15.0, aRow.WaitedInWindowS, 0.001, "10s + 5s in-window wait")
	assert.InDelta(t, 10.0, aRow.SaturatedWaitS, 0.05, "only waiter1's 10s overlapped saturation")

	// --- Blame query: (A,B) ≈ 20 slot-seconds; NO (A,C) row. ---
	blame, err := repo.WaitSlotBlameByKind(ctx, from, to, threshold)
	require.NoError(t, err)
	var abBlame float64
	sawAC := false
	for _, b := range blame {
		if b.WaitKind == kA && b.RunningKind == kB {
			abBlame = b.BlameSlotS
		}
		if b.WaitKind == kA && b.RunningKind == kC {
			sawAC = true
		}
	}
	assert.InDelta(t, 20.0, abBlame, 0.1, "two B jobs each held a slot for A's 10s wait window")
	assert.False(t, sawAC, "disjoint C-saturated period must not be blamed for A's wait")

	// --- Below-threshold: nothing saturated → zero saturated wait, no blame. ---
	satHigh, err := repo.WaitDuringSaturationByKind(ctx, from, to, 10)
	require.NoError(t, err)
	for _, r := range satHigh {
		if r.Kind == kA {
			assert.InDelta(t, 0.0, r.SaturatedWaitS, 0.001, "no segment reaches concurrency 10")
		}
	}
	blameHigh, err := repo.WaitSlotBlameByKind(ctx, from, to, 10)
	require.NoError(t, err)
	for _, b := range blameHigh {
		assert.NotEqual(t, kA, b.WaitKind, "no blame rows when nothing is saturated")
	}
}

// TestJobSample_Trim runs the global trim DELETE on an isolated clone (on the
// shared DB it would delete sibling tests' rows).
func TestJobSample_Trim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	const retentionDays = 14
	now := accelerated.GetCurrentTime()
	cutoff := now.AddDate(0, 0, -retentionDays)

	k := "jst-" + uuid.NewString()[:8]
	attempted := now.Add(-time.Hour)
	// Old rows (created_at before cutoff) — should be deleted.
	seedSampleAt(t, ctx, repo, 74_080_001, k, attempted, attempted.Add(time.Second), 0, "completed", now.AddDate(0, 0, -20))
	seedSampleAt(t, ctx, repo, 74_080_002, k, attempted, attempted.Add(time.Second), 0, "completed", now.AddDate(0, 0, -30))
	// Recent rows (created_at after cutoff) — should survive.
	seedSampleAt(t, ctx, repo, 74_080_003, k, attempted, attempted.Add(time.Second), 0, "completed", now.AddDate(0, 0, -2))
	seedSampleAt(t, ctx, repo, 74_080_004, k, attempted, attempted.Add(time.Second), 0, "completed", now.AddDate(0, 0, -1))

	deleted, err := repo.TrimJobExecSamples(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	survivors, err := repo.ListJobExecSamplesByRiverJobIDForTest(ctx,
		[]int64{74_080_001, 74_080_002, 74_080_003, 74_080_004})
	require.NoError(t, err)
	surviveIDs := map[int64]bool{}
	for _, s := range survivors {
		surviveIDs[s.RiverJobID] = true
	}
	assert.False(t, surviveIDs[74_080_001], "old row must be trimmed")
	assert.False(t, surviveIDs[74_080_002], "old row must be trimmed")
	assert.True(t, surviveIDs[74_080_003], "recent row must survive")
	assert.True(t, surviveIDs[74_080_004], "recent row must survive")
}

// TestJobSample_Tier0StatsByKind seeds finished river_job rows on an isolated
// clone (so a sibling test's live client can't claim them) and asserts the
// Tier-0 per-kind wait/run percentiles.
func TestJobSample_Tier0StatsByKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	repo := repository.NewJobSampleRepository(database.Queries)

	now := accelerated.GetCurrentTime()
	cutoff := now.Add(-2 * time.Hour)
	base := now.Add(-30 * time.Minute)
	kx := "jt0-x-" + uuid.NewString()[:8]
	ky := "jt0-y-" + uuid.NewString()[:8]
	kz := "jt0-z-" + uuid.NewString()[:8]

	// kx: wait 2s (attempted - scheduled), run 5s (finalized - attempted).
	require.NoError(t, repo.InsertRiverJobFullTimingForTest(ctx, kx, db.RiverJobStateCompleted,
		base, base.Add(2*time.Second), base.Add(7*time.Second)))
	// ky: wait 1s, run 3s.
	require.NoError(t, repo.InsertRiverJobFullTimingForTest(ctx, ky, db.RiverJobStateCompleted,
		base, base.Add(1*time.Second), base.Add(4*time.Second)))
	// kz: FINALIZED but NEVER attempted (cancelled before it ran) — attempted_at
	// is NULL. The Tier-0 WHERE must exclude it; without the attempted_at guard
	// its NULL percentiles would abort the whole scan (P1 regression guard).
	health := repository.NewHealthRepository(database.Queries)
	require.NoError(t, health.InsertRiverJobForTest(ctx, kz, db.RiverJobStateCancelled,
		base, ptrTime(base.Add(1*time.Second))))

	rows, err := repo.Tier0StatsByKind(ctx, cutoff)
	require.NoError(t, err, "read must succeed despite a finalized-but-never-attempted row")
	byKind := map[string]repository.Tier0Row{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	require.Contains(t, byKind, kx)
	require.Contains(t, byKind, ky)
	assert.NotContains(t, byKind, kz, "never-attempted kind must be excluded, not crash the read")
	assert.Equal(t, int64(1), byKind[kx].N)
	assert.InDelta(t, 2.0, byKind[kx].P50WaitS, 0.001)
	assert.InDelta(t, 5.0, byKind[kx].P50RunS, 0.001)
	assert.InDelta(t, 1.0, byKind[ky].P50WaitS, 0.001)
	assert.InDelta(t, 3.0, byKind[ky].P50RunS, 0.001)
}
