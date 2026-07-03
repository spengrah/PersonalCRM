package consumer

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
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

// settingsOK returns a TodoistSettingsFunc that always resolves.
func settingsOK() TodoistSettingsFunc {
	return func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "proj", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
	}
}

func newOpWorker(repo todoistOpTaskRepo, client *fakeOpClient, inserter RiverInserter) *TodoistTaskOpWorker {
	return newOpWorkerMode(FollowUpModeCutover, repo, client, inserter)
}

// newOpWorkerMode builds an executor with an explicit follow-up mode so the
// off-mode gate tests can exercise the lifecycle-scoped kill switch.
func newOpWorkerMode(mode string, repo todoistOpTaskRepo, client *fakeOpClient, inserter RiverInserter) *TodoistTaskOpWorker {
	return NewTodoistTaskOpWorker(mode, repo, settingsOK(), newFakeOpClientFactory(client), inserter, nil)
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
	require.Equal(t, "Follow up: contact", cmd.Args["content"])
	// The command UUID is set by executeCreate's uuidFn, not the builder —
	// TestOpWorker_UpdateDeadline_ManagedPushesCurrentValue et al. cover it.
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
	w := NewTodoistTaskOpWorker(FollowUpModeCutover, nil, nil, nil, nil, nil)
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

func TestOpWorker_UpdateDeadline_ManagedEmptyExternalID_Errors(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "", // corrupt: managed rows always carry an external id
		Metadata:       map[string]any{"due_date": "2026-03-15"},
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpUpdateDeadline))
	require.Error(t, err, "a managed row with an empty external id is corrupt — retryable error, not a snooze")
	var snooze *river.JobSnoozeError
	require.False(t, errors.As(err, &snooze), "must not snooze forever on a corrupt row")
	require.Empty(t, client.commands)
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

// -----------------------------------------------------------------------------
// close / delete verbs (step 4).
// -----------------------------------------------------------------------------

func TestOpWorker_Close_ManagedIssuesItemClose(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-7",
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
	require.NoError(t, err)
	require.Len(t, client.commands, 1)
	require.Equal(t, "item_close", client.commands[0].Type)
	require.Equal(t, "remote-7", client.commands[0].Args["id"])
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpClose, taskID, ""), client.commands[0].UUID)
}

func TestOpWorker_Close_EmptyExternalID_PendingSnoozes(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStatePendingRemoteCreate,
		ExternalTaskID: "",
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
	requireSnooze(t, err)
	require.Empty(t, client.commands)
}

func TestOpWorker_Close_EmptyExternalID_TerminalNoOp(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateCompleted,
		ExternalTaskID: "",
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
	require.NoError(t, err, "terminal row that never created a remote task → no-op")
	require.Empty(t, client.commands)
}

func TestOpWorker_Delete_IssuesItemDeleteIdempotent(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-8",
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	require.NoError(t, w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpDelete)))
	// Idempotent on repeat: a second execution issues item_delete again
	// without error (Todoist ignores a delete of an already-gone task).
	require.NoError(t, w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpDelete)))
	require.Len(t, client.commands, 2)
	require.Equal(t, "item_delete", client.commands[0].Type)
	require.Equal(t, "item_delete", client.commands[1].Type)
	require.Equal(t, "remote-8", client.commands[0].Args["id"])
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpDelete, taskID, ""), client.commands[0].UUID,
		"both delete attempts carry the same deterministic UUID (harmless retry)")
	require.Equal(t, client.commands[0].UUID, client.commands[1].UUID)
}

func TestOpWorker_TerminalOp_TodoistFailureBubblesUp(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:             taskID,
		State:          repository.ContactTaskStateManaged,
		ExternalTaskID: "remote-7",
	})
	client := &fakeOpClient{syncErr: errors.New("500 upstream")}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
	require.Error(t, err)
	require.Contains(t, err.Error(), "todoist close")
}

// -----------------------------------------------------------------------------
// create verb — phase-1/phase-2 dispatch (step 5). The phase-3 finalize
// matrix is unit-tested directly via finalizeCreate below (BeginTxFunc
// needs a live pool; the end-to-end create flow is integration-tested).
// -----------------------------------------------------------------------------

func createMetadata() map[string]any {
	return map[string]any{
		"content":     "Follow up: contact",
		"due_date":    "2026-01-01",
		"marker_json": `{"k":"v"}`,
		"project_id":  "proj",
		"label_name":  "followup",
	}
}

func TestOpWorker_Create_ManagedRowNoOp(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:       taskID,
		State:    repository.ContactTaskStateManaged,
		Metadata: createMetadata(),
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpCreate))
	require.NoError(t, err, "already-managed row → idempotent no-op")
	require.Empty(t, client.commands, "must not re-create an already-managed row")
}

func TestOpWorker_Create_MissingMetadataPermanent(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:       taskID,
		State:    repository.ContactTaskStatePendingRemoteCreate,
		Metadata: map[string]any{"content": "x"}, // no due_date
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpCreate))
	require.Error(t, err, "incomplete item_add metadata is a permanent build error")
	require.Empty(t, client.commands)
}

func TestOpWorker_Create_UnknownStateErrors(t *testing.T) {
	taskID := uuid.New()
	repo := newFakeOpTaskRepo(&repository.ContactTask{
		ID:       taskID,
		State:    repository.ContactTaskState("weird"),
		Metadata: createMetadata(),
	})
	client := &fakeOpClient{}
	w := newOpWorker(repo, client, &recordingInserter{})

	err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpCreate))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected state")
}

// -----------------------------------------------------------------------------
// create verb — phase-3 finalize dispatch matrix (step 5, fake tx).
// -----------------------------------------------------------------------------

func opCloseArgs(args []river.JobArgs) int {
	n := 0
	for _, a := range args {
		if op, ok := a.(consumerjobs.TodoistTaskOpArgs); ok && op.Op == consumerjobs.TaskOpClose {
			n++
		}
	}
	return n
}

func TestOpWorker_FinalizeCreate_Pending_FinalizesManaged(t *testing.T) {
	taskID := uuid.New()
	row := &repository.ContactTask{ID: taskID, State: repository.ContactTaskStatePendingRemoteCreate}
	repo := newFakeOpTaskRepo(row)
	inserter := &recordingInserter{}
	w := newOpWorker(repo, &fakeOpClient{}, inserter)

	err := w.finalizeCreate(context.Background(), nonNilFakeTx(), row, "real-1")
	require.NoError(t, err)
	require.Equal(t, repository.ContactTaskStateManaged, repo.rows[taskID].State)
	require.Equal(t, "real-1", repo.rows[taskID].ExternalTaskID)
	require.Empty(t, inserter.args, "pending finalize must NOT enqueue a close op")
}

func TestOpWorker_FinalizeCreate_RetiredMidFlight_RecordsIDAndEnqueuesClose(t *testing.T) {
	for _, state := range []repository.ContactTaskState{
		repository.ContactTaskStateCompleted,
		repository.ContactTaskStateSuperseded,
	} {
		t.Run(string(state), func(t *testing.T) {
			taskID := uuid.New()
			row := &repository.ContactTask{ID: taskID, State: state}
			repo := newFakeOpTaskRepo(row)
			inserter := &recordingInserter{}
			w := newOpWorker(repo, &fakeOpClient{}, inserter)

			err := w.finalizeCreate(context.Background(), nonNilFakeTx(), row, "real-2")
			require.NoError(t, err)
			require.Equal(t, "real-2", repo.rows[taskID].ExternalTaskID, "records the real id")
			require.Equal(t, state, repo.rows[taskID].State, "state is NOT flipped to managed")
			require.Equal(t, 1, opCloseArgs(inserter.args), "retired-mid-flight must enqueue exactly one close op")
		})
	}
}

func TestOpWorker_FinalizeCreate_DismissedUnmanaged_RecordsIDNoClose(t *testing.T) {
	for _, state := range []repository.ContactTaskState{
		repository.ContactTaskStateDismissed,
		repository.ContactTaskStateUnmanaged,
	} {
		t.Run(string(state), func(t *testing.T) {
			taskID := uuid.New()
			row := &repository.ContactTask{ID: taskID, State: state}
			repo := newFakeOpTaskRepo(row)
			inserter := &recordingInserter{}
			w := newOpWorker(repo, &fakeOpClient{}, inserter)

			err := w.finalizeCreate(context.Background(), nonNilFakeTx(), row, "real-3")
			require.NoError(t, err)
			require.Equal(t, "real-3", repo.rows[taskID].ExternalTaskID)
			require.Empty(t, inserter.args, "dismissed/unmanaged finalize records id only, NO close")
		})
	}
}

// -----------------------------------------------------------------------------
// Follow-up off-mode gate (lifecycle-scoped kill switch).
// -----------------------------------------------------------------------------

// TestTodoistTaskOpWorker_FollowUpOffMode_SnoozesWithoutRemoteWrite proves the
// lifecycle-scoped kill switch: when follow-up writes are off, every verb on a
// followup_loop row snoozes (attempts-neutral) before any Todoist call or local
// write. The forward-safety subtests lock the scoping so other-lifecycle ops
// cannot regress it: cadence_due rows are never gated, cutover never snoozes,
// and the legacy adapters inherit the gate through the shared executor.
func TestTodoistTaskOpWorker_FollowUpOffMode_SnoozesWithoutRemoteWrite(t *testing.T) {
	// failIfCalled builds a client that fails the (sub)test if Todoist is
	// ever hit — the gate must snooze BEFORE any remote call.
	failIfCalled := func(t *testing.T) *fakeOpClient {
		c := &fakeOpClient{}
		c.beforeSync = func() { t.Fatalf("off-mode followup_loop op must not call Todoist") }
		return c
	}

	t.Run("create pending followup_loop snoozes with no remote or local write", func(t *testing.T) {
		taskID := uuid.New()
		repo := newFakeOpTaskRepo(&repository.ContactTask{
			ID:        taskID,
			State:     repository.ContactTaskStatePendingRemoteCreate,
			Lifecycle: contacttask.LifecycleFollowUpLoop,
			Metadata:  createMetadata(),
		})
		client := failIfCalled(t)
		w := newOpWorkerMode(FollowUpModeOff, repo, client, &recordingInserter{})

		err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpCreate))
		requireSnooze(t, err)
		require.Empty(t, client.commands)
		require.Equal(t, repository.ContactTaskStatePendingRemoteCreate, repo.rows[taskID].State,
			"gate must snooze before any local write")
		require.Empty(t, repo.rows[taskID].ExternalTaskID, "gate must snooze before any local write")
	})

	for _, op := range []string{
		consumerjobs.TaskOpClose,
		consumerjobs.TaskOpDelete,
		consumerjobs.TaskOpUpdateDeadline,
		consumerjobs.TaskOpUpdateDescription,
	} {
		t.Run(op+" managed followup_loop snoozes with no remote write", func(t *testing.T) {
			taskID := uuid.New()
			repo := newFakeOpTaskRepo(&repository.ContactTask{
				ID:             taskID,
				State:          repository.ContactTaskStateManaged,
				Lifecycle:      contacttask.LifecycleFollowUpLoop,
				ExternalTaskID: "remote-1",
				Metadata:       map[string]any{"due_date": "2026-03-15", "description": "notes"},
			})
			client := failIfCalled(t)
			w := newOpWorkerMode(FollowUpModeOff, repo, client, &recordingInserter{})

			err := w.Work(context.Background(), opJob(taskID, op))
			requireSnooze(t, err)
			require.Empty(t, client.commands)
		})
	}

	t.Run("cadence_due row is never gated by the follow-up kill switch", func(t *testing.T) {
		taskID := uuid.New()
		repo := newFakeOpTaskRepo(&repository.ContactTask{
			ID:             taskID,
			State:          repository.ContactTaskStateManaged,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "remote-2",
		})
		client := &fakeOpClient{}
		w := newOpWorkerMode(FollowUpModeOff, repo, client, &recordingInserter{})

		err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
		require.NoError(t, err, "a follow-up kill switch must never suppress cadence writes")
		require.Len(t, client.commands, 1)
		require.Equal(t, "item_close", client.commands[0].Type)
	})

	t.Run("cutover mode never snoozes a followup_loop op", func(t *testing.T) {
		taskID := uuid.New()
		repo := newFakeOpTaskRepo(&repository.ContactTask{
			ID:             taskID,
			State:          repository.ContactTaskStateManaged,
			Lifecycle:      contacttask.LifecycleFollowUpLoop,
			ExternalTaskID: "remote-3",
		})
		client := &fakeOpClient{}
		w := newOpWorkerMode(FollowUpModeCutover, repo, client, &recordingInserter{})

		err := w.Work(context.Background(), opJob(taskID, consumerjobs.TaskOpClose))
		require.NoError(t, err)
		require.Len(t, client.commands, 1)
		require.Equal(t, "item_close", client.commands[0].Type)
	})

	t.Run("legacy create adapter inherits the off-mode gate", func(t *testing.T) {
		taskID := uuid.New()
		repo := newFakeOpTaskRepo(&repository.ContactTask{
			ID:        taskID,
			State:     repository.ContactTaskStatePendingRemoteCreate,
			Lifecycle: contacttask.LifecycleFollowUpLoop,
			Metadata:  createMetadata(),
		})
		client := failIfCalled(t)
		executor := newOpWorkerMode(FollowUpModeOff, repo, client, &recordingInserter{})
		adapter := NewTodoistFollowUpCreateAdapterWorker(executor)

		err := adapter.Work(context.Background(), &river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]{
			Args: consumerjobs.TodoistFollowUpCreateJobArgs{ContactTaskID: taskID},
		})
		requireSnooze(t, err)
		require.Empty(t, client.commands)
	})
}

func TestOpWorker_FinalizeCreate_ManagedNoOp(t *testing.T) {
	taskID := uuid.New()
	row := &repository.ContactTask{ID: taskID, State: repository.ContactTaskStateManaged, ExternalTaskID: "already"}
	repo := newFakeOpTaskRepo(row)
	inserter := &recordingInserter{}
	w := newOpWorker(repo, &fakeOpClient{}, inserter)

	err := w.finalizeCreate(context.Background(), nonNilFakeTx(), row, "real-4")
	require.NoError(t, err, "another worker finalized first → idempotent no-op")
	require.Equal(t, "already", repo.rows[taskID].ExternalTaskID)
	require.Empty(t, inserter.args)
}
