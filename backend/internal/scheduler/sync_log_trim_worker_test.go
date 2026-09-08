package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type stubSyncLogTrimmer struct {
	cutoff  time.Time
	deleted int64
	err     error
	calls   int
}

func (s *stubSyncLogTrimmer) DeleteOldSyncLogs(_ context.Context, cutoff time.Time) (int64, error) {
	s.calls++
	s.cutoff = cutoff
	return s.deleted, s.err
}

// TestSyncLogTrimWorker_CutoffIsRetentionDaysBeforeAppClock freezes the app
// clock and uses a non-default window, so the cutoff is pinned exactly: a
// worker reading wall time instead of the app clock, hardcoding 30, or getting
// the sign wrong all miss the frozen value.
func TestSyncLogTrimWorker_CutoffIsRetentionDaysBeforeAppClock(t *testing.T) {
	frozen := time.Date(2030, time.March, 15, 12, 0, 0, 0, time.UTC)
	restore := accelerated.SetNowForTest(func() time.Time { return frozen })
	t.Cleanup(restore)

	stub := &stubSyncLogTrimmer{deleted: 3}
	worker := NewSyncLogTrimWorker(stub, 7)

	err := worker.Work(context.Background(), &river.Job[SyncLogTrimArgs]{})
	require.NoError(t, err)

	require.Equal(t, 1, stub.calls)
	require.True(t, stub.cutoff.Equal(frozen.AddDate(0, 0, -7)), "cutoff %v, want %v", stub.cutoff, frozen.AddDate(0, 0, -7))
}

func TestSyncLogTrimWorker_WrapsRepositoryError(t *testing.T) {
	boom := errors.New("boom")
	worker := NewSyncLogTrimWorker(&stubSyncLogTrimmer{err: boom}, 30)

	err := worker.Work(context.Background(), &river.Job[SyncLogTrimArgs]{})
	require.EqualError(t, err, "trim sync logs: boom")
	require.ErrorIs(t, err, boom)
}

// TestSyncLogTrimWorker_Timeout pins the generous first-sweep budget; see the
// Timeout doc comment for why it exceeds the sibling workers' 30s.
func TestSyncLogTrimWorker_Timeout(t *testing.T) {
	worker := NewSyncLogTrimWorker(&stubSyncLogTrimmer{}, 30)
	require.Equal(t, 2*time.Minute, worker.Timeout(nil))
}
