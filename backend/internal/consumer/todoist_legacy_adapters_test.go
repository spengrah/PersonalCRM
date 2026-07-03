package consumer

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Transitional legacy-kind adapters: each delegates to the executor's
// corresponding verb with the same ContactTaskID.
// -----------------------------------------------------------------------------

func TestLegacyCreateAdapter_UsesOldUUIDDerivation(t *testing.T) {
	taskID := uuid.New()
	// The retained old derivation: a queued legacy job that already
	// attempted pre-deploy must retry with an UNCHANGED command UUID.
	expected := uuid.NewSHA1(uuid.NameSpaceURL, []byte("todoist_followup_create:"+taskID.String())).String()
	require.Equal(t, expected, legacyFollowupCreateUUID(taskID))
	// And it must differ from the new-kind derivation.
	require.NotEqual(t, defaultCreateCommandUUID(taskID), legacyFollowupCreateUUID(taskID))
}

func TestLegacyCreateAdapter_DelegatesWithLegacyUUID(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:       taskID,
		State:    repository.ContactTaskStatePendingRemoteCreate,
		Metadata: createMetadata(),
	})
	client := &fakeOpClient{}
	executor := newOpWorker(repo, client, &recordingInserter{})
	adapter := NewTodoistFollowUpCreateAdapterWorker(executor)

	err := adapter.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: taskID},
	})
	// The unit executor has no pool, so phase 3 (finalize) fails — the
	// item_add HTTP command was already issued and is what this test pins.
	// The full adapter create flow is integration-tested with a live pool.
	require.Error(t, err)
	require.Contains(t, err.Error(), "pool not wired")

	require.Len(t, client.commands, 1)
	cmd := client.commands[0]
	require.Equal(t, "item_add", cmd.Type)
	require.Equal(t, taskID.String(), cmd.TempID, "temp_id stays the row id")
	require.Equal(t, legacyFollowupCreateUUID(taskID), cmd.UUID,
		"legacy create adapter must keep the old command-UUID derivation")
}

func TestLegacyCloseAdapter_DelegatesToCloseVerb(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateCompleted,
		ExternalTaskID: "remote-legacy-1",
	})
	client := &fakeOpClient{}
	executor := newOpWorker(repo, client, &recordingInserter{})
	adapter := NewTodoistFollowUpCloseAdapterWorker(executor)

	err := adapter.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: taskID},
	})
	require.NoError(t, err)
	require.Len(t, client.commands, 1)
	require.Equal(t, "item_close", client.commands[0].Type)
	require.Equal(t, "remote-legacy-1", client.commands[0].Args["id"])
}

func TestLegacyRefreshAdapter_IgnoresCarriedDeadline(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-legacy-2",
		Metadata:       map[string]any{"due_date": "2026-05-05"},
	})
	client := &fakeOpClient{}
	executor := newOpWorker(repo, client, &recordingInserter{})
	adapter := NewTodoistFollowUpRefreshAdapterWorker(executor)

	// The carried NewDeadline is STALE by design — the adapter converges
	// on the row's current metadata instead (strictly more correct).
	carried := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	err := adapter.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]{
		Args: consumerjobs.TodoistFollowUpRefreshJobArgs{ContactTaskID: taskID, NewDeadline: carried},
	})
	require.NoError(t, err)
	require.Len(t, client.commands, 1)
	cmd := client.commands[0]
	require.Equal(t, "item_update", cmd.Type)
	require.Equal(t, map[string]string{"date": "2026-05-05"}, cmd.Args["deadline"],
		"refresh adapter must push the row's CURRENT due_date, not the carried NewDeadline")
}

func TestLegacyAdapters_MissingDepsError(t *testing.T) {
	executor := NewTodoistTaskOpWorker(FollowUpModeCutover, nil, nil, nil, nil, nil)

	err := NewTodoistFollowUpCreateAdapterWorker(executor).Work(context.Background(),
		&river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")

	err = NewTodoistFollowUpCloseAdapterWorker(executor).Work(context.Background(),
		&river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: uuid.New()}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")

	err = NewTodoistFollowUpRefreshAdapterWorker(executor).Work(context.Background(),
		&river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]{Args: consumerjobs.TodoistFollowUpRefreshJobArgs{ContactTaskID: uuid.New()}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")
}
