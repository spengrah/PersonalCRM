package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// opUpdateSnoozeDelay is how long an update/close/delete op waits when it
// lands on a row whose remote task does not exist yet (still
// pending_remote_create), or when an update's verify-after-push detects
// the pushed value is already stale. Snoozing is attempts-neutral (River
// bumps MaxAttempts), so the op can wait out an arbitrarily long create
// outage without being discarded.
const opUpdateSnoozeDelay = 30 * time.Second

// opModeOffSnoozeDelay is how long a followup_loop op waits when follow-up
// writes are disabled (EVENT_BUS_FOLLOWUP_MODE=off). Snoozing is
// attempts-neutral, so the queued op pauses under the emergency kill switch
// and resumes — draining the suppressed work — once the operator restarts
// into cutover, rather than burning MaxAttempts and dead-lettering. Modestly
// longer than the create-wait to cut wake churn; the exact value is not
// load-bearing.
const opModeOffSnoozeDelay = 60 * time.Second

// todoistOpTaskRepo is the subset of ContactTaskRepository the executor
// needs: a pool-scoped read to branch on state (and to verify-after-push),
// plus the tx-threaded finalize writes for the create verb.
type todoistOpTaskRepo interface {
	GetContactTask(ctx context.Context, id uuid.UUID) (*repository.ContactTask, error)
	GetContactTaskTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ContactTask, error)
	UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) (*repository.ContactTask, error)
	SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error
}

// todoistTaskOpDeps bundles the executor's dependencies. Interface-typed
// (consumer-package convention) so unit tests substitute fakes. Mirrors
// the deleted followUpCreateWorkerDeps: a task-repo subset, the settings
// func, the client factory, a river inserter (finalize enqueues a close
// op), and the pool for the create verb's short finalize tx.
type todoistTaskOpDeps struct {
	taskRepo      todoistOpTaskRepo
	settings      TodoistSettingsFunc
	clientFactory todoist.ClientFactory
	riverInserter RiverInserter
	pool          *pgxpool.Pool
}

// TodoistTaskOpWorker is the single River executor for every Todoist
// mutation. Work dispatches on the op verb (create / close / delete /
// update_deadline / update_description). Ops are convergence instructions:
// the executor reads the row's CURRENT authoritative local state at
// execution time and pushes that, so duplicate/at-least-once ops are
// harmless (each re-reads and re-pushes; identical pushes dedup Todoist-
// side via temp_id / payload-fingerprint UUID).
//
// The executor serves all lifecycles and is mode-blind for every lifecycle
// EXCEPT followup_loop: when follow-up writes are disabled
// (followUpMode == off) it snoozes followup_loop ops before any Todoist call
// or local write, preserving the deleted follow-up workers' emergency kill
// switch. cadence_due / manual ops are never gated.
type TodoistTaskOpWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistTaskOpArgs]
	followUpMode string
	deps         todoistTaskOpDeps
}

// NewTodoistTaskOpWorker constructs the executor. followUpMode is the
// process-wide follow-up write mode (off / cutover) used only to gate
// followup_loop ops; all other dependencies are required in production
// wiring, and a nil-dep invocation fails closed with a descriptive error
// rather than panicking mid-verb.
func NewTodoistTaskOpWorker(
	followUpMode string,
	taskRepo todoistOpTaskRepo,
	settings TodoistSettingsFunc,
	clientFactory todoist.ClientFactory,
	riverInserter RiverInserter,
	pool *pgxpool.Pool,
) *TodoistTaskOpWorker {
	return &TodoistTaskOpWorker{
		followUpMode: followUpMode,
		deps: todoistTaskOpDeps{
			taskRepo:      taskRepo,
			settings:      settings,
			clientFactory: clientFactory,
			riverInserter: riverInserter,
			pool:          pool,
		},
	}
}

// depsWired returns a descriptive error when a required dependency is
// nil, so the executor and the legacy adapters that delegate to it fail
// closed instead of panicking mid-verb.
func (w *TodoistTaskOpWorker) depsWired() error {
	if w.deps.taskRepo == nil || w.deps.settings == nil || w.deps.clientFactory == nil {
		return errors.New("todoist_task_op: worker dependencies not wired")
	}
	return nil
}

// followUpWritesDisabled reports whether the follow-up emergency kill
// switch (followUpMode == off) suppresses this row's op. Scoped strictly to
// followup_loop rows so a follow-up-only switch never suppresses cadence_due
// or manual Todoist writes. Callers snooze (attempts-neutral) when true so
// the queued op resumes on a restart into cutover.
func (w *TodoistTaskOpWorker) followUpWritesDisabled(task *repository.ContactTask) bool {
	return w.followUpMode == FollowUpModeOff && task.Lifecycle == contacttask.LifecycleFollowUpLoop
}

// Work dispatches the op verb. Unknown verbs are a permanent failure
// (river.JobCancel) — a malformed enqueue must never retry forever.
func (w *TodoistTaskOpWorker) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistTaskOpArgs]) error {
	if err := w.depsWired(); err != nil {
		return err
	}
	taskID := j.Args.ContactTaskID
	switch j.Args.Op {
	case consumerjobs.TaskOpCreate:
		return w.executeCreate(ctx, taskID, defaultCreateCommandUUID)
	case consumerjobs.TaskOpClose, consumerjobs.TaskOpDelete:
		return w.executeTerminalOp(ctx, taskID, j.Args.Op)
	case consumerjobs.TaskOpUpdateDeadline:
		return w.executeUpdate(ctx, taskID, deadlineUpdateVerb())
	case consumerjobs.TaskOpUpdateDescription:
		return w.executeUpdate(ctx, taskID, descriptionUpdateVerb())
	default:
		return river.JobCancel(fmt.Errorf("todoist_task_op: unknown op %q", j.Args.Op))
	}
}

// Timeout bounds the per-job runtime. Create runs three phases (read →
// HTTP item_add → short finalize tx) so it gets 60s; the other verbs
// issue a single HTTP call and get 30s (matching the legacy adapters).
func (*TodoistTaskOpWorker) Timeout(j *river.Job[consumerjobs.TodoistTaskOpArgs]) time.Duration {
	if j.Args.Op == consumerjobs.TaskOpCreate {
		return 60 * time.Second
	}
	return 30 * time.Second
}

// defaultCreateCommandUUID is the command UUID for a new-kind create op:
// v5 over (create, taskID, "") — matching buildItemAddFromMetadata's
// default. The legacy create adapter injects a different derivation so a
// queued legacy job retries with its original UUID (DD5/DD9).
func defaultCreateCommandUUID(taskID uuid.UUID) string {
	return taskOpCommandUUID(consumerjobs.TaskOpCreate, taskID, "")
}

// executeCreate runs the create verb as a three-phase operation so the
// no-tx-across-HTTP rule holds. Phase 1: pool read + state dispatch
// (managed → no-op; pending/completed/superseded/dismissed/unmanaged →
// continue; unknown → error). Phase 2: HTTP item_add from the metadata
// snapshot with temp_id = row id and the command UUID from uuidFn. Phase
// 3: short tx — re-read and dispatch on the FRESH state:
//   - pending_remote_create → finalize (state=managed + external id)
//   - completed | superseded → record external id only + enqueue a close
//     op in the finalize tx (the remote task was just created but the row
//     is already retired; one op = one HTTP write)
//   - dismissed | unmanaged → record external id only, NO close
//   - managed → no-op (another worker finalized mid-flight)
func (w *TodoistTaskOpWorker) executeCreate(ctx context.Context, taskID uuid.UUID, uuidFn func(uuid.UUID) string) error {
	// Phase 1 — read (pool-scoped; single SELECT needs no tx).
	task, err := w.deps.taskRepo.GetContactTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", taskID, err)
	}
	if w.followUpWritesDisabled(task) {
		return river.JobSnooze(opModeOffSnoozeDelay)
	}
	switch task.State {
	case repository.ContactTaskStateManaged:
		// Another worker already finalized this row. Idempotent no-op.
		return nil
	case repository.ContactTaskStatePendingRemoteCreate,
		repository.ContactTaskStateCompleted,
		repository.ContactTaskStateSuperseded,
		repository.ContactTaskStateDismissed,
		repository.ContactTaskStateUnmanaged:
		// Continue to phase 2.
	default:
		return fmt.Errorf("todoist_task_op create: unexpected state %q for %s", task.State, taskID)
	}

	// Phase 2 — HTTP item_add (no tx open).
	itemAdd, err := buildItemAddFromMetadata(task)
	if err != nil {
		return fmt.Errorf("build item_add command: %w", err)
	}
	itemAdd.UUID = uuidFn(taskID)
	_, accessToken, err := w.deps.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	client := w.deps.clientFactory(accessToken)
	resp, err := client.Sync(ctx, "*", nil, []todoist.SyncCommand{itemAdd})
	if err != nil {
		return fmt.Errorf("todoist item_add: %w", err)
	}
	realID, ok := resp.TempIDMap[itemAdd.TempID]
	if !ok {
		return fmt.Errorf("todoist: no temp_id mapping in response for %s", itemAdd.TempID)
	}

	// Phase 3 — write (short tx). Re-read inside the tx so the finalize
	// decision gates on the FRESH state (a parallel writer may have
	// completed/dismissed/superseded the row while we were mid-HTTP).
	if w.deps.pool == nil {
		return errors.New("todoist_task_op create: pool not wired")
	}
	return pgx.BeginTxFunc(ctx, w.deps.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		fresh, err := w.deps.taskRepo.GetContactTaskTx(ctx, tx, taskID)
		if err != nil {
			return fmt.Errorf("re-read contact_task %s: %w", taskID, err)
		}
		return w.finalizeCreate(ctx, tx, fresh, realID)
	})
}

// finalizeCreate is phase 3 of the create verb: it dispatches on the
// FRESH (in-tx re-read) row state to persist the real external id and,
// for a row retired mid-create, enqueue a close op in the same tx. Split
// out so the dispatch matrix is unit-testable with a fake tx + repo +
// inserter without a live pool.
func (w *TodoistTaskOpWorker) finalizeCreate(ctx context.Context, tx pgx.Tx, fresh *repository.ContactTask, realID string) error {
	switch fresh.State {
	case repository.ContactTaskStatePendingRemoteCreate:
		if _, err := w.deps.taskRepo.UpdateContactTaskExternalIDTx(ctx, tx, fresh.ID, realID); err != nil {
			return fmt.Errorf("finalize pending_remote_create: %w", err)
		}
		return nil
	case repository.ContactTaskStateCompleted, repository.ContactTaskStateSuperseded:
		// Row retired mid-create: record the real id, then close the
		// freshly-created remote task via a close op in this tx.
		if err := w.deps.taskRepo.SetContactTaskExternalIDOnlyTx(ctx, tx, fresh.ID, realID); err != nil {
			return fmt.Errorf("persist external id on retired row: %w", err)
		}
		if w.deps.riverInserter == nil {
			return errors.New("todoist_task_op create finalize: river inserter not wired for close op")
		}
		if _, err := w.deps.riverInserter.InsertTx(ctx, tx,
			consumerjobs.TodoistTaskOpArgs{ContactTaskID: fresh.ID, Op: consumerjobs.TaskOpClose},
			&river.InsertOpts{MaxAttempts: 10}); err != nil {
			return fmt.Errorf("enqueue close op for retired row: %w", err)
		}
		return nil
	case repository.ContactTaskStateDismissed, repository.ContactTaskStateUnmanaged:
		// Record the real id, no close (legacy finalize rule preserved).
		if err := w.deps.taskRepo.SetContactTaskExternalIDOnlyTx(ctx, tx, fresh.ID, realID); err != nil {
			return fmt.Errorf("persist external id on dismissed/unmanaged row: %w", err)
		}
		return nil
	case repository.ContactTaskStateManaged:
		// Another worker finalized while we were mid-HTTP — idempotent.
		return nil
	default:
		return fmt.Errorf("todoist_task_op create: unexpected state %q at finalize", fresh.State)
	}
}

// executeTerminalOp runs the close or delete verb. Pool read only (no
// local write): if the row has no external id yet it snoozes while the
// row is still pending_remote_create (a create is in flight; the close/
// delete will succeed once finalize lands) and no-ops otherwise (nothing
// was ever created remotely). With an external id it issues item_close /
// item_delete with the deterministic command UUID; both are naturally
// idempotent server-side, so retries are harmless.
func (w *TodoistTaskOpWorker) executeTerminalOp(ctx context.Context, taskID uuid.UUID, op string) error {
	task, err := w.deps.taskRepo.GetContactTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", taskID, err)
	}
	if w.followUpWritesDisabled(task) {
		return river.JobSnooze(opModeOffSnoozeDelay)
	}
	if task.ExternalTaskID == "" {
		if task.State == repository.ContactTaskStatePendingRemoteCreate {
			return river.JobSnooze(opUpdateSnoozeDelay)
		}
		// Terminal row, nothing created remotely — nothing to close/delete.
		return nil
	}

	_, accessToken, err := w.deps.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	client := w.deps.clientFactory(accessToken)
	var cmd todoist.SyncCommand
	switch op {
	case consumerjobs.TaskOpClose:
		cmd = todoist.NewItemCloseCommand(task.ExternalTaskID)
	case consumerjobs.TaskOpDelete:
		cmd = todoist.NewItemDeleteCommand(task.ExternalTaskID)
	default:
		return river.JobCancel(fmt.Errorf("executeTerminalOp: non-terminal op %q", op))
	}
	cmd.UUID = taskOpCommandUUID(op, taskID, "")
	if _, err := client.Sync(ctx, "*", nil, []todoist.SyncCommand{cmd}); err != nil {
		return fmt.Errorf("todoist %s: %w", op, err)
	}
	return nil
}

// opUpdateVerb describes an update_* verb: how to extract the value being
// pushed from the row's current metadata, and how to fingerprint it for
// the command UUID.
type opUpdateVerb struct {
	op string
	// extract reads the value to push + builds the item_update args from
	// the row's CURRENT metadata. ok=false means a required metadata key
	// is absent → permanent error (the verb has nothing valid to push).
	extract func(task *repository.ContactTask) (value string, args map[string]any, ok bool)
	// fingerprint maps the pushed value to the command-UUID fingerprint.
	fingerprint func(value string) string
}

// deadlineUpdateVerb pushes the row's current `due_date` metadata as a
// Todoist deadline. Fingerprint is the deadline string itself.
//
// The item_update also clears the task's Todoist due date (`due: null`):
// the CRM only ever writes deadlines on follow-ups, so any due date is
// user-set (Todoist's quick-reschedule/postpone edits the due field, not
// the deadline). Left in place, a postponed task would keep surfacing as
// due-today even though the reply window just restarted.
func deadlineUpdateVerb() opUpdateVerb {
	return opUpdateVerb{
		op: consumerjobs.TaskOpUpdateDeadline,
		extract: func(task *repository.ContactTask) (string, map[string]any, bool) {
			due, ok := metadataString(task, "due_date")
			if !ok || due == "" {
				return "", nil, false
			}
			return due, map[string]any{
				"deadline": map[string]string{"date": due},
				"due":      nil,
			}, true
		},
		fingerprint: func(value string) string { return value },
	}
}

// descriptionUpdateVerb pushes the row's current `description` metadata
// verbatim. Fails permanently if the key is absent (arc §5.3 / DD4).
// Fingerprint is sha256(description) so distinct descriptions get distinct
// command UUIDs.
func descriptionUpdateVerb() opUpdateVerb {
	return opUpdateVerb{
		op: consumerjobs.TaskOpUpdateDescription,
		extract: func(task *repository.ContactTask) (string, map[string]any, bool) {
			desc, ok := metadataString(task, "description")
			if !ok {
				return "", nil, false
			}
			return desc, map[string]any{"description": desc}, true
		},
		fingerprint: descriptionFingerprint,
	}
}

// metadataString reads a string metadata key. ok=false when the key is
// absent or not a string.
func metadataString(task *repository.ContactTask, key string) (string, bool) {
	if task.Metadata == nil {
		return "", false
	}
	v, present := task.Metadata[key]
	if !present {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// executeUpdate runs an update_* verb. Phases: (1) pool read + state
// dispatch — pending_remote_create snoozes attempts-neutrally (a create is
// in flight; wait for finalize), ANY terminal state no-ops (the close/
// delete op owns terminal convergence — a stale update must never touch a
// retired task, even one with an external id), managed pushes the current
// value. (2) HTTP item_update built from the row's CURRENT metadata with
// the payload-fingerprint command UUID. (3) verify-after-push: re-read the
// row and, if the value just pushed is no longer current, snooze so the
// same job re-runs and pushes the newer value — closing the out-of-order-
// HTTP race that at-least-once enqueue alone does not.
func (w *TodoistTaskOpWorker) executeUpdate(ctx context.Context, taskID uuid.UUID, verb opUpdateVerb) error {
	task, err := w.deps.taskRepo.GetContactTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("fetch contact_task %s: %w", taskID, err)
	}
	if w.followUpWritesDisabled(task) {
		return river.JobSnooze(opModeOffSnoozeDelay)
	}
	switch task.State {
	case repository.ContactTaskStatePendingRemoteCreate:
		return river.JobSnooze(opUpdateSnoozeDelay)
	case repository.ContactTaskStateManaged:
		// Continue to push.
	default:
		// Any terminal state (completed/dismissed/superseded/unmanaged):
		// the close/delete op owns terminal convergence. No-op.
		return nil
	}

	value, args, ok := verb.extract(task)
	if !ok {
		return river.JobCancel(fmt.Errorf("todoist_task_op %s: contact_task %s missing metadata to push", verb.op, taskID))
	}
	if task.ExternalTaskID == "" {
		// Managed rows always carry an external id (finalize sets state + id
		// in one UPDATE; PR3's manual adopt does likewise), so an empty id on
		// a managed row is genuine corruption, not a race. Return a retryable
		// error so it exhausts MaxAttempts and surfaces in dead-letter rather
		// than snoozing forever.
		return fmt.Errorf("todoist_task_op %s: managed contact_task %s has empty external_task_id (corrupt row)", verb.op, taskID)
	}

	_, accessToken, err := w.deps.settings(ctx)
	if err != nil {
		return fmt.Errorf("get todoist settings: %w", err)
	}
	client := w.deps.clientFactory(accessToken)
	cmd := todoist.NewItemUpdateCommand(task.ExternalTaskID, args)
	cmd.UUID = taskOpCommandUUID(verb.op, taskID, verb.fingerprint(value))
	if _, err := client.Sync(ctx, "*", nil, []todoist.SyncCommand{cmd}); err != nil {
		return fmt.Errorf("todoist %s: %w", verb.op, err)
	}

	// Verify-after-push: a concurrent fresher op may have pushed a newer
	// value whose HTTP request landed BEFORE ours, silently reverting
	// Todoist. Detect value ≠ current and snooze to re-run.
	fresh, err := w.deps.taskRepo.GetContactTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("verify-after-push re-read %s: %w", taskID, err)
	}
	if freshValue, _, freshOK := verb.extract(fresh); freshOK && freshValue != value {
		return river.JobSnooze(opUpdateSnoozeDelay)
	}
	return nil
}

// taskOpCommandUUID derives the deterministic Todoist Sync command UUID
// (v5) for a task op. Computed at execution time over the value actually
// being pushed: the fingerprint is the deadline string for
// update_deadline, sha256(description) for update_description, and empty
// for create/close/delete. Retries of an unchanged push reuse the UUID
// (safe server-side dedup); a later push of a DIFFERENT value gets a
// DIFFERENT UUID, so Todoist never dedups a genuinely new command away.
func taskOpCommandUUID(op string, taskID uuid.UUID, fingerprint string) string {
	return uuid.NewSHA1(
		uuid.NameSpaceURL,
		[]byte("todoist_op:"+op+":"+taskID.String()+":"+fingerprint),
	).String()
}

// descriptionFingerprint returns the payload fingerprint for an
// update_description op: sha256 of the exact description string being
// pushed, hex-encoded.
func descriptionFingerprint(description string) string {
	sum := sha256.Sum256([]byte(description))
	return hex.EncodeToString(sum[:])
}

// buildItemAddFromMetadata reconstructs the Todoist item_add command from
// the snapshot stored in contact_task.metadata at row-creation time. The
// temp_id is deterministic (contact_task.id), so crash-retries emit the
// same temp_id and Todoist's server-side dedup returns the same real id.
// The command UUID is left as the default create-derivation; callers that
// need a different derivation (the legacy create adapter) override it.
func buildItemAddFromMetadata(task *repository.ContactTask) (todoist.SyncCommand, error) {
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
	// Deterministic temp_id so crash-retries dedup server-side. The command
	// UUID is set by the caller (executeCreate) via its uuidFn, so the
	// default random UUID here is always overwritten.
	cmd.TempID = task.ID.String()
	return cmd, nil
}
