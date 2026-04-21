package consumer

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/riverqueue/river"
)

// TodoistFollowUpCreateJobWorker and TodoistFollowUpCloseJobWorker are
// registered so river knows their job kinds, but no code path enqueues
// them in shadow mode. A cutover-mode flip later in the rollout will
// switch the worker bodies to perform the actual Todoist side-effects.

type TodoistFollowUpCreateJobWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCreateJobArgs]
	mode string
}

// NewTodoistFollowUpCreateJobWorker constructs the worker. mode is used
// to produce a clear error if a job is somehow enqueued while the
// consumer is in a non-cutover mode.
func NewTodoistFollowUpCreateJobWorker(mode string) *TodoistFollowUpCreateJobWorker {
	return &TodoistFollowUpCreateJobWorker{mode: mode}
}

// Work returns an error in shadow mode — no code path should enqueue
// this job yet. The worker body will be implemented when cutover lands.
func (w *TodoistFollowUpCreateJobWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) error {
	if w.mode != FollowUpModeCutover {
		return fmt.Errorf("todoist_followup_create invoked in mode=%s; enqueuing is a cutover-only path", w.mode)
	}
	return fmt.Errorf("todoist_followup_create: cutover implementation not yet landed")
}

// Timeout bounds the per-job runtime.
func (*TodoistFollowUpCreateJobWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) time.Duration {
	return 30 * time.Second
}

type TodoistFollowUpCloseJobWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
	mode string
}

// NewTodoistFollowUpCloseJobWorker constructs the worker.
func NewTodoistFollowUpCloseJobWorker(mode string) *TodoistFollowUpCloseJobWorker {
	return &TodoistFollowUpCloseJobWorker{mode: mode}
}

// Work returns an error in shadow mode — no code path should enqueue
// this job yet.
func (w *TodoistFollowUpCloseJobWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	if w.mode != FollowUpModeCutover {
		return fmt.Errorf("todoist_followup_close invoked in mode=%s; enqueuing is a cutover-only path", w.mode)
	}
	return fmt.Errorf("todoist_followup_close: cutover implementation not yet landed")
}

// Timeout bounds the per-job runtime.
func (*TodoistFollowUpCloseJobWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) time.Duration {
	return 30 * time.Second
}
