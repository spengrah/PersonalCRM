package consumer

import (
	"context"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The Todoist follow-up workers are registered so river knows their
// kinds, but no code path should enqueue them in shadow. These tests
// pin the shadow-mode refusal semantics so a cutover-mode flip alone
// cannot quietly activate the workers.

func TestTodoistFollowUpCreateJobWorker_ShadowReturnsError(t *testing.T) {
	w := NewTodoistFollowUpCreateJobWorker(FollowUpModeShadow)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "mode=shadow")
}

func TestTodoistFollowUpCreateJobWorker_OffReturnsError(t *testing.T) {
	w := NewTodoistFollowUpCreateJobWorker(FollowUpModeOff)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "mode=off")
}

func TestTodoistFollowUpCreateJobWorker_CutoverSignalsNotImplemented(t *testing.T) {
	w := NewTodoistFollowUpCreateJobWorker(FollowUpModeCutover)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet landed")
}

func TestTodoistFollowUpCloseJobWorker_ShadowReturnsError(t *testing.T) {
	w := NewTodoistFollowUpCloseJobWorker(FollowUpModeShadow)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "mode=shadow")
}

func TestTodoistFollowUpCloseJobWorker_CutoverSignalsNotImplemented(t *testing.T) {
	w := NewTodoistFollowUpCloseJobWorker(FollowUpModeCutover)
	err := w.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: uuid.New()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet landed")
}
