package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStalenessChecker struct {
	called    bool
	returnErr error
}

func (f *fakeStalenessChecker) RunChecks(_ context.Context) error {
	f.called = true
	return f.returnErr
}

func TestStalenessWatchdogWorker_Work_CallsRunChecks(t *testing.T) {
	checker := &fakeStalenessChecker{}
	worker := NewStalenessWatchdogWorker(checker)

	err := worker.Work(context.Background(), &river.Job[StalenessWatchdogArgs]{
		Args: StalenessWatchdogArgs{},
	})
	require.NoError(t, err)
	assert.True(t, checker.called, "Work should delegate to RunChecks")
}

func TestStalenessWatchdogWorker_Work_WrapsError(t *testing.T) {
	checker := &fakeStalenessChecker{returnErr: errors.New("boom")}
	worker := NewStalenessWatchdogWorker(checker)

	err := worker.Work(context.Background(), &river.Job[StalenessWatchdogArgs]{
		Args: StalenessWatchdogArgs{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Contains(t, err.Error(), "run staleness checks")
}

func TestStalenessWatchdogArgs_Kind(t *testing.T) {
	assert.Equal(t, "sync_staleness_watchdog", StalenessWatchdogArgs{}.Kind())
}
