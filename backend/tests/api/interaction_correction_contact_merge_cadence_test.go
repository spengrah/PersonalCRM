//go:build integration_testdb

package api

import (
	"net/http"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
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

	// liveFollowUps counts the contact's follow-up tasks still open.
	liveFollowUps := func(t *testing.T, contactID uuid.UUID) int {
		n := 0
		for _, task := range e.followUps(t, contactID) {
			if task.State == repository.ContactTaskStateManaged || task.State == repository.ContactTaskStatePendingRemoteCreate {
				n++
			}
		}
		return n
	}

	t.Run("a window the merged-away contact ended with a skip stays ended", func(t *testing.T) {
		// spec: CAD-047.skip-ended-window-stays-ended
		removed, survivor := e.contact(t, "monthly"), e.contact(t, "monthly")
		thread := e.ref("thread")
		node := email(t, survivor, thread, repository.InteractionDirectionInbound, 2)
		require.Equal(t, node, email(t, removed, thread, repository.InteractionDirectionInbound, 2))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, removed, e.ref("call"), repository.InteractionDirectionOutbound, e.at(30))
		require.False(t, e.skip(t, removed).AwaitingReply, "the skip ended the window")
		require.Zero(t, liveFollowUps(t, removed))

		mergeInto(t, survivor, removed)

		require.False(t, e.fields(t, survivor).AwaitingReply)
		require.Zero(t, liveFollowUps(t, survivor))
	})

	t.Run("the survivor's skip does not end the merged-away contact's window", func(t *testing.T) {
		// spec: CAD-047.skip-ended-window-stays-ended
		removed, survivor := e.contact(t, "monthly"), e.contact(t, "monthly")
		thread := e.ref("thread")
		node := email(t, survivor, thread, repository.InteractionDirectionInbound, 2)
		require.Equal(t, node, email(t, removed, thread, repository.InteractionDirectionInbound, 2))
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, removed))
		e.call(t, survivor, e.ref("call"), repository.InteractionDirectionOutbound, e.at(10))
		e.call(t, removed, e.ref("call"), repository.InteractionDirectionOutbound, e.at(20))
		open := e.fields(t, removed)
		require.True(t, open.AwaitingReply)
		require.Equal(t, 1, liveFollowUps(t, removed))
		require.False(t, e.skip(t, survivor).AwaitingReply, "the skip ended the survivor's window")

		mergeInto(t, survivor, removed)

		got := e.fields(t, survivor)
		require.True(t, got.AwaitingReply)
		require.Equal(t, open.AwaitingReplyUntil, got.AwaitingReplyUntil)
		require.Equal(t, 1, liveFollowUps(t, survivor))
	})
}
