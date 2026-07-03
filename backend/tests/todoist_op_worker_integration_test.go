// Integration coverage for the unified TodoistTaskOpWorker executor
// against a live DB with a mocked Todoist client. Ports the old create-
// worker state-branching scenarios onto op=create and adds the op-
// lifecycle flows the arc requires:
//
//   - Normal finalize: pending_remote_create → managed after item_add.
//   - Already-managed idempotent no-op.
//   - Completed-mid-flight: finalize records the real id WITHOUT
//     flipping state and enqueues a close op in the finalize tx (one op
//     = one HTTP write; the old inline-close + fallback-job dance is
//     gone). Executing that close op issues item_close.
//   - Dismissed-mid-flight: finalize records the id only, NO close op.
//   - Refresh convergence: an update_deadline op snoozes while the row
//     is pending, then pushes the CURRENT due_date once managed.
//   - At-least-once convergence: two refreshes → two op jobs; both
//     execute; the final remote value equals the latest local value and
//     the duplicate push carries the same payload-fingerprint UUID.
//
// The old drift-refresh detection tests died with the mechanism: any
// post-enqueue local change inserts its OWN update_deadline op in the
// changing tx, and the executor's snooze + verify-after-push unit tests
// (internal/consumer/todoist_op_worker_test.go) cover the replacement
// convergence rules.
package tests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// Mock + env.
// --------------------------------------------------------------------------

// opWorkerMockTodoist records each Sync call and hands back deterministic
// temp-id-to-real-id mappings. `beforeSync` can mutate DB state while the
// HTTP call is "in flight" so the finalize / verify-after-push phases see
// something different than the initial read.
type opWorkerMockTodoist struct {
	mu         sync.Mutex
	calls      []todoist.SyncCommand
	realIDs    map[string]string
	beforeSync func(cmds []todoist.SyncCommand)
}

func (m *opWorkerMockTodoist) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("QuickAdd not implemented in mock")
}

func (m *opWorkerMockTodoist) Sync(_ context.Context, _ string, _ []string, cmds []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, cmds...)
	cb := m.beforeSync
	m.mu.Unlock()
	if cb != nil {
		cb(cmds)
	}
	tempMap := map[string]string{}
	for _, c := range cmds {
		if c.Type == "item_add" {
			real := m.realIDs[c.TempID]
			if real == "" {
				real = "real-" + c.TempID
			}
			tempMap[c.TempID] = real
		}
	}
	return &todoist.SyncResponse{TempIDMap: tempMap}, nil
}

func (m *opWorkerMockTodoist) snapshotCalls() []todoist.SyncCommand {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]todoist.SyncCommand, len(m.calls))
	copy(out, m.calls)
	return out
}

type opWorkerEnv struct {
	database    *db.Database
	gen         *factory.Generator
	contactRepo *repository.ContactRepository
	taskRepo    *repository.ContactTaskRepository
	riverClient *river.Client[pgx.Tx]
	worker      *consumer.TodoistTaskOpWorker
	mock        *opWorkerMockTodoist
}

func newOpWorkerEnv(t *testing.T) (*opWorkerEnv, func()) {
	t.Helper()
	database, closeFn := newFollowUpIntegrationDB(t)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	taskRepo := repository.NewContactTaskRepository(database.Queries)

	workers := river.NewWorkers()
	// Register the op kind so the finalize tx's close-op InsertTx
	// succeeds; we don't Start() the client so nothing auto-runs.
	river.AddWorker(workers, &followUpTestNoopOp{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	mock := &opWorkerMockTodoist{realIDs: map[string]string{}}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "proj-test", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
	}
	clientFactory := func(string) todoist.Client { return mock }

	worker := consumer.NewTodoistTaskOpWorker(taskRepo, settings, clientFactory, riverClient, database.Pool)
	gen, _ := migrationGenerator(t)
	env := &opWorkerEnv{
		database:    database,
		gen:         gen,
		contactRepo: contactRepo,
		taskRepo:    taskRepo,
		riverClient: riverClient,
		worker:      worker,
		mock:        mock,
	}
	return env, func() {
		_ = riverClient.Stop(context.Background())
		closeFn()
	}
}

func (e *opWorkerEnv) seedPendingTask(t *testing.T) *repository.ContactTask {
	t.Helper()
	ctx := context.Background()
	contact, err := e.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: e.gen.Contact(factory.WithNoMethods()).FullName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(ctx, contact.ID) })

	idem := "idem-" + uuid.NewString()
	metadata := map[string]any{
		"due_date":                "2026-06-01",
		"content":                 "Follow up: test",
		"marker_json":             `{"crm":true}`,
		"project_id":              "proj-test",
		"label_name":              "followup",
		"integration_instance_id": "inst",
	}
	var task *repository.ContactTask
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var createErr error
		task, createErr = e.taskRepo.CreateContactTaskTx(ctx, tx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       todoist.SourceName,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleFollowUpLoop,
			ExternalTaskID: "",
			State:          string(repository.ContactTaskStatePendingRemoteCreate),
			Metadata:       metadata,
		}, &idem)
		return createErr
	}))
	require.NotNil(t, task)
	require.Equal(t, repository.ContactTaskStatePendingRemoteCreate, task.State)
	return task
}

func (e *opWorkerEnv) runOp(ctx context.Context, taskID uuid.UUID, op string) error {
	return e.worker.Work(ctx, &river.Job[consumerjobs.TodoistTaskOpArgs]{
		Args: consumerjobs.TodoistTaskOpArgs{ContactTaskID: taskID, Op: op},
	})
}

func (e *opWorkerEnv) setDueDate(t *testing.T, taskID uuid.UUID, due string) {
	t.Helper()
	ctx := context.Background()
	fresh, err := e.taskRepo.GetContactTask(ctx, taskID)
	require.NoError(t, err)
	newMeta := map[string]any{}
	for k, v := range fresh.Metadata {
		newMeta[k] = v
	}
	newMeta["due_date"] = due
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := e.taskRepo.UpdateContactTaskMetadataTx(ctx, tx, taskID, newMeta)
		return err
	}))
}

func (e *opWorkerEnv) countOps(t *testing.T, taskID uuid.UUID, op string) int {
	t.Helper()
	n, err := e.taskRepo.CountTodoistOpJobsByOp(context.Background(), taskID, op)
	require.NoError(t, err)
	return int(n)
}

// --------------------------------------------------------------------------
// create verb — state branching.
// --------------------------------------------------------------------------

// TestIntegration_OpWorker_Create_NormalFinalize asserts the happy path:
// pending_remote_create → managed after item_add succeeds, with the
// deterministic temp_id = row id.
func TestIntegration_OpWorker_Create_NormalFinalize(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-123"

	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))

	calls := env.mock.snapshotCalls()
	require.Len(t, calls, 1, "exactly one item_add HTTP call on normal finalize")
	assert.Equal(t, "item_add", calls[0].Type)
	assert.Equal(t, task.ID.String(), calls[0].TempID, "temp_id = row id for server-side dedup")

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, fresh.State)
	assert.Equal(t, "real-123", fresh.ExternalTaskID)
}

// TestIntegration_OpWorker_Create_AlreadyManaged asserts the idempotent
// no-op path: the executor exits at phase 1 when the row is already
// managed (another worker finalized).
func TestIntegration_OpWorker_Create_AlreadyManaged(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.taskRepo.UpdateContactTaskExternalIDTx(ctx, tx, task.ID, "real-winner")
		return err
	}))

	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))
	assert.Empty(t, env.mock.snapshotCalls(), "no Todoist calls on already-managed path")

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "real-winner", fresh.ExternalTaskID, "external id untouched by the no-op")
}

// TestIntegration_OpWorker_Create_CompletedMidFlight asserts the close-
// while-pending race: the row completes while the create op is in
// flight. Finalize records the real id WITHOUT flipping state and
// enqueues a close op in the finalize tx; the create issues exactly one
// HTTP write (no inline close). Executing the close op then issues the
// item_close against the recorded external id.
func TestIntegration_OpWorker_Create_CompletedMidFlight(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-race"

	// Flip to completed while the item_add HTTP call is in flight.
	env.mock.beforeSync = func(cmds []todoist.SyncCommand) {
		if cmds[0].Type != "item_add" {
			return
		}
		_ = pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := env.taskRepo.UpdateContactTaskStateTx(ctx, tx, task.ID, repository.ContactTaskStateCompleted)
			return err
		})
	}

	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))

	calls := env.mock.snapshotCalls()
	require.Len(t, calls, 1, "one op = one HTTP write; the close is a separate op")
	assert.Equal(t, "item_add", calls[0].Type)

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, fresh.State, "state stays completed; no flip back to managed")
	assert.Equal(t, "real-race", fresh.ExternalTaskID, "external id recorded on the retired row")

	require.Equal(t, 1, env.countOps(t, task.ID, consumerjobs.TaskOpClose),
		"finalize must enqueue a close op for the freshly-created remote task")

	// Execute the enqueued close op: it reads the recorded external id
	// and issues item_close.
	env.mock.beforeSync = nil
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpClose))
	calls = env.mock.snapshotCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "item_close", calls[1].Type)
	assert.Equal(t, "real-race", calls[1].Args["id"])
}

// TestIntegration_OpWorker_Create_DismissedMidFlight asserts the
// dismissed branch: finalize records the id only — NO close op (the
// user deleted/unlabeled the task in Todoist; closing it would be
// wrong; legacy finalizeTempIDMappingTx rule preserved).
func TestIntegration_OpWorker_Create_DismissedMidFlight(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-dismissed"
	env.mock.beforeSync = func(cmds []todoist.SyncCommand) {
		if cmds[0].Type != "item_add" {
			return
		}
		_ = pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := env.taskRepo.UpdateContactTaskStateTx(ctx, tx, task.ID, repository.ContactTaskStateDismissed)
			return err
		})
	}

	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateDismissed, fresh.State)
	assert.Equal(t, "real-dismissed", fresh.ExternalTaskID)
	assert.Equal(t, 0, env.countOps(t, task.ID, consumerjobs.TaskOpClose),
		"dismissed finalize records the id only — no close op")
}

// --------------------------------------------------------------------------
// Refresh convergence.
// --------------------------------------------------------------------------

// TestIntegration_OpWorker_RefreshConverges drives the pending-then-
// managed convergence flow: an update_deadline op on a pending row
// snoozes (attempts-neutral); after the create finalizes, re-running
// the update op pushes the row's CURRENT due_date.
func TestIntegration_OpWorker_RefreshConverges(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-conv"

	// Refresh lands while the row is still pending: metadata advances,
	// and the update op snoozes.
	env.setDueDate(t, task.ID, "2026-07-15")
	err := env.runOp(ctx, task.ID, consumerjobs.TaskOpUpdateDeadline)
	var snooze *river.JobSnoozeError
	require.Truef(t, errors.As(err, &snooze), "update op on pending row must snooze, got %v", err)
	assert.Empty(t, env.mock.snapshotCalls(), "no push while pending")

	// Create finalizes.
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))

	// The snoozed update op re-runs and pushes the CURRENT deadline.
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpUpdateDeadline))
	calls := env.mock.snapshotCalls()
	require.Len(t, calls, 2, "item_add + item_update")
	update := calls[1]
	assert.Equal(t, "item_update", update.Type)
	assert.Equal(t, "real-conv", update.Args["id"])
	assert.Equal(t, map[string]string{"date": "2026-07-15"}, update.Args["deadline"],
		"the op pushes the row's current due_date, not a value carried at enqueue time")
}

// TestIntegration_OpWorker_AtLeastOnceConvergence proves duplicate op
// jobs are harmless: two refreshes in quick succession enqueue two
// update_deadline ops (no dedup by design); both execute; the final
// remote value equals the latest local value, and the duplicate push
// carries the same payload-fingerprint UUID as a retry would.
func TestIntegration_OpWorker_AtLeastOnceConvergence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newOpWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-alo"
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpCreate))

	// Two refreshes in quick succession: each mutation tx enqueues its
	// own op (at-least-once, no UniqueOpts to swallow either).
	env.setDueDate(t, task.ID, "2026-08-01")
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.riverClient.InsertTx(ctx, tx, consumerjobs.TodoistTaskOpArgs{ContactTaskID: task.ID, Op: consumerjobs.TaskOpUpdateDeadline}, &river.InsertOpts{MaxAttempts: 10})
		return err
	}))
	env.setDueDate(t, task.ID, "2026-09-01")
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.riverClient.InsertTx(ctx, tx, consumerjobs.TodoistTaskOpArgs{ContactTaskID: task.ID, Op: consumerjobs.TaskOpUpdateDeadline}, &river.InsertOpts{MaxAttempts: 10})
		return err
	}))
	require.Equal(t, 2, env.countOps(t, task.ID, consumerjobs.TaskOpUpdateDeadline),
		"two refreshes → two op jobs (no dedup by design)")

	// Both jobs execute. Each reads current state, so both push the
	// LATEST value.
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpUpdateDeadline))
	require.NoError(t, env.runOp(ctx, task.ID, consumerjobs.TaskOpUpdateDeadline))

	calls := env.mock.snapshotCalls()
	require.Len(t, calls, 3, "item_add + two item_updates")
	first, second := calls[1], calls[2]
	assert.Equal(t, map[string]string{"date": "2026-09-01"}, first.Args["deadline"],
		"first executed op pushes the latest local value (convergence instruction, not value carrier)")
	assert.Equal(t, map[string]string{"date": "2026-09-01"}, second.Args["deadline"],
		"duplicate op pushes the same latest value — final remote state equals latest local state")
	assert.Equal(t, first.UUID, second.UUID,
		"identical pushes carry the same payload-fingerprint UUID, so Todoist dedups the duplicate like a retry")
}
