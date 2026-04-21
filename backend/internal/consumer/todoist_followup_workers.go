package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// buildItemAddCommandFromMetadata reconstructs the Todoist item_add
// command stored at step-1 time in contact_task.metadata. The temp_id
// is deterministic (contact_task.id.String()), so crash-retries emit
// the same temp_id and Todoist's server-side dedupe returns the same
// real id on success.
func buildItemAddCommandFromMetadata(task *repository.ContactTask) (todoist.SyncCommand, error) {
	if task.Metadata == nil {
		return todoist.SyncCommand{}, fmt.Errorf("contact_task %s: missing metadata for item_add", task.ID)
	}
	content, _ := task.Metadata["content"].(string)
	dueDate, _ := task.Metadata["due_date"].(string)
	markerJSON, _ := task.Metadata["marker_json"].(string)
	projectID, _ := task.Metadata["project_id"].(string)
	labelName, _ := task.Metadata["label_name"].(string)
	if content == "" || dueDate == "" {
		return todoist.SyncCommand{}, fmt.Errorf("contact_task %s: incomplete metadata for item_add (content/due_date)", task.ID)
	}
	labels := []string{}
	if labelName != "" {
		labels = append(labels, labelName)
	}
	cmd := todoist.NewItemAddCommand(content, markerJSON, projectID, labels, &dueDate)
	// Override temp_id so crash-retries dedup server-side.
	cmd.TempID = task.ID.String()
	// UUID should also be deterministic-per-attempt; reuse temp_id-based
	// derivation so a retry with the same payload gets the same UUID.
	cmd.UUID = deterministicCommandUUID(task.ID)
	return cmd, nil
}

// deterministicCommandUUID returns a UUID-v5 derived from the contact
// task id under a fixed namespace so the same contact_task retries
// produce the same command UUID. Todoist rejects duplicate UUIDs
// on a single session but tolerates them across sessions; the v5 hash
// keeps us safe both ways.
func deterministicCommandUUID(contactTaskID uuid.UUID) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("todoist_followup_create:"+contactTaskID.String())).String()
}

// followUpCreateWorkerDeps bundles the dependencies
// TodoistFollowUpCreateJobWorker needs to perform the three-phase
// create (read → HTTP → write). Split out because the worker goes
// through core.md rule 153 territory (no tx across HTTP) and needs
// both tx-threaded writes and pool access for phase 3.
type followUpCreateWorkerDeps struct {
	taskRepo      followUpCreateTaskRepo
	settings      TodoistSettingsFunc
	clientFactory todoist.ClientFactory
	riverInserter RiverInserter
	pool          *pgxpool.Pool
}

// followUpCreateTaskRepo is the subset of ContactTaskRepository the
// create worker needs: a pool-scoped read to branch on state + tx-
// threaded writes for phase 3.
type followUpCreateTaskRepo interface {
	GetContactTask(ctx context.Context, id uuid.UUID) (*repository.ContactTask, error)
	GetContactTaskTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ContactTask, error)
	UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) (*repository.ContactTask, error)
	SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error
}

// TodoistFollowUpCreateJobWorker runs the step-2 Todoist item_add for
// a follow-up task, then finalizes the local pending_remote_create row
// to state='managed'. Handles the close-while-pending race (an inbound
// arrived while this worker was in flight) by doing create-then-close
// in one invocation and persisting external_task_id without flipping
// state back to managed (single-owner semantics — no duplicate closes).
type TodoistFollowUpCreateJobWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCreateJobArgs]
	mode string
	deps followUpCreateWorkerDeps
}

// NewTodoistFollowUpCreateJobWorker constructs the worker. mode is
// used to produce a clear error if a job is somehow enqueued while
// the consumer is in a non-cutover mode (which shouldn't happen —
// only cutover enqueues these jobs).
func NewTodoistFollowUpCreateJobWorker(
	mode string,
	taskRepo followUpCreateTaskRepo,
	settings TodoistSettingsFunc,
	clientFactory todoist.ClientFactory,
	riverInserter RiverInserter,
	pool *pgxpool.Pool,
) *TodoistFollowUpCreateJobWorker {
	return &TodoistFollowUpCreateJobWorker{
		mode: mode,
		deps: followUpCreateWorkerDeps{
			taskRepo:      taskRepo,
			settings:      settings,
			clientFactory: clientFactory,
			riverInserter: riverInserter,
			pool:          pool,
		},
	}
}

// Work implements the three-phase create. Split across read / HTTP /
// write phases so the tx-across-HTTP rule (core.md rule 153) holds.
func (w *TodoistFollowUpCreateJobWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) error {
	if w.mode != FollowUpModeCutover {
		return fmt.Errorf("todoist_followup_create invoked in mode=%s; enqueuing is a cutover-only path", w.mode)
	}
	if w.deps.taskRepo == nil || w.deps.settings == nil || w.deps.clientFactory == nil || w.deps.pool == nil {
		return errors.New("todoist_followup_create: worker dependencies not wired")
	}

	// Phase 1 — read (pool-scoped; no tx wrap needed for a single SELECT).
	task, err := w.deps.taskRepo.GetContactTask(ctx, j.Args.ContactTaskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", j.Args.ContactTaskID, err)
	}

	switch task.State {
	case repository.ContactTaskStateManaged:
		// Another worker already finalized this row. Idempotent no-op.
		return nil
	case repository.ContactTaskStatePendingRemoteCreate, repository.ContactTaskStateCompleted:
		// Continue to phase 2.
	default:
		return fmt.Errorf("unexpected state %q for follow-up create worker", task.State)
	}

	// Phase 2 — HTTP call(s) to Todoist (no tx open).
	itemAdd, err := buildItemAddCommandFromMetadata(task)
	if err != nil {
		return fmt.Errorf("build item_add command: %w", err)
	}
	settings, accessToken, err := w.deps.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	_ = settings // only accessToken is needed; settings shape was captured at step-1 time.

	client := w.deps.clientFactory(accessToken)
	resp, err := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{itemAdd})
	if err != nil {
		return fmt.Errorf("todoist item_add: %w", err)
	}
	realID, ok := resp.TempIDMap[itemAdd.TempID]
	if !ok {
		return fmt.Errorf("todoist: no temp_id mapping in response for %s", itemAdd.TempID)
	}

	// If the row was already completed when we entered (inbound arrived
	// while step-2 was pending), also perform the close. Still outside
	// the tx — it's a second HTTP call.
	closeFailed := false
	if task.State == repository.ContactTaskStateCompleted {
		closeCmd := todoist.NewItemCloseCommand(realID)
		if _, closeErr := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{closeCmd}); closeErr != nil {
			closeFailed = true
		}
	}

	// Phase 3 — write (short tx). Re-read inside the tx so the state-
	// dispatch decision survives a parallel writer that landed between
	// phase 1 and phase 3.
	return pgx.BeginTxFunc(ctx, w.deps.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		fresh, err := w.deps.taskRepo.GetContactTaskTx(ctx, tx, j.Args.ContactTaskID)
		if err != nil {
			return fmt.Errorf("re-read contact_task %s: %w", j.Args.ContactTaskID, err)
		}
		switch fresh.State {
		case repository.ContactTaskStatePendingRemoteCreate:
			// Normal finalize: query atomically sets state='managed' +
			// external_task_id in one UPDATE.
			if _, err := w.deps.taskRepo.UpdateContactTaskExternalIDTx(ctx, tx, fresh.ID, realID); err != nil {
				return fmt.Errorf("finalize pending_remote_create: %w", err)
			}
			return nil
		case repository.ContactTaskStateCompleted:
			// Persist external_task_id WITHOUT flipping state.
			if err := w.deps.taskRepo.SetContactTaskExternalIDOnlyTx(ctx, tx, fresh.ID, realID); err != nil {
				return fmt.Errorf("persist external id on completed row: %w", err)
			}
			if closeFailed && w.deps.riverInserter != nil {
				_, insertErr := w.deps.riverInserter.InsertTx(ctx, tx,
					consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: fresh.ID},
					&river.InsertOpts{MaxAttempts: 10})
				if insertErr != nil {
					return fmt.Errorf("enqueue fallback close: %w", insertErr)
				}
			}
			return nil
		case repository.ContactTaskStateManaged:
			// Another worker finalized while we were mid-HTTP — idempotent.
			return nil
		default:
			return fmt.Errorf("unexpected state %q at write phase", fresh.State)
		}
	})
}

// Timeout bounds the per-job runtime. The create path issues one or
// two HTTP calls plus a short write tx; 60s covers Pi-level HTTP +
// pool saturation headroom.
func (*TodoistFollowUpCreateJobWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) time.Duration {
	return 60 * time.Second
}

// followUpCloseTaskRepo is the subset of ContactTaskRepository the
// close worker needs: a pool-scoped read.
type followUpCloseTaskRepo interface {
	GetContactTask(ctx context.Context, id uuid.UUID) (*repository.ContactTask, error)
}

// TodoistFollowUpCloseJobWorker issues a Todoist item_close for a
// completed follow-up row. On transient failure river retries
// exponentially (MaxAttempts=10 at enqueue time).
type TodoistFollowUpCloseJobWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
	mode          string
	taskRepo      followUpCloseTaskRepo
	settings      TodoistSettingsFunc
	clientFactory todoist.ClientFactory
}

// NewTodoistFollowUpCloseJobWorker constructs the close worker.
func NewTodoistFollowUpCloseJobWorker(
	mode string,
	taskRepo followUpCloseTaskRepo,
	settings TodoistSettingsFunc,
	clientFactory todoist.ClientFactory,
) *TodoistFollowUpCloseJobWorker {
	return &TodoistFollowUpCloseJobWorker{
		mode:          mode,
		taskRepo:      taskRepo,
		settings:      settings,
		clientFactory: clientFactory,
	}
}

// Work issues item_close. Returns a retryable error if
// external_task_id is still empty (defensive — shouldn't happen under
// single-owner rule but guards against ordering surprises).
func (w *TodoistFollowUpCloseJobWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	if w.mode != FollowUpModeCutover {
		return fmt.Errorf("todoist_followup_close invoked in mode=%s; enqueuing is a cutover-only path", w.mode)
	}
	if w.taskRepo == nil || w.settings == nil || w.clientFactory == nil {
		return errors.New("todoist_followup_close: worker dependencies not wired")
	}
	task, err := w.taskRepo.GetContactTask(ctx, j.Args.ContactTaskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", j.Args.ContactTaskID, err)
	}
	if task.ExternalTaskID == "" {
		return fmt.Errorf("todoist_followup_close: external_task_id not yet populated for %s", task.ID)
	}
	_, accessToken, err := w.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	client := w.clientFactory(accessToken)
	closeCmd := todoist.NewItemCloseCommand(task.ExternalTaskID)
	if _, err := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{closeCmd}); err != nil {
		return fmt.Errorf("todoist close: %w", err)
	}
	return nil
}

// Timeout bounds the per-job runtime.
func (*TodoistFollowUpCloseJobWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) time.Duration {
	return 30 * time.Second
}

// TodoistFollowUpRefreshJobWorker retries Todoist item_update for a
// follow-up whose deadline was updated locally but whose post-commit
// HTTP attempt failed. The local row is authoritative; this worker
// just keeps Todoist eventually consistent.
type TodoistFollowUpRefreshJobWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpRefreshJobArgs]
	mode          string
	taskRepo      followUpCloseTaskRepo
	settings      TodoistSettingsFunc
	clientFactory todoist.ClientFactory
}

// NewTodoistFollowUpRefreshJobWorker constructs the refresh worker.
func NewTodoistFollowUpRefreshJobWorker(
	mode string,
	taskRepo followUpCloseTaskRepo,
	settings TodoistSettingsFunc,
	clientFactory todoist.ClientFactory,
) *TodoistFollowUpRefreshJobWorker {
	return &TodoistFollowUpRefreshJobWorker{
		mode:          mode,
		taskRepo:      taskRepo,
		settings:      settings,
		clientFactory: clientFactory,
	}
}

// Work issues item_update with the stored NewDeadline argument.
// Returns a retryable error on HTTP failure so river backs off.
func (w *TodoistFollowUpRefreshJobWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]) error {
	if w.mode != FollowUpModeCutover {
		return fmt.Errorf("todoist_followup_refresh invoked in mode=%s; enqueuing is a cutover-only path", w.mode)
	}
	if w.taskRepo == nil || w.settings == nil || w.clientFactory == nil {
		return errors.New("todoist_followup_refresh: worker dependencies not wired")
	}
	task, err := w.taskRepo.GetContactTask(ctx, j.Args.ContactTaskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", j.Args.ContactTaskID, err)
	}
	if task.ExternalTaskID == "" {
		// No remote task yet — the create worker will pick up the new
		// metadata when it runs, so a refresh retry is redundant.
		return nil
	}
	_, accessToken, err := w.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	client := w.clientFactory(accessToken)
	deadlineStr := j.Args.NewDeadline.Format(todoist.DateFormat)
	updateCmd := todoist.NewItemUpdateCommand(task.ExternalTaskID, map[string]any{
		"deadline": map[string]string{"date": deadlineStr},
	})
	if _, err := client.Sync(ctx, "*", []string{}, []todoist.SyncCommand{updateCmd}); err != nil {
		return fmt.Errorf("todoist item_update: %w", err)
	}
	return nil
}

// Timeout bounds the per-job runtime.
func (*TodoistFollowUpRefreshJobWorker) Timeout(*river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]) time.Duration {
	return 30 * time.Second
}
