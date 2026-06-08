// Integration coverage for TodoistFollowUpCreateJobWorker state-
// branching. The worker's three-phase body (read → HTTP → write) mixes
// a live DB with a mocked Todoist client, so it's exercised here rather
// than in pure unit tests. Focuses on:
//
//   - Normal finalize: pending_remote_create → managed after item_add
//     succeeds.
//   - Already-managed idempotent no-op: a retry after another worker
//     finalized does nothing.
//   - Close-while-pending race, inline close succeeds: create worker
//     enters on completed row, issues item_add + item_close, persists
//     external_task_id without flipping state, no fallback close job.
//   - Close-while-pending race, state flips between phase 1 and phase 3:
//     phase 1 sees pending_remote_create, phase 3 sees completed, so
//     the worker enqueues a fallback close job because no inline close
//     was attempted.
//   - Metadata drift: due_date changes between phase 1 and phase 3; the
//     worker enqueues TodoistFollowUpRefreshJob to bring Todoist back
//     in sync after the finalize.
package tests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
// Mocks + env for the worker integration tests.
// --------------------------------------------------------------------------

// createWorkerMockTodoist is a Todoist client that records each Sync
// call and hands back deterministic temp-id-to-real-id mappings. A
// callback `beforeSync` can mutate DB state between phases so phase 3
// sees something different than phase 1.
type createWorkerMockTodoist struct {
	mu         sync.Mutex
	calls      []todoist.SyncCommand
	realIDs    map[string]string
	errOnNth   map[int]error
	callCount  int
	beforeSync func(cmds []todoist.SyncCommand)
}

func (m *createWorkerMockTodoist) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("QuickAdd not implemented in mock")
}

func (m *createWorkerMockTodoist) Sync(_ context.Context, _ string, _ []string, cmds []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, cmds...)
	m.callCount++
	n := m.callCount
	cb := m.beforeSync
	m.mu.Unlock()
	if cb != nil {
		cb(cmds)
	}
	if err, ok := m.errOnNth[n]; ok {
		return nil, err
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

type createWorkerEnv struct {
	database    *db.Database
	gen         *factory.Generator
	contactRepo *repository.ContactRepository
	taskRepo    *repository.ContactTaskRepository
	riverClient *river.Client[pgx.Tx]
	worker      *consumer.TodoistFollowUpCreateJobWorker
	mock        *createWorkerMockTodoist
}

func newCreateWorkerEnv(t *testing.T) (*createWorkerEnv, func()) {
	t.Helper()
	database, closeFn := newFollowUpIntegrationDB(t)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	taskRepo := repository.NewContactTaskRepository(database.Queries)

	workers := river.NewWorkers()
	// Register the close worker type so InsertTx of a fallback close job
	// from phase 3 succeeds; we don't Start() the client so the worker
	// never runs.
	river.AddWorker(workers, &followUpTestNoopClose{})
	river.AddWorker(workers, &followUpTestNoopRefresh{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	mock := &createWorkerMockTodoist{realIDs: map[string]string{}, errOnNth: map[int]error{}}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "proj-test", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
	}
	factory := func(string) todoist.Client { return mock }

	worker := consumer.NewTodoistFollowUpCreateJobWorker(
		consumer.FollowUpModeCutover,
		taskRepo,
		settings,
		factory,
		riverClient,
		database.Pool,
	)
	gen, _ := migrationGenerator(t)
	env := &createWorkerEnv{
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

func (e *createWorkerEnv) seedPendingTask(t *testing.T) *repository.ContactTask {
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

// --------------------------------------------------------------------------
// State-branching tests.
// --------------------------------------------------------------------------

// TestIntegration_CreateWorker_NormalFinalize asserts the happy path:
// pending_remote_create → managed after item_add succeeds.
func TestIntegration_CreateWorker_NormalFinalize(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newCreateWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-123"

	err := env.worker.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: task.ID},
	})
	require.NoError(t, err)

	require.Len(t, env.mock.calls, 1, "exactly one item_add HTTP call on normal finalize")
	assert.Equal(t, "item_add", env.mock.calls[0].Type)

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, fresh.State)
	assert.Equal(t, "real-123", fresh.ExternalTaskID)
}

// TestIntegration_CreateWorker_AlreadyManaged asserts the idempotent
// no-op path: the worker exits at phase 1 when the row is already
// managed (another worker finalized).
func TestIntegration_CreateWorker_AlreadyManaged(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newCreateWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	// Simulate a winning concurrent worker by finalizing the row first.
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.taskRepo.UpdateContactTaskExternalIDTx(ctx, tx, task.ID, "real-winner")
		return err
	}))

	err := env.worker.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: task.ID},
	})
	require.NoError(t, err)
	assert.Empty(t, env.mock.calls, "no Todoist calls on already-managed path")

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "real-winner", fresh.ExternalTaskID, "external id untouched by the no-op")
}

// TestIntegration_CreateWorker_CompletedAtStart asserts the close-while-
// pending race where the row is already completed at phase 1. Worker
// issues item_add + item_close in one invocation, persists external_id
// without flipping state back to managed, and does NOT enqueue a
// fallback close (inline close succeeded).
func TestIntegration_CreateWorker_CompletedAtStart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newCreateWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	// Flip to completed BEFORE the worker runs — simulates an inbound
	// landing while this row was still pending_remote_create.
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.taskRepo.UpdateContactTaskStateTx(ctx, tx, task.ID, repository.ContactTaskStateCompleted)
		return err
	}))

	env.mock.realIDs[task.ID.String()] = "real-race"

	err := env.worker.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: task.ID},
	})
	require.NoError(t, err)

	require.Len(t, env.mock.calls, 2, "item_add + item_close on race path")
	assert.Equal(t, "item_add", env.mock.calls[0].Type)
	assert.Equal(t, "item_close", env.mock.calls[1].Type)
	assert.Equal(t, "real-race", env.mock.calls[1].Args["id"])

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, fresh.State, "state stays completed; no flip back to managed")
	assert.Equal(t, "real-race", fresh.ExternalTaskID)

	// No fallback close job (inline close succeeded).
	n, err := env.taskRepo.CountRiverJobsByContactTask(ctx, (consumerjobs.TodoistFollowUpCloseJobArgs{}).Kind(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "inline close succeeded — no fallback close enqueued")
}

// TestIntegration_CreateWorker_StateFlipsBetweenPhases asserts the
// race where phase 1 sees pending_remote_create but phase 3 sees
// completed (inbound arrived between phase 1 and phase 3). The worker
// persists external_task_id via SetContactTaskExternalIDOnlyTx and
// enqueues a fallback TodoistFollowUpCloseJob because no inline close
// was attempted.
func TestIntegration_CreateWorker_StateFlipsBetweenPhases(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newCreateWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-flip"

	// beforeSync fires at phase 2 (after phase 1 read, before phase 3).
	// Transitioning the row to 'completed' simulates an inbound landing
	// mid-flight.
	env.mock.beforeSync = func(cmds []todoist.SyncCommand) {
		if cmds[0].Type != "item_add" {
			return
		}
		_ = pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := env.taskRepo.UpdateContactTaskStateTx(ctx, tx, task.ID, repository.ContactTaskStateCompleted)
			return err
		})
	}

	err := env.worker.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: task.ID},
	})
	require.NoError(t, err)

	require.Len(t, env.mock.calls, 1, "only item_add issued; no inline close because phase 1 saw pending_remote_create")
	assert.Equal(t, "item_add", env.mock.calls[0].Type)

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, fresh.State, "state stays completed")
	assert.Equal(t, "real-flip", fresh.ExternalTaskID, "external_task_id persisted without state flip")

	// Fallback close job IS enqueued: inline close never attempted, but
	// phase 3 saw completed, so phase 3 queues a close to keep Todoist
	// consistent.
	n, err := env.taskRepo.CountRiverJobsByContactTask(ctx, (consumerjobs.TodoistFollowUpCloseJobArgs{}).Kind(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "fallback close enqueued because phase 3 saw completed and no inline close was attempted")
}

// TestIntegration_CreateWorker_MetadataDriftEnqueuesRefresh asserts the
// drift-detection path: due_date changes between phase 1 (item_add
// uses old value) and phase 3 (metadata shows new value). Worker
// finalizes normally AND enqueues a TodoistFollowUpRefreshJob so river
// brings Todoist in sync after finalize.
func TestIntegration_CreateWorker_MetadataDriftEnqueuesRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newCreateWorkerEnv(t)
	defer cleanup()
	ctx := context.Background()

	task := env.seedPendingTask(t)
	env.mock.realIDs[task.ID.String()] = "real-drift"

	// Bump due_date between phase 1 read and phase 3 write.
	env.mock.beforeSync = func(cmds []todoist.SyncCommand) {
		if cmds[0].Type != "item_add" {
			return
		}
		newMeta := map[string]any{}
		for k, v := range task.Metadata {
			newMeta[k] = v
		}
		newMeta["due_date"] = "2026-07-15"
		_ = pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := env.taskRepo.UpdateContactTaskMetadataTx(ctx, tx, task.ID, newMeta)
			return err
		})
	}

	err := env.worker.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
		Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: task.ID},
	})
	require.NoError(t, err)

	fresh, err := env.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, fresh.State, "normal finalize on drift")
	assert.Equal(t, "real-drift", fresh.ExternalTaskID)

	// Drift refresh enqueued so Todoist's deadline catches up.
	n, err := env.taskRepo.CountRiverJobsByContactTask(ctx, (consumerjobs.TodoistFollowUpRefreshJobArgs{}).Kind(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "metadata drift should enqueue a refresh job")

	// Sanity check the local metadata got the new deadline through to
	// the finalize write (the refresh job will bring Todoist in sync).
	if dd, ok := fresh.Metadata["due_date"].(string); ok {
		parsed, perr := time.Parse("2006-01-02", dd)
		require.NoError(t, perr)
		assert.Equal(t, "2026-07-15", parsed.Format("2006-01-02"))
	}
}
