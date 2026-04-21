package consumer

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// Stubs for the worker dependency interfaces.
// --------------------------------------------------------------------------

type stubCreateTaskRepo struct {
	task           *repository.ContactTask
	poolReadErr    error
	freshTask      *repository.ContactTask
	freshReadErr   error
	finalizeErr    error
	setExternalErr error
	finalizeCalls  int
	setExtCalls    int
}

func (s *stubCreateTaskRepo) GetContactTask(_ context.Context, _ uuid.UUID) (*repository.ContactTask, error) {
	if s.poolReadErr != nil {
		return nil, s.poolReadErr
	}
	if s.task == nil {
		return nil, errors.New("stubCreateTaskRepo: no task configured")
	}
	return s.task, nil
}

func (s *stubCreateTaskRepo) GetContactTaskTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.ContactTask, error) {
	if s.freshReadErr != nil {
		return nil, s.freshReadErr
	}
	t := s.freshTask
	if t == nil {
		t = s.task
	}
	if t == nil {
		return nil, errors.New("stubCreateTaskRepo: no fresh task configured")
	}
	return t, nil
}

func (s *stubCreateTaskRepo) UpdateContactTaskExternalIDTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, externalID string) (*repository.ContactTask, error) {
	s.finalizeCalls++
	if s.finalizeErr != nil {
		return nil, s.finalizeErr
	}
	out := *s.task
	out.ExternalTaskID = externalID
	out.State = repository.ContactTaskStateManaged
	return &out, nil
}

func (s *stubCreateTaskRepo) SetContactTaskExternalIDOnlyTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ string) error {
	s.setExtCalls++
	return s.setExternalErr
}

type stubTodoistClient struct {
	syncErrs  []error
	syncCalls []todoist.SyncCommand
	realIDs   map[string]string
}

func (s *stubTodoistClient) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("not implemented")
}

func (s *stubTodoistClient) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	for _, c := range commands {
		s.syncCalls = append(s.syncCalls, c)
	}
	// Pop the next error if any; leave realIDs static.
	if len(s.syncErrs) > 0 {
		err := s.syncErrs[0]
		s.syncErrs = s.syncErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	tempMap := map[string]string{}
	for _, c := range commands {
		if c.Type == "item_add" {
			real := s.realIDs[c.TempID]
			if real == "" {
				real = "real-" + c.TempID
			}
			tempMap[c.TempID] = real
		}
	}
	return &todoist.SyncResponse{TempIDMap: tempMap}, nil
}

func newStubTodoistFactory(client *stubTodoistClient) todoist.ClientFactory {
	return func(string) todoist.Client {
		return client
	}
}

func settingsOK() TodoistSettingsFunc {
	return func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "proj", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
	}
}

// --------------------------------------------------------------------------
// TodoistFollowUpCreateJobWorker tests.
// --------------------------------------------------------------------------

func TestTodoistFollowUpCreateWorker_NonCutoverModesFail(t *testing.T) {
	for _, mode := range []string{FollowUpModeOff, FollowUpModeShadow} {
		t.Run(mode, func(t *testing.T) {
			w := NewTodoistFollowUpCreateJobWorker(mode, nil, nil, nil, nil, nil)
			err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
				Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "mode="+mode)
		})
	}
}

func TestTodoistFollowUpCreateWorker_MissingDependenciesFail(t *testing.T) {
	w := NewTodoistFollowUpCreateJobWorker(FollowUpModeCutover, nil, nil, nil, nil, nil)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")
}

// --------------------------------------------------------------------------
// TodoistFollowUpCloseJobWorker tests.
// --------------------------------------------------------------------------

type stubCloseTaskRepo struct {
	task *repository.ContactTask
	err  error
}

func (s *stubCloseTaskRepo) GetContactTask(context.Context, uuid.UUID) (*repository.ContactTask, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.task, nil
}

func TestTodoistFollowUpCloseWorker_NonCutoverModesFail(t *testing.T) {
	for _, mode := range []string{FollowUpModeOff, FollowUpModeShadow} {
		t.Run(mode, func(t *testing.T) {
			w := NewTodoistFollowUpCloseJobWorker(mode, nil, nil, nil)
			err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
				Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: uuid.New()},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "mode="+mode)
		})
	}
}

func TestTodoistFollowUpCloseWorker_EmptyExternalID_ReturnsError(t *testing.T) {
	taskID := uuid.New()
	repo := &stubCloseTaskRepo{task: &repository.ContactTask{ID: taskID, ExternalTaskID: ""}}
	client := &stubTodoistClient{}
	w := NewTodoistFollowUpCloseJobWorker(FollowUpModeCutover, repo, settingsOK(), newStubTodoistFactory(client))
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: taskID},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "external_task_id not yet populated")
	require.Empty(t, client.syncCalls, "must not call Todoist when external_task_id is empty")
}

func TestTodoistFollowUpCloseWorker_Success(t *testing.T) {
	taskID := uuid.New()
	repo := &stubCloseTaskRepo{task: &repository.ContactTask{ID: taskID, ExternalTaskID: "remote-123"}}
	client := &stubTodoistClient{}
	w := NewTodoistFollowUpCloseJobWorker(FollowUpModeCutover, repo, settingsOK(), newStubTodoistFactory(client))
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: taskID},
	})
	require.NoError(t, err)
	require.Len(t, client.syncCalls, 1)
	require.Equal(t, "item_close", client.syncCalls[0].Type)
	require.Equal(t, "remote-123", client.syncCalls[0].Args["id"])
}

func TestTodoistFollowUpCloseWorker_TodoistFailureBubblesUp(t *testing.T) {
	taskID := uuid.New()
	repo := &stubCloseTaskRepo{task: &repository.ContactTask{ID: taskID, ExternalTaskID: "remote-123"}}
	client := &stubTodoistClient{syncErrs: []error{errors.New("500 upstream")}}
	w := NewTodoistFollowUpCloseJobWorker(FollowUpModeCutover, repo, settingsOK(), newStubTodoistFactory(client))
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: taskID},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "todoist close")
}

// --------------------------------------------------------------------------
// TodoistFollowUpRefreshJobWorker tests.
// --------------------------------------------------------------------------

func TestTodoistFollowUpRefreshWorker_NonCutoverModesFail(t *testing.T) {
	w := NewTodoistFollowUpRefreshJobWorker(FollowUpModeShadow, nil, nil, nil)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]{
		Args: consumerjobs.TodoistFollowUpRefreshJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "mode=shadow")
}

func TestTodoistFollowUpRefreshWorker_EmptyExternalID_NoOp(t *testing.T) {
	taskID := uuid.New()
	repo := &stubCloseTaskRepo{task: &repository.ContactTask{ID: taskID, ExternalTaskID: ""}}
	client := &stubTodoistClient{}
	w := NewTodoistFollowUpRefreshJobWorker(FollowUpModeCutover, repo, settingsOK(), newStubTodoistFactory(client))
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]{
		Args: consumerjobs.TodoistFollowUpRefreshJobArgs{ContactTaskID: taskID},
	})
	require.NoError(t, err, "empty external_task_id means no remote task to refresh — no-op, not retry")
	require.Empty(t, client.syncCalls)
}
