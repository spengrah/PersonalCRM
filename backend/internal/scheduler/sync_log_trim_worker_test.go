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

// TestSyncLogTrimWorker_CutoffIsRetentionDaysBeforeNow pins the cutoff to
// accelerated-now minus the configured window: a worker that used SQL NOW() or
// the wrong sign would drift or delete everything.
func TestSyncLogTrimWorker_CutoffIsRetentionDaysBeforeNow(t *testing.T) {
	stub := &stubSyncLogTrimmer{deleted: 3}
	worker := NewSyncLogTrimWorker(stub, 30)

	before := accelerated.GetCurrentTime()
	err := worker.Work(context.Background(), &river.Job[SyncLogTrimArgs]{})
	after := accelerated.GetCurrentTime()
	require.NoError(t, err)

	require.Equal(t, 1, stub.calls)
	require.False(t, stub.cutoff.Before(before.AddDate(0, 0, -30)), "cutoff earlier than now-30d")
	require.False(t, stub.cutoff.After(after.AddDate(0, 0, -30)), "cutoff later than now-30d")
}

func TestSyncLogTrimWorker_WrapsRepositoryError(t *testing.T) {
	boom := errors.New("boom")
	worker := NewSyncLogTrimWorker(&stubSyncLogTrimmer{err: boom}, 30)

	err := worker.Work(context.Background(), &river.Job[SyncLogTrimArgs]{})
	require.ErrorIs(t, err, boom)
}
