//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/require"
)

// TestSyncLogTrim_Isolated covers external_sync_log retention end to end at the
// repository layer. It stays SERIAL and on a per-test clone for two reasons:
// the trim DELETE is table-wide, so an exact deleted-count assertion needs a
// database no sibling writes to; and it freezes the process-global app clock
// via accelerated.SetNowForTest, which a parallel sibling could observe.
func TestSyncLogTrim_Isolated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	repo := repository.NewSyncRepository(database.Queries)

	// Freeze the app clock 100 days AHEAD of wall time. This is the time-
	// acceleration shape: a log row stamped by SQL NOW() would sit 100 days
	// behind the cutoff computed from the app clock and be deleted the moment
	// it was written.
	frozen := accelerated.GetCurrentTime().AddDate(0, 0, 100).Truncate(time.Microsecond)
	restore := accelerated.SetNowForTest(func() time.Time { return frozen })
	t.Cleanup(restore)

	state, err := repo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:   "log_trim_isolated",
		Strategy: repository.SyncStrategyContactDriven,
	})
	require.NoError(t, err)

	newLog := func() *repository.SyncLog {
		l, err := repo.CreateSyncLog(ctx, state)
		require.NoError(t, err)
		return l
	}

	// 1. Insert stamps started_at from the app clock, not the DB clock.
	fresh := newLog()
	require.True(t, fresh.StartedAt.Equal(frozen), "started_at %v, want app clock %v", fresh.StartedAt, frozen)

	// 2. Completion stamps completed_at from the same clock, so durations
	//    computed from the row are on one clock.
	completed, err := repo.CompleteSyncLog(ctx, fresh.ID, repository.CompleteSyncLogResult{Status: "success"})
	require.NoError(t, err)
	require.NotNil(t, completed.CompletedAt)
	require.True(t, completed.CompletedAt.Equal(frozen), "completed_at %v, want app clock %v", *completed.CompletedAt, frozen)

	// 3. Retention boundaries around cutoff = app-now - 30d. started_at is
	//    backdated through the test-only setter; created_at stays at insert
	//    time so a query trimming the wrong column would delete nothing.
	cutoff := frozen.AddDate(0, 0, -30)
	dayBefore := cutoff.AddDate(0, 0, -1)
	dayAfter := cutoff.AddDate(0, 0, 1)

	oldRunning := newLog()
	require.NoError(t, repo.SetSyncLogStartedAtForTest(ctx, oldRunning.ID, dayBefore))

	oldSuccess := newLog()
	_, err = repo.CompleteSyncLog(ctx, oldSuccess.ID, repository.CompleteSyncLogResult{Status: "success"})
	require.NoError(t, err)
	require.NoError(t, repo.SetSyncLogStartedAtForTest(ctx, oldSuccess.ID, dayBefore))

	oldError := newLog()
	msg := "boom"
	_, err = repo.CompleteSyncLog(ctx, oldError.ID, repository.CompleteSyncLogResult{Status: "error", ErrorMessage: &msg})
	require.NoError(t, err)
	require.NoError(t, repo.SetSyncLogStartedAtForTest(ctx, oldError.ID, dayBefore))

	atCutoff := newLog()
	require.NoError(t, repo.SetSyncLogStartedAtForTest(ctx, atCutoff.ID, cutoff))

	recent := newLog()
	require.NoError(t, repo.SetSyncLogStartedAtForTest(ctx, recent.ID, dayAfter))

	deleted, err := repo.DeleteOldSyncLogs(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(3), deleted, "exactly the three rows strictly before the cutoff go, regardless of status")

	logs, err := repo.ListSyncLogsByState(ctx, state.ID, 10, 0)
	require.NoError(t, err)
	survivors := map[string]bool{}
	for _, l := range logs {
		survivors[l.ID.String()] = true
	}
	require.Equal(t, map[string]bool{
		fresh.ID.String():    true, // app-clock-stamped row survives under acceleration
		atCutoff.ID.String(): true, // boundary is strict: started_at < cutoff
		recent.ID.String():   true,
	}, survivors)

	// 4. A second sweep at the same cutoff finds nothing.
	deleted, err = repo.DeleteOldSyncLogs(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(0), deleted)

	// 5. Error path: the repository wraps the query error with its own context.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.DeleteOldSyncLogs(cancelled, cutoff)
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete old sync logs: ")
}
