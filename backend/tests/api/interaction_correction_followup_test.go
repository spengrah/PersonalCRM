//go:build integration_testdb

package api

import (
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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

// deadlinePushed reports whether Todoist accepted a deadline update of the
// external id to date.
func (f *correctionTodoist) deadlinePushed(externalID, date string) bool {
	for _, cmd := range f.accepted {
		if cmd.Type != "item_update" || cmd.Args["id"] != externalID {
			continue
		}
		if deadline, ok := cmd.Args["deadline"].(map[string]string); ok && deadline["date"] == date {
			return true
		}
	}
	return false
}

// countOp counts op in ops.
func countOp(ops []string, op string) int {
	n := 0
	for _, o := range ops {
		if o == op {
			n++
		}
	}
	return n
}

// sendCompleted records the completion of a send task (an outbound whose
// follow-up the Todoist provider suppresses) through the interaction recorder.
func (e *correctionEnv) sendCompleted(t *testing.T, contactID uuid.UUID, at time.Time) {
	t.Helper()
	payload, err := events.Marshal(events.KindTaskCompleted, events.TaskCompletedPayload{
		Version:          2,
		ContactID:        contactID,
		TaskID:           e.ref("send"),
		TaskKind:         contacttask.KindSend,
		CompletedAt:      at,
		Direction:        repository.InteractionDirectionOutbound,
		SuppressFollowUp: true,
	})
	require.NoError(t, err)
	env := &events.Envelope{ID: uuid.New(), Source: todoist.SourceName, SourceID: uuid.NewString(), Kind: events.KindTaskCompleted, Payload: payload, ObservedAt: at}
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	ix, err := e.recorder.HandleEvent(e.ctx, tx, env)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	e.finalizeRecorded(t, todoist.SourceName, ix.ID)
}

// finalizeRecorded cancels the queued consumer jobs of the interaction.recorded
// event the recorder published for interaction ix under source. The recorder
// applied the event inline; its queued jobs are no-ops on this never-started
// client, so they are finalized for teardown.
func (e *correctionEnv) finalizeRecorded(t *testing.T, source string, ix uuid.UUID) {
	t.Helper()
	recorded, err := e.events.FindEventBySource(e.ctx, source, ix.String())
	require.NoError(t, err)
	res, err := e.riverClient.JobList(e.ctx, river.NewJobListParams().States(rivertype.JobStateAvailable).First(10000))
	require.NoError(t, err)
	for _, job := range res.Jobs {
		var args struct {
			EventID uuid.UUID `json:"event_id"`
		}
		if json.Unmarshal(job.EncodedArgs, &args) == nil && args.EventID == recorded.ID {
			_, err := e.riverClient.JobCancel(e.ctx, job.ID)
			require.NoError(t, err)
		}
	}
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

	t.Run("a live follow-up another remaining outbound still warrants stays open, due by that outbound", func(t *testing.T) {
		// spec: CAD-048.warranted-follow-up-kept
		a := e.contact(t, "monthly")
		_, task := e.liveFollowUp(t, a, e.ref("call"), e.at(-24))
		later := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(30))
		updates := countOp(e.ops(t, task.ID), consumerjobs.TaskOpUpdateDeadline)

		e.remove(t, later, removalBody(service.RemovalReasonDuplicate, a))

		kept := e.task(t, task.ID)
		require.Equal(t, repository.ContactTaskStateManaged, kept.State)
		require.NotContains(t, e.ops(t, task.ID), consumerjobs.TaskOpClose)
		require.Len(t, e.followUps(t, a), 1)
		due := cadence.AwaitingReplyUntil(e.at(-24), config.TestConfig().Watchdog.DaysForCadence("monthly")).Format(todoist.DateFormat)
		require.Equal(t, due, kept.Metadata["due_date"], "the deadline follows the remaining awaited outbound")
		require.Equal(t, updates+1, countOp(e.ops(t, task.ID), consumerjobs.TaskOpUpdateDeadline))
		require.NoError(t, e.runOp(task.ID, consumerjobs.TaskOpUpdateDeadline))
		require.True(t, e.todoist.deadlinePushed(task.ExternalTaskID, due))
	})

	t.Run("removing the reply to a one-shot send opens no follow-up", func(t *testing.T) {
		// spec: CAD-048.new-follow-up-by-rules
		a := e.contact(t, "monthly")
		e.sendCompleted(t, a, e.at(20))
		reply := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(30))
		require.Empty(t, e.followUps(t, a))

		e.remove(t, reply, removalBody(service.RemovalReasonDidNotHappen, a))

		require.Empty(t, e.followUps(t, a), "a send's completion never warrants a follow-up")
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

	t.Run("after a skip ended an outbound's window, removing its later reply gives the outbound a new follow-up", func(t *testing.T) {
		// spec: CAD-048.new-follow-up-by-rules, CAD-048.completed-never-reopened
		a := e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(20))
		opened := e.followUps(t, a)
		require.Len(t, opened, 1)
		e.skip(t, a)
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, opened[0].ID).State, "the skip ended the follow-up")
		reply := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(30))

		e.remove(t, reply, removalBody(service.RemovalReasonDidNotHappen, a))

		require.True(t, e.fields(t, a).AwaitingReply, "the outbound's window is open again")
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, opened[0].ID).State, "a completed task is never reopened")
		tasks := e.followUps(t, a)
		require.Len(t, tasks, 2)
		var created *repository.ContactTask
		for i := range tasks {
			if tasks[i].ID != opened[0].ID {
				created = &tasks[i]
			}
		}
		require.NotNil(t, created)
		require.Equal(t, repository.ContactTaskStatePendingRemoteCreate, created.State)
		require.Equal(t, cadence.AwaitingReplyUntil(e.at(20), config.TestConfig().Watchdog.DaysForCadence("monthly")).Format(todoist.DateFormat),
			created.Metadata["due_date"])
		require.Contains(t, e.ops(t, created.ID), consumerjobs.TaskOpCreate)
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

func TestInteractionCorrection_CadenceTaskFollowsAtReconciliation(t *testing.T) {
	// spec: CAD-048.cadence-task-follows-at-reconciliation
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, cfg := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)
	todoistSync := newCadenceSync(e, cfg)

	a := e.contact(t, "monthly")
	e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*24))
	recent := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
	todoistSync.reconcile(t)
	due, err := e.tasks.GetContactTaskByContactCadenceDue(e.ctx, a, todoist.SourceName)
	require.NoError(t, err)
	before := e.fields(t, a)
	require.Equal(t, (*before.ContactBy)[:10], due.Metadata[todoist.MetadataKeySyncedDeadline], "the cadence task carries the next-contact date")

	e.remove(t, recent, removalBody(service.RemovalReasonDidNotHappen, a))
	after := e.fields(t, a)
	today := cadence.Today(accelerated.GetCurrentTime()).Format(todoist.DateFormat)
	require.Less(t, (*after.ContactBy)[:10], today, "the removal made the contact due")
	todoistSync.reconcile(t)

	redated, err := e.tasks.GetContactTaskByContactCadenceDue(e.ctx, a, todoist.SourceName)
	require.NoError(t, err)
	require.Equal(t, due.ID, redated.ID)
	require.Equal(t, today, redated.Metadata[todoist.MetadataKeySyncedDeadline], "re-dated to today, never a past date")
}
