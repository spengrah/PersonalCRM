//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// These tests observe the follow-up effects of the correction routes through
// the contact's tasks, the enqueued Todoist op jobs, the commands Todoist (faked
// at its client boundary) accepts, and a following CadenceSyncProvider.Sync.

// followUps returns the contact's follow-up tasks.
func (e *correctionEnv) followUps(t *testing.T, contactID uuid.UUID) []repository.ContactTask {
	t.Helper()
	tasks, err := e.tasks.ListContactTasksByContact(e.ctx, contactID)
	require.NoError(t, err)
	var out []repository.ContactTask
	for _, task := range tasks {
		if task.Lifecycle == contacttask.LifecycleFollowUpLoop {
			out = append(out, task)
		}
	}
	return out
}

// task re-reads one task.
func (e *correctionEnv) task(t *testing.T, id uuid.UUID) *repository.ContactTask {
	t.Helper()
	task, err := e.tasks.GetContactTask(e.ctx, id)
	require.NoError(t, err)
	return task
}

// ops returns the verbs of the Todoist op jobs enqueued for a task.
func (e *correctionEnv) ops(t *testing.T, taskID uuid.UUID) []string {
	t.Helper()
	res, err := e.riverClient.JobList(e.ctx, river.NewJobListParams().Kinds(consumerjobs.TodoistTaskOpArgs{}.Kind()).First(10000))
	require.NoError(t, err)
	var ops []string
	for _, job := range res.Jobs {
		var args consumerjobs.TodoistTaskOpArgs
		require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
		if args.ContactTaskID == taskID {
			ops = append(ops, args.Op)
		}
	}
	return ops
}

// runOp runs one Todoist op for a task through the real op worker.
func (e *correctionEnv) runOp(taskID uuid.UUID, op string) error {
	return e.opWorker.Work(e.ctx, &river.Job[consumerjobs.TodoistTaskOpArgs]{
		Args: consumerjobs.TodoistTaskOpArgs{ContactTaskID: taskID, Op: op},
	})
}

// liveFollowUp records an outbound and finalizes the follow-up it opens in
// Todoist, returning the managed task.
func (e *correctionEnv) liveFollowUp(t *testing.T, contactID uuid.UUID, ref string, at time.Time) (uuid.UUID, *repository.ContactTask) {
	t.Helper()
	node := e.call(t, contactID, ref, repository.InteractionDirectionOutbound, at)
	tasks := e.followUps(t, contactID)
	require.Len(t, tasks, 1)
	require.NoError(t, e.runOp(tasks[0].ID, consumerjobs.TaskOpCreate))
	task := e.task(t, tasks[0].ID)
	require.Equal(t, repository.ContactTaskStateManaged, task.State)
	require.NotEmpty(t, task.ExternalTaskID)
	return node, task
}

// closedRemotely reports whether Todoist accepted a close of the external id.
func (f *correctionTodoist) closedRemotely(externalID string) bool {
	for _, cmd := range f.accepted {
		if cmd.Type == "item_close" && cmd.Args["id"] == externalID {
			return true
		}
	}
	return false
}

func TestInteractionCorrection_ReconcilesFollowUps(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("removing the outbound that opened a live follow-up closes it locally and enqueues its remote close", func(t *testing.T) {
		// spec: CAD-048.unwarranted-follow-up-closed
		a := e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		outbound, task := e.liveFollowUp(t, a, e.ref("call"), e.at(30))

		e.remove(t, outbound, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State)
		require.Contains(t, e.ops(t, task.ID), consumerjobs.TaskOpClose)
		require.NoError(t, e.runOp(task.ID, consumerjobs.TaskOpClose))
		require.True(t, e.todoist.closedRemotely(task.ExternalTaskID))
	})

	t.Run("a live follow-up another remaining outbound still warrants stays open", func(t *testing.T) {
		// spec: CAD-048.warranted-follow-up-kept
		a := e.contact(t, "monthly")
		_, task := e.liveFollowUp(t, a, e.ref("call"), e.at(20))
		later := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(30))

		e.remove(t, later, removalBody(service.RemovalReasonDuplicate, a))

		require.Equal(t, repository.ContactTaskStateManaged, e.task(t, task.ID).State)
		require.NotContains(t, e.ops(t, task.ID), consumerjobs.TaskOpClose)
		require.Len(t, e.followUps(t, a), 1)
	})

	t.Run("removing the reply that completed a follow-up keeps it completed and opens a new one for the outbound", func(t *testing.T) {
		// spec: CAD-048.completed-never-reopened, CAD-048.new-follow-up-by-rules
		a := e.contact(t, "monthly")
		_, task := e.liveFollowUp(t, a, e.ref("call"), e.at(20))
		reply := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(30))
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State)

		e.remove(t, reply, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State, "a completed task is never reopened")
		tasks := e.followUps(t, a)
		require.Len(t, tasks, 2)
		var opened *repository.ContactTask
		for i := range tasks {
			if tasks[i].ID != task.ID {
				opened = &tasks[i]
			}
		}
		require.NotNil(t, opened)
		require.Equal(t, repository.ContactTaskStatePendingRemoteCreate, opened.State)
		require.Equal(t, cadence.AwaitingReplyUntil(e.at(20), config.TestConfig().Watchdog.DaysForCadence("monthly")).Format(todoist.DateFormat),
			opened.Metadata["due_date"])
		require.Contains(t, e.ops(t, opened.ID), consumerjobs.TaskOpCreate)
	})

	t.Run("merging an outbound with its later inbound closes the follow-up the outbound opened", func(t *testing.T) {
		// spec: CAD-048.unwarranted-follow-up-closed
		a := e.contact(t, "monthly")
		inbound := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(30))
		outbound, task := e.liveFollowUp(t, a, e.ref("call"), e.at(20))

		e.merge(t, a, outbound, inbound)

		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State)
		require.Contains(t, e.ops(t, task.ID), consumerjobs.TaskOpClose)
	})

	t.Run("with Todoist failing the removal commits and closes locally, and the remote close lands once Todoist is back", func(t *testing.T) {
		// spec: CAD-048.local-close-independent-of-todoist
		a := e.contact(t, "monthly")
		outbound, task := e.liveFollowUp(t, a, e.ref("call"), e.at(30))
		e.todoist.failing = true
		defer func() { e.todoist.failing = false }()

		e.remove(t, outbound, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State)
		require.Error(t, e.runOp(task.ID, consumerjobs.TaskOpClose))
		require.False(t, e.todoist.closedRemotely(task.ExternalTaskID))
		e.todoist.failing = false
		require.NoError(t, e.runOp(task.ID, consumerjobs.TaskOpClose))
		require.True(t, e.todoist.closedRemotely(task.ExternalTaskID))
	})
}

// correctionOAuth is the Todoist OAuth token provider faked for the cadence
// sync run.
type correctionOAuth struct{}

func (correctionOAuth) GetAccessToken(context.Context, string) (string, error) { return "token", nil }
func (correctionOAuth) HasAnyAccount(context.Context) bool                     { return true }

func TestInteractionCorrection_CadenceTaskFollowsAtReconciliation(t *testing.T) {
	// spec: CAD-048.cadence-task-follows-at-reconciliation
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, cfg := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)
	provider := todoist.NewCadenceSyncProvider(correctionOAuth{}, e.tasks, e.contactRepo,
		repository.NewSyncRepositoryWithPool(database.Queries, database.Pool), cfg, e.bus, e.cadence, database.Pool,
		func(string) todoist.Client { return e.todoist }, e.riverClient, true)
	accountID := e.harness.Generator().Prefix() + "todoist"
	reconcile := func() {
		_, err := provider.Sync(e.ctx, &repository.SyncState{
			Source:    repository.InteractionSourceTodoist,
			AccountID: &accountID,
			Metadata:  map[string]any{todoist.MetadataKeyProjectID: "proj-correction", todoist.MetadataKeyLabelID: "label-correction"},
		}, nil)
		require.NoError(t, err)
	}

	a := e.contact(t, "monthly")
	e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*24))
	recent := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
	reconcile()
	due, err := e.tasks.GetContactTaskByContactCadenceDue(e.ctx, a, todoist.SourceName)
	require.NoError(t, err)
	before := e.fields(t, a)
	require.Equal(t, (*before.ContactBy)[:10], due.Metadata[todoist.MetadataKeySyncedDeadline], "the cadence task carries the next-contact date")

	e.remove(t, recent, removalBody(service.RemovalReasonDidNotHappen, a))
	after := e.fields(t, a)
	today := cadence.Today(accelerated.GetCurrentTime()).Format(todoist.DateFormat)
	require.Less(t, (*after.ContactBy)[:10], today, "the removal made the contact due")
	reconcile()

	redated, err := e.tasks.GetContactTaskByContactCadenceDue(e.ctx, a, todoist.SourceName)
	require.NoError(t, err)
	require.Equal(t, due.ID, redated.ID)
	require.Equal(t, today, redated.Metadata[todoist.MetadataKeySyncedDeadline], "re-dated to today, never a past date")
}
