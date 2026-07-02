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
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Shared fakes for executor unit tests.
// -----------------------------------------------------------------------------

// fakeOpTaskRepo is an in-memory todoistOpTaskRepo. Tx methods ignore the
// (typed-nil) tx and mutate the in-memory row so verify-after-push /
// finalize assertions can read back mutations.
type fakeOpTaskRepo struct {
	rows   map[uuid.UUID]*repository.ContactTask
	getErr error
}

func newFakeOpTaskRepo(tasks ...*repository.ContactTask) *fakeOpTaskRepo {
	r := &fakeOpTaskRepo{rows: map[uuid.UUID]*repository.ContactTask{}}
	for _, t := range tasks {
		r.rows[t.ID] = t
	}
	return r
}

func (r *fakeOpTaskRepo) GetContactTask(_ context.Context, id uuid.UUID) (*repository.ContactTask, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	row, ok := r.rows[id]
	if !ok {
		return nil, errors.New("fakeOpTaskRepo: row not found")
	}
	// Return a shallow copy so callers can't mutate the stored row directly.
	cp := *row
	return &cp, nil
}

func (r *fakeOpTaskRepo) GetContactTaskTx(ctx context.Context, _ pgx.Tx, id uuid.UUID) (*repository.ContactTask, error) {
	return r.GetContactTask(ctx, id)
}

func (r *fakeOpTaskRepo) UpdateContactTaskExternalIDTx(_ context.Context, _ pgx.Tx, id uuid.UUID, externalTaskID string) (*repository.ContactTask, error) {
	row, ok := r.rows[id]
	if !ok {
		return nil, errors.New("fakeOpTaskRepo: row not found")
	}
	row.State = repository.ContactTaskStateManaged
	row.ExternalTaskID = externalTaskID
	cp := *row
	return &cp, nil
}

func (r *fakeOpTaskRepo) SetContactTaskExternalIDOnlyTx(_ context.Context, _ pgx.Tx, id uuid.UUID, externalTaskID string) error {
	row, ok := r.rows[id]
	if !ok {
		return errors.New("fakeOpTaskRepo: row not found")
	}
	row.ExternalTaskID = externalTaskID
	return nil
}

// setValue mutates a stored row's metadata key (used by the in-flight
// hook to simulate a concurrent local write during an HTTP call).
func (r *fakeOpTaskRepo) setMetadata(id uuid.UUID, key, value string) {
	row := r.rows[id]
	if row.Metadata == nil {
		row.Metadata = map[string]any{}
	}
	row.Metadata[key] = value
}

// fakeOpClient records Sync commands and optionally runs a hook before
// returning (to simulate a concurrent local write landing while the HTTP
// call is "in flight").
type fakeOpClient struct {
	commands   []todoist.SyncCommand
	syncErr    error
	beforeSync func()
	realIDs    map[string]string
}

func (c *fakeOpClient) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("fakeOpClient.QuickAdd: not used")
}

func (c *fakeOpClient) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	if c.beforeSync != nil {
		c.beforeSync()
	}
	c.commands = append(c.commands, commands...)
	if c.syncErr != nil {
		return nil, c.syncErr
	}
	tempMap := map[string]string{}
	for _, cmd := range commands {
		if cmd.Type == "item_add" {
			real := c.realIDs[cmd.TempID]
			if real == "" {
				real = "real-" + cmd.TempID
			}
			tempMap[cmd.TempID] = real
		}
	}
	return &todoist.SyncResponse{TempIDMap: tempMap}, nil
}

func newFakeOpClientFactory(c *fakeOpClient) todoist.ClientFactory {
	return func(string) todoist.Client { return c }
}

// recordingInserter records InsertTx args for finalize-enqueue assertions.
type recordingInserter struct {
	args []river.JobArgs
}

func (i *recordingInserter) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	i.args = append(i.args, args)
	return &rivertype.JobInsertResult{}, nil
}

func requireSnooze(t *testing.T, err error) {
	t.Helper()
	var snooze *river.JobSnoozeError
	require.Truef(t, errors.As(err, &snooze), "expected *river.JobSnoozeError, got %v", err)
}

func newOpWorker(repo todoistOpTaskRepo, client *fakeOpClient, inserter RiverInserter) *TodoistTaskOpWorker {
	return NewTodoistTaskOpWorker(repo, settingsOK(), newFakeOpClientFactory(client), inserter, nil)
}

func opJob(taskID uuid.UUID, op string) *river.Job[consumerjobs.TodoistTaskOpArgs] {
	return &river.Job[consumerjobs.TodoistTaskOpArgs]{
		Args: consumerjobs.TodoistTaskOpArgs{ContactTaskID: taskID, Op: op},
	}
}

// -----------------------------------------------------------------------------
// Derivation helpers (step 2).
// -----------------------------------------------------------------------------

func TestTaskOpCommandUUID_DeterministicAndDistinct(t *testing.T) {
	taskA := uuid.New()
	taskB := uuid.New()

	// Same (op, taskID, fingerprint) → same UUID.
	require.Equal(t,
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
	)

	// Different fingerprint (two deadlines for the SAME row) → different UUID.
	// This is the round-1 review defect class: reusing one UUID across
	// different pushed values would let Todoist dedup the second away.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-02-01"),
	)

	// Different op → different UUID.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskA, ""),
		taskOpCommandUUID(consumerjobs.TaskOpClose, taskA, ""),
	)

	// Different task → different UUID.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskA, ""),
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskB, ""),
	)
}

func TestDescriptionFingerprint_DeterministicPerValue(t *testing.T) {
	require.Equal(t, descriptionFingerprint("hello world"), descriptionFingerprint("hello world"))
	require.NotEqual(t, descriptionFingerprint("hello world"), descriptionFingerprint("hello worlx"))
}

func TestBuildItemAddFromMetadata_TempIDIsRowID(t *testing.T) {
	taskID := uuid.New()
	task := &repository.ContactTask{
		ID: taskID,
		Metadata: map[string]any{
			"content":     "Follow up: contact",
			"due_date":    "2026-01-01",
			"marker_json": `{"k":"v"}`,
			"project_id":  "proj",
			"label_name":  "followup",
		},
	}
	cmd, err := buildItemAddFromMetadata(task)
	require.NoError(t, err)
	require.Equal(t, "item_add", cmd.Type)
	require.Equal(t, taskID.String(), cmd.TempID, "temp_id must be the row id for server-side create dedup")
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpCreate, taskID, ""), cmd.UUID)
	require.Equal(t, "Follow up: contact", cmd.Args["content"])
}

func TestBuildItemAddFromMetadata_MissingMetadataErrors(t *testing.T) {
	taskID := uuid.New()

	_, err := buildItemAddFromMetadata(&repository.ContactTask{ID: taskID})
	require.Error(t, err, "nil metadata is a permanent error")

	_, err = buildItemAddFromMetadata(&repository.ContactTask{
		ID:       taskID,
		Metadata: map[string]any{"content": "x"}, // no due_date
	})
	require.Error(t, err, "missing due_date is a permanent error")
}

// -----------------------------------------------------------------------------
// Work dispatch (step 3).
// -----------------------------------------------------------------------------

func TestOpWorker_UnknownVerb_PermanentCancel(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{ID: taskID, State: repository.ContactTaskStateManaged})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, "bogus_verb"))
	require.Error(t, err)
	var cancel *river.JobCancelError
	require.Truef(t, errors.As(err, &cancel), "unknown verb must be a permanent JobCancel, got %v", err)
	require.Empty(t, client.commands, "unknown verb must not touch Todoist")
}

func TestOpWorker_MissingDependencies_Errors(t *testing.T) {
	w := NewTodoistTaskOpWorker(nil, nil, nil, nil, nil)
	err := w.Work(context.Background(), opJob(uuid.New(), consumerjobs.TaskOpUpdateDeadline))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")
}

// -----------------------------------------------------------------------------
// update_deadline / update_description verbs (step 3).
// -----------------------------------------------------------------------------

func TestOpWorker_UpdateDeadline_PendingRowSnoozes(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:       taskID,
		State:    repository.ContactTaskStatePendingRemoteCreate,
		Metadata: map[string]any{"due_date": "2026-01-01"},
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
	requireSnooze(t, err)
	require.Empty(t, client.commands, "pending row must not push before the create finalizes")
}

func TestOpWorker_UpdateDeadline_TerminalRowsNoOp(t *testing.T) {
	for _, state := range []repository.ContactTaskState{
		repository.ContactTaskStateCompleted,
		repository.ContactTaskStateDismissed,
		repository.ContactTaskStateSuperseded,
		repository.ContactTaskStateUnmanaged,
	} {
		t.Run(string(state), func(t *testing.T) {
			taskID := uuid.New()
			repo := newFakeOpTaskRepo(&repository.ContactTask{
				ID:             taskID,
				State:          state,
				ExternalTaskID: "remote-1", // even WITH an external id, no touch
				Metadata:       map[string]any{"due_date": "2026-01-01"},
			})
			client := &fakeOpClient{}
			w := newOpWorker(repo, client, &recordingInserter{})

			err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
			require.NoError(t, err, "terminal update must no-op (close/delete owns terminal convergence)")
			require.Empty(t, client.commands, "a stale update must never touch a retired task")
		})
	}
}

func TestOpWorker_UpdateDeadline_ManagedPushesCurrentValue(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-42",
		// Anti-drift: metadata already carries the value updated AFTER
		// enqueue; the executor reads current state, so this value wins.
		Metadata: map[string]any{"due_date": "2026-03-15"},
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
	require.NoError(t, err)
	require.Len(t, client.commands, 1)
	cmd := client.commands[0]
	require.Equal(t, "item_update", cmd.Type)
	require.Equal(t, "remote-42", cmd.Args["id"])
	require.Equal(t, map[string]string{"date": "2026-03-15"}, cmd.Args["deadline"])
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskID, "2026-03-15"), cmd.UUID,
		"command UUID must carry the pushed-value fingerprint")
}

func TestOpWorker_UpdateDeadline_VerifyAfterPush_StaleSnoozes(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-42",
		Metadata:       map[string]any{"due_date": "2026-03-15"},
	})
	client := &fakeOpClient{}
	// Simulate a concurrent local write landing while the HTTP call is in
	// flight: the value we pushed (2026-03-15) is no longer current.
	client.beforeSync = func() { repo.setMetadata(taskID, "due_date", "2026-04-01") }
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
	requireSnooze(t, err)
	require.Len(t, client.commands, 1, "the push still happened; the snooze re-runs to push the newer value")
}

func TestOpWorker_UpdateDeadline_VerifyAfterPush_UnchangedCompletes(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-42",
		Metadata:       map[string]any{"due_date": "2026-03-15"},
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
	require.NoError(t, err, "an unchanged value completes normally")
}

func TestOpWorker_UpdateDescription_ManagedPushesDescription(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-99",
		Metadata:       map[string]any{"description": "notes\n\n---\n{\"k\":\"v\"}"},
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDescription))
	require.NoError(t, err)
	require.Len(t, client.commands, 1)
	cmd := client.commands[0]
	require.Equal(t, "item_update", cmd.Type)
	require.Equal(t, "notes\n\n---\n{\"k\":\"v\"}", cmd.Args["description"])
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpUpdateDescription, taskID,
		descriptionFingerprint("notes\n\n---\n{\"k\":\"v\"}")), cmd.UUID)
}

func TestOpWorker_UpdateDescription_MissingKeyPermanent(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-99",
		Metadata:       map[string]any{}, // no description key
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDescription))
	require.Error(t, err)
	var cancel *river.JobCancelError
	require.Truef(t, errors.As(err, &cancel), "missing description key must be a permanent JobCancel, got %v", err)
	require.Empty(t, client.commands)
}
