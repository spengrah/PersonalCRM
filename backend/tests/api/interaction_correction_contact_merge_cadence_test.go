//go:build integration_testdb

package api

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe a removal made through POST /api/v1/interactions/:id/removals
// across a later contact merge through POST /api/v1/contacts/:id/merge, when
// the other contact holds its own observation of the removed interaction. The
// merge makes the removal cover that observation too, so the survivor's
// effective participation (arc seam S4) and its cadence fields through
// GET /api/v1/contacts/:id are those of a contact on which the observation was
// never recorded.

func TestInteractionRemoval_SurvivesContactMergeIntoObservedContact(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// control holds only the participation the survivor keeps.
	control := func(t *testing.T) uuid.UUID {
		c := e.contact(t, "monthly")
		e.call(t, c, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		return c
	}
	// email records a contact's email of a thread-day under a ref of its own;
	// every contact's email of one thread-day shares a node.
	email := func(t *testing.T, contactID uuid.UUID, thread, direction string, h int) uuid.UUID {
		ref := uuid.NewString() + ":" + thread + ":" + e.at(h).Format("2006-01-02")
		ix := e.record(t, contactID, repository.InteractionSourceEmail, ref, direction, e.at(h))
		require.NotNil(t, ix.InteractionNodeID)
		return *ix.InteractionNodeID
	}
	mergeInto := func(t *testing.T, target, source uuid.UUID) {
		w := e.post(t, "/api/v1/contacts/"+target.String()+"/merge", map[string]any{"source_contact_id": source.String()})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	t.Run("the removed contact merged into a contact observed on the interaction", func(t *testing.T) {
		// spec: IXN-018.survives-contact-merge
		removed, survivor, want := e.contact(t, "monthly"), e.contact(t, "monthly"), control(t)
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		thread := e.ref("thread")
		node := email(t, survivor, thread, repository.InteractionDirectionInbound, 2)
		require.Equal(t, node, email(t, removed, thread, repository.InteractionDirectionInbound, 2))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))

		mergeInto(t, survivor, removed)

		entries := e.correctable(t, survivor)
		require.Len(t, entries, 1)
		require.NotEqual(t, node, *entries[0].InteractionNodeID)
		require.Equal(t, e.fields(t, want), e.fields(t, survivor))
	})

	t.Run("a contact observed on the interaction merged into the removed contact", func(t *testing.T) {
		// spec: IXN-018.survives-contact-merge
		removed, source, want := e.contact(t, "monthly"), e.contact(t, "monthly"), control(t)
		e.call(t, removed, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		thread := e.ref("thread")
		node := email(t, removed, thread, repository.InteractionDirectionInbound, 2)
		require.Equal(t, node, email(t, source, thread, repository.InteractionDirectionInbound, 2))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))

		mergeInto(t, removed, source)

		entries := e.correctable(t, removed)
		require.Len(t, entries, 1)
		require.NotEqual(t, node, *entries[0].InteractionNodeID)
		require.Equal(t, e.fields(t, want), e.fields(t, removed))
	})

	t.Run("the survivor's follow-up for an outbound the merge excludes is closed", func(t *testing.T) {
		// spec: CAD-048.unwarranted-follow-up-closed
		removed, survivor := e.contact(t, "monthly"), e.contact(t, "monthly")
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		thread := e.ref("thread")
		node := email(t, survivor, thread, repository.InteractionDirectionOutbound, 30)
		tasks := e.followUps(t, survivor)
		require.Len(t, tasks, 1)
		require.NoError(t, e.runOp(tasks[0].ID, consumerjobs.TaskOpCreate))
		task := e.task(t, tasks[0].ID)
		require.Equal(t, repository.ContactTaskStateManaged, task.State)
		require.Equal(t, node, email(t, removed, thread, repository.InteractionDirectionInbound, 30))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))

		mergeInto(t, survivor, removed)

		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, task.ID).State)
		require.Contains(t, e.ops(t, task.ID), consumerjobs.TaskOpClose)
	})

	// live returns the contact's follow-up tasks still open.
	live := func(t *testing.T, contactID uuid.UUID) []repository.ContactTask {
		var out []repository.ContactTask
		for _, task := range e.followUps(t, contactID) {
			if task.State == repository.ContactTaskStateManaged || task.State == repository.ContactTaskStatePendingRemoteCreate {
				out = append(out, task)
			}
		}
		return out
	}
	// removedFromLaterReply records an inbound Y on a thread-day for removed and
	// survivor, removes removed from it, and records removed's earlier outbound
	// X, which the removal leaves awaiting a reply.
	removedFromLaterReply := func(t *testing.T) (removed, survivor uuid.UUID) {
		removed, survivor = e.contact(t, "monthly"), e.contact(t, "monthly")
		thread := e.ref("thread")
		node := email(t, removed, thread, repository.InteractionDirectionInbound, 30)
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, removed, e.ref("call"), repository.InteractionDirectionOutbound, e.at(20))
		require.Equal(t, node, email(t, survivor, thread, repository.InteractionDirectionInbound, 30))
		return removed, survivor
	}

	t.Run("an outbound the merge leaves awaiting a reply gets a new follow-up when the merged-away contact's skip ended its own", func(t *testing.T) {
		// spec: CAD-048.new-follow-up-by-rules, CAD-048.completed-never-reopened
		removed, survivor := removedFromLaterReply(t)
		skipped := live(t, removed)
		require.Len(t, skipped, 1)
		e.skip(t, removed)
		require.Empty(t, live(t, removed), "the skip ended the follow-up")
		require.False(t, e.fields(t, survivor).AwaitingReply)

		mergeInto(t, survivor, removed)

		require.True(t, e.fields(t, survivor).AwaitingReply)
		created := live(t, survivor)
		require.Len(t, created, 1)
		require.NotEqual(t, skipped[0].ID, created[0].ID)
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, skipped[0].ID).State)
	})

	t.Run("the survivor's retained follow-up follows the outbound the merge leaves awaiting a reply", func(t *testing.T) {
		// spec: CAD-048.warranted-follow-up-kept
		removed, survivor := removedFromLaterReply(t)
		awaitingX := live(t, removed)
		require.Len(t, awaitingX, 1)
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionOutbound, e.at(-24))
		awaitingZ := live(t, survivor)
		require.Len(t, awaitingZ, 1)
		require.NotEqual(t, awaitingX[0].Metadata["due_date"], awaitingZ[0].Metadata["due_date"])

		mergeInto(t, survivor, removed)

		retained := live(t, survivor)
		require.Len(t, retained, 1)
		require.Equal(t, awaitingZ[0].ID, retained[0].ID, "the survivor's own follow-up stays open")
		require.Equal(t, awaitingX[0].Metadata["due_date"], retained[0].Metadata["due_date"])
	})

	// removedFromEarlierReply records an inbound Y on a thread-day for removed
	// and survivor, removes removed from it, and records removed's later
	// outbound X. Both contacts' participation together awaited X before the
	// merge, as the survivor's alone does after it.
	removedFromEarlierReply := func(t *testing.T) (removed, survivor uuid.UUID) {
		removed, survivor = e.contact(t, "monthly"), e.contact(t, "monthly")
		thread := e.ref("thread")
		node := email(t, removed, thread, repository.InteractionDirectionInbound, -48)
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, removed, e.ref("call"), repository.InteractionDirectionOutbound, e.at(20))
		require.Equal(t, node, email(t, survivor, thread, repository.InteractionDirectionInbound, -48))
		return removed, survivor
	}

	t.Run("the survivor's own older follow-up follows the merged-away contact's later outbound both awaited", func(t *testing.T) {
		// spec: CAD-048.warranted-follow-up-kept
		removed, survivor := removedFromEarlierReply(t)
		awaitingX := live(t, removed)
		require.Len(t, awaitingX, 1)
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionOutbound, e.at(-24))
		awaitingZ := live(t, survivor)
		require.Len(t, awaitingZ, 1)
		require.NotEqual(t, awaitingX[0].Metadata["due_date"], awaitingZ[0].Metadata["due_date"])

		mergeInto(t, survivor, removed)

		retained := live(t, survivor)
		require.Len(t, retained, 1)
		require.Equal(t, awaitingZ[0].ID, retained[0].ID, "the survivor's own follow-up stays open")
		require.Equal(t, awaitingX[0].Metadata["due_date"], retained[0].Metadata["due_date"])
	})

	t.Run("an outbound both awaited gets a new follow-up when the merged-away contact's skip ended its own and the survivor had none", func(t *testing.T) {
		// spec: CAD-048.new-follow-up-by-rules, CAD-048.completed-never-reopened
		removed, survivor := removedFromEarlierReply(t)
		skipped := live(t, removed)
		require.Len(t, skipped, 1)
		e.skip(t, removed)
		require.Empty(t, live(t, removed), "the skip ended the follow-up")
		require.Empty(t, live(t, survivor))

		mergeInto(t, survivor, removed)

		require.True(t, e.fields(t, survivor).AwaitingReply)
		created := live(t, survivor)
		require.Len(t, created, 1)
		require.NotEqual(t, skipped[0].ID, created[0].ID)
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, skipped[0].ID).State)
	})

	t.Run("the survivor's outbound gets a new follow-up when its own skip ended the follow-up and the merge excludes its older reply", func(t *testing.T) {
		// spec: CAD-048.new-follow-up-by-rules, CAD-048.completed-never-reopened
		removed, survivor := e.contact(t, "monthly"), e.contact(t, "monthly")
		thread := e.ref("thread")
		node := email(t, survivor, thread, repository.InteractionDirectionInbound, -48)
		require.Equal(t, node, email(t, removed, thread, repository.InteractionDirectionInbound, -48))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionOutbound, e.at(20))
		skipped := e.followUps(t, survivor)
		require.Len(t, skipped, 1)
		e.skip(t, survivor)
		require.Empty(t, live(t, survivor), "the skip ended the follow-up")
		require.False(t, e.fields(t, survivor).AwaitingReply)

		mergeInto(t, survivor, removed)

		require.True(t, e.fields(t, survivor).AwaitingReply)
		created := live(t, survivor)
		require.Len(t, created, 1)
		require.NotEqual(t, skipped[0].ID, created[0].ID)
		require.Equal(t, repository.ContactTaskStateCompleted, e.task(t, skipped[0].ID).State)
	})
}

// TestInteractionRemoval_ContactMergeComposesWithDueDatesSetDirectly makes a
// Todoist deadline edit through the real cadence sync, which reconciles every
// contact, so it runs on a database it owns.
func TestInteractionRemoval_ContactMergeComposesWithDueDatesSetDirectly(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, cfg := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)
	todoistSync := newCadenceSync(e, cfg)

	t.Run("an earlier Todoist deadline stays when a merge-transferred removal leaves last_contacted unchanged", func(t *testing.T) {
		// spec: CAD-047.due-date-follows-last-contacted, IXN-018.survives-contact-merge
		// removed has no cadence, so it has no cadence task for the merge to close.
		removed, survivor := e.contact(t, ""), e.contact(t, "monthly")
		thread := e.ref("thread")
		ref := func() string { return uuid.NewString() + ":" + thread + ":" + e.at(-20).Format("2006-01-02") }
		node := e.record(t, survivor, repository.InteractionSourceEmail, ref(), repository.InteractionDirectionInbound, e.at(-20)).InteractionNodeID
		require.Equal(t, node, e.record(t, removed, repository.InteractionSourceEmail, ref(), repository.InteractionDirectionInbound, e.at(-20)).InteractionNodeID)
		e.remove(t, *node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
		derived, err := time.Parse(todoist.DateFormat, (*e.fields(t, survivor).ContactBy)[:10])
		require.NoError(t, err)
		todoistSync.editDeadline(t, survivor, derived.AddDate(0, 0, -5))
		before := e.fields(t, survivor)

		w := e.post(t, "/api/v1/contacts/"+survivor.String()+"/merge", map[string]any{"source_contact_id": removed.String()})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		require.Equal(t, before, e.fields(t, survivor))
	})
}
