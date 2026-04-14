package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSyncServiceForTick is a hand-rolled test double for SyncServiceForTick.
// It records every EnqueueAccountSyncIfNotInFlight call with its args and
// returns scripted (enqueued, err) pairs keyed by source.
type fakeSyncServiceForTick struct {
	mu              sync.Mutex
	listAccounts    []DueAccount
	listErr         error
	enqueueCalls    []enqueueCall
	enqueueResults  map[string]enqueueResult // by Source
	defaultEnqueued bool
}

type enqueueCall struct {
	Source    string
	AccountID *string
}

type enqueueResult struct {
	enqueued bool
	err      error
}

func (f *fakeSyncServiceForTick) ListDueAccounts(_ context.Context) ([]DueAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listAccounts, f.listErr
}

func (f *fakeSyncServiceForTick) EnqueueAccountSyncIfNotInFlight(
	_ context.Context, source string, accountID *string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueueCalls = append(f.enqueueCalls, enqueueCall{Source: source, AccountID: accountID})
	if res, ok := f.enqueueResults[source]; ok {
		return res.enqueued, res.err
	}
	return f.defaultEnqueued, nil
}

func TestSchedulerTickWorker_Work_EnqueuesPerDueAccount(t *testing.T) {
	acct1 := "acct-1"
	svc := &fakeSyncServiceForTick{
		listAccounts: []DueAccount{
			{Source: "gmail", AccountID: &acct1},
			{Source: "todoist", AccountID: nil},
			{Source: "calendar", AccountID: &acct1},
		},
		defaultEnqueued: true,
	}

	worker := NewSchedulerTickWorker(svc)
	err := worker.Work(context.Background(), &river.Job[SchedulerTickArgs]{Args: SchedulerTickArgs{}})
	require.NoError(t, err)

	require.Len(t, svc.enqueueCalls, 3)
	assert.Equal(t, "gmail", svc.enqueueCalls[0].Source)
	require.NotNil(t, svc.enqueueCalls[0].AccountID)
	assert.Equal(t, "acct-1", *svc.enqueueCalls[0].AccountID)
	assert.Equal(t, "todoist", svc.enqueueCalls[1].Source)
	assert.Nil(t, svc.enqueueCalls[1].AccountID)
	assert.Equal(t, "calendar", svc.enqueueCalls[2].Source)
}

func TestSchedulerTickWorker_Work_EmptyDueSetIsNoop(t *testing.T) {
	svc := &fakeSyncServiceForTick{listAccounts: nil}

	worker := NewSchedulerTickWorker(svc)
	err := worker.Work(context.Background(), &river.Job[SchedulerTickArgs]{Args: SchedulerTickArgs{}})
	require.NoError(t, err)
	assert.Empty(t, svc.enqueueCalls)
}

func TestSchedulerTickWorker_Work_SkipsWhenEnqueuerReturnsFalse(t *testing.T) {
	acct1 := "acct-1"
	svc := &fakeSyncServiceForTick{
		listAccounts: []DueAccount{
			{Source: "gmail", AccountID: &acct1},
			{Source: "todoist", AccountID: nil},
		},
		enqueueResults: map[string]enqueueResult{
			"gmail":   {enqueued: false, err: nil},
			"todoist": {enqueued: true, err: nil},
		},
	}

	worker := NewSchedulerTickWorker(svc)
	err := worker.Work(context.Background(), &river.Job[SchedulerTickArgs]{Args: SchedulerTickArgs{}})
	require.NoError(t, err)
	require.Len(t, svc.enqueueCalls, 2, "both sources were attempted even though gmail was deduped")
}

func TestSchedulerTickWorker_Work_ContinuesAfterPerAccountError(t *testing.T) {
	svc := &fakeSyncServiceForTick{
		listAccounts: []DueAccount{
			{Source: "gmail", AccountID: nil},
			{Source: "todoist", AccountID: nil},
			{Source: "calendar", AccountID: nil},
		},
		enqueueResults: map[string]enqueueResult{
			"gmail":    {enqueued: false, err: errors.New("transient")},
			"todoist":  {enqueued: true, err: nil},
			"calendar": {enqueued: true, err: nil},
		},
	}

	worker := NewSchedulerTickWorker(svc)
	err := worker.Work(context.Background(), &river.Job[SchedulerTickArgs]{Args: SchedulerTickArgs{}})
	require.NoError(t, err, "per-account error should not fail the tick")
	require.Len(t, svc.enqueueCalls, 3)
}

func TestSchedulerTickWorker_Work_ReturnsErrorWhenListFails(t *testing.T) {
	svc := &fakeSyncServiceForTick{listErr: errors.New("db down")}

	worker := NewSchedulerTickWorker(svc)
	err := worker.Work(context.Background(), &river.Job[SchedulerTickArgs]{Args: SchedulerTickArgs{}})
	require.Error(t, err, "list error should surface so river retries the tick")
	assert.Empty(t, svc.enqueueCalls)
}
