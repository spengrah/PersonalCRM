// Transitional adapter workers for the three legacy Todoist follow-up
// River job kinds (todoist_followup_create / close / refresh). Jobs of
// these kinds queued before the todoist_task_op cutover deployed — plus
// the two legacy close-enqueue sites that survive until the cadence
// cutover (provider temp-id finalize, contact-merge close) — still need
// to execute after the bespoke workers are deleted. Each adapter is a
// thin shim delegating to the unified executor's corresponding verb with
// the same ContactTaskID.
//
// Remove once legacy todoist_followup_* jobs are drained in prod (the
// reconciler arc doc owns the sequencing).
package consumer

import (
	"context"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// legacyFollowupCreateUUID is the OLD command-UUID derivation the deleted
// create worker used. Retained for the create adapter only: a queued
// legacy job that already attempted pre-deploy must retry with an
// UNCHANGED command UUID — switching derivations mid-retry-chain would
// weaken Todoist-side dedup exactly at cutover. New-kind create ops use
// defaultCreateCommandUUID.
func legacyFollowupCreateUUID(contactTaskID uuid.UUID) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("todoist_followup_create:"+contactTaskID.String())).String()
}

// TodoistFollowUpCreateAdapterWorker executes queued todoist_followup_create
// jobs via the unified executor's create verb, injecting the legacy
// command-UUID derivation.
type TodoistFollowUpCreateAdapterWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCreateJobArgs]
	executor *TodoistTaskOpWorker
}

// NewTodoistFollowUpCreateAdapterWorker wraps the shared executor.
func NewTodoistFollowUpCreateAdapterWorker(executor *TodoistTaskOpWorker) *TodoistFollowUpCreateAdapterWorker {
	return &TodoistFollowUpCreateAdapterWorker{executor: executor}
}

// Work delegates to the executor's create verb with the legacy UUID fn.
func (w *TodoistFollowUpCreateAdapterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) error {
	if err := w.executor.depsWired(); err != nil {
		return err
	}
	return w.executor.executeCreate(ctx, j.Args.ContactTaskID, legacyFollowupCreateUUID)
}

// Timeout matches the executor's create budget.
func (*TodoistFollowUpCreateAdapterWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) time.Duration {
	return 60 * time.Second
}

// TodoistFollowUpCloseAdapterWorker executes queued todoist_followup_close
// jobs via the unified executor's close verb.
type TodoistFollowUpCloseAdapterWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
	executor *TodoistTaskOpWorker
}

// NewTodoistFollowUpCloseAdapterWorker wraps the shared executor.
func NewTodoistFollowUpCloseAdapterWorker(executor *TodoistTaskOpWorker) *TodoistFollowUpCloseAdapterWorker {
	return &TodoistFollowUpCloseAdapterWorker{executor: executor}
}

// Work delegates to the executor's close verb.
func (w *TodoistFollowUpCloseAdapterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	if err := w.executor.depsWired(); err != nil {
		return err
	}
	return w.executor.executeTerminalOp(ctx, j.Args.ContactTaskID, consumerjobs.TaskOpClose)
}

// Timeout matches the executor's single-HTTP-call budget.
func (*TodoistFollowUpCloseAdapterWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) time.Duration {
	return 30 * time.Second
}

// TodoistFollowUpRefreshAdapterWorker executes queued
// todoist_followup_refresh jobs via the unified executor's
// update_deadline verb. The carried NewDeadline is deliberately IGNORED:
// ops are convergence instructions, so the executor pushes the row's
// current metadata due_date — strictly more correct than replaying a
// value that may have gone stale in the queue.
type TodoistFollowUpRefreshAdapterWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpRefreshJobArgs]
	executor *TodoistTaskOpWorker
}

// NewTodoistFollowUpRefreshAdapterWorker wraps the shared executor.
func NewTodoistFollowUpRefreshAdapterWorker(executor *TodoistTaskOpWorker) *TodoistFollowUpRefreshAdapterWorker {
	return &TodoistFollowUpRefreshAdapterWorker{executor: executor}
}

// Work delegates to the executor's update_deadline verb.
func (w *TodoistFollowUpRefreshAdapterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]) error {
	if err := w.executor.depsWired(); err != nil {
		return err
	}
	return w.executor.executeUpdate(ctx, j.Args.ContactTaskID, deadlineUpdateVerb())
}

// Timeout matches the executor's single-HTTP-call budget.
func (*TodoistFollowUpRefreshAdapterWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]) time.Duration {
	return 30 * time.Second
}
