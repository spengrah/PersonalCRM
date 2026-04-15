package scheduler

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSyncServiceForAccount struct {
	calledSource    string
	calledAccountID *string
	returnErr       error
}

func (f *fakeSyncServiceForAccount) RunAccountSync(_ context.Context, source string, accountID *string) error {
	f.calledSource = source
	f.calledAccountID = accountID
	return f.returnErr
}

func TestSyncProviderAccountWorker_Work_CallsRunAccountSync(t *testing.T) {
	acct := "u@example.com"
	svc := &fakeSyncServiceForAccount{}
	worker := NewSyncProviderAccountWorker(svc)

	err := worker.Work(context.Background(), &river.Job[SyncProviderAccountArgs]{
		Args: SyncProviderAccountArgs{Source: "gmail", AccountID: &acct},
	})
	require.NoError(t, err)
	assert.Equal(t, "gmail", svc.calledSource)
	require.NotNil(t, svc.calledAccountID)
	assert.Equal(t, "u@example.com", *svc.calledAccountID)
}

func TestSyncProviderAccountWorker_Work_PropagatesError(t *testing.T) {
	svc := &fakeSyncServiceForAccount{returnErr: errors.New("provider failed")}
	worker := NewSyncProviderAccountWorker(svc)

	err := worker.Work(context.Background(), &river.Job[SyncProviderAccountArgs]{
		Args: SyncProviderAccountArgs{Source: "gmail"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider failed")
}

func TestSyncProviderAccountWorker_Work_NotFoundIsTerminal(t *testing.T) {
	svc := &fakeSyncServiceForAccount{returnErr: db.ErrNotFound}
	worker := NewSyncProviderAccountWorker(svc)

	err := worker.Work(context.Background(), &river.Job[SyncProviderAccountArgs]{
		Args: SyncProviderAccountArgs{Source: "gmail"},
	})
	require.NoError(t, err, "db.ErrNotFound should be swallowed as terminal")
}
