//go:build integration_testdb

package judgment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Once a message is recorded on a thread, the user's own message closes the
// thread's "I owe a reply" obligation and a participant's own message closes
// that participant's "they owe a reply" obligation; neither closes a
// commitment, and a message on one thread closes nothing on another.
// spec: CAD-058
func TestRecordedMessagesCloseReplyObligations(t *testing.T) {
	iOwe := func(c uuid.UUID) judgment.Outcomes {
		return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}
	}
	theyOwe := func(c uuid.UUID) judgment.Outcomes {
		return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtTheyOweReply, 0.9)}}
	}
	commits := func(direction judgment.CommitmentDirection) func(uuid.UUID) judgment.Outcomes {
		return func(c uuid.UUID) judgment.Outcomes {
			return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, direction, judgment.CommitmentStateOpen, 0.9, nil)}}
		}
	}
	cases := []struct {
		name     string
		outcomes func(uuid.UUID) judgment.Outcomes
		// answer records a message after the obligation opened; it returns the
		// thread the obligation is on.
		answer func(t *testing.T, e *env, th thread, c uuid.UUID)
		open   bool
	}{
		{
			name:     "the user's outbound message on the thread, recorded as a new interaction, closes I owe a reply",
			outcomes: iOwe,
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				e.message(t, th, c, true, e.day(2))
				e.record(t, th, c, repository.InteractionDirectionOutbound, e.day(2))
			},
		},
		{
			name:     "the user's outbound message extending an existing session closes I owe a reply",
			outcomes: iOwe,
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				session := e.record(t, th, c, repository.InteractionDirectionOutbound, e.day(1).Add(-time.Hour))
				e.message(t, th, c, true, e.day(2))
				require.NoError(t, e.contacts.ExtendInteraction(e.ctx, session.ID, c, repository.InteractionDirectionOutbound, e.day(2), nil))
			},
		},
		{
			name:     "an outbound message to the same contact on another thread closes nothing",
			outcomes: iOwe,
			answer: func(t *testing.T, e *env, _ thread, c uuid.UUID) {
				other := e.thread(t)
				e.message(t, other, c, true, e.day(2))
				e.record(t, other, c, repository.InteractionDirectionOutbound, e.day(2))
			},
			open: true,
		},
		{
			name:     "the owing participant's own inbound message closes they owe a reply",
			outcomes: theyOwe,
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				e.message(t, th, c, false, e.day(2))
				e.record(t, th, c, repository.InteractionDirectionInbound, e.day(2))
			},
		},
		{
			name:     "the owed participant's inbound message leaves I owe a reply open",
			outcomes: iOwe,
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				e.message(t, th, c, false, e.day(2))
				e.record(t, th, c, repository.InteractionDirectionInbound, e.day(2))
			},
			open: true,
		},
		{
			name:     "the owing participant's own inbound message leaves their commitment open",
			outcomes: commits(judgment.CommitmentDirectionTheirs),
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				e.message(t, th, c, false, e.day(2))
				e.record(t, th, c, repository.InteractionDirectionInbound, e.day(2))
			},
			open: true,
		},
		{
			name:     "the user's own outbound message leaves the user's commitment open",
			outcomes: commits(judgment.CommitmentDirectionMine),
			answer: func(t *testing.T, e *env, th thread, c uuid.UUID) {
				e.message(t, th, c, true, e.day(2))
				e.record(t, th, c, repository.InteractionDirectionOutbound, e.day(2))
			},
			open: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, production)
			c, th := e.contact(t), e.thread(t)
			e.message(t, th, c, false, e.day(1))
			e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", tc.outcomes(c)))
			require.Len(t, e.openObligations(t, th), 1)

			tc.answer(t, e, th, c)

			all, err := e.store.Obligations(e.ctx, th.id)
			require.NoError(t, err)
			require.Len(t, all, 1)
			assert.Equal(t, tc.open, all[0].Open)
		})
	}
}

// "They owe a reply" composes with the timestamp rule's contact-wide wait: no
// thread obligation while the contact is awaited, one when not, and a "nobody
// owes" judgment on the wait's thread is a claim whose confirmation ends the
// wait without a skip, so the deferred cadence task proceeds.
func TestTheyOweReplyComposesWithTheAwaitingReplyWait(t *testing.T) {
	// awaited records the user's outreach on the thread two hours ago, which
	// starts the weekly contact's wait, and judges the thread an hour ago.
	awaited := func(t *testing.T, e *env, value judgment.BallInCourt) (uuid.UUID, thread) {
		t.Helper()
		c, th := e.contact(t, "weekly"), e.thread(t)
		sent := accelerated.GetCurrentTime().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		e.message(t, th, c, true, sent)
		e.record(t, th, c, repository.InteractionDirectionOutbound, sent)
		require.True(t, e.awaiting(t, c), "the outreach starts the wait")
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, sent.Add(time.Hour), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, value, 0.9)}}))
		return c, th
	}

	t.Run("they_owe_reply while the contact is awaited opens no obligation", func(t *testing.T) {
		// spec: CAD-059.no-obligation-while-awaiting
		e := newEnv(t, production)
		c, th := awaited(t, e, judgment.BallInCourtTheyOweReply)
		assert.Empty(t, e.openObligations(t, th))
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
	})

	t.Run("they_owe_reply while the contact is not awaited opens one obligation with a reach_out task", func(t *testing.T) {
		// spec: CAD-059.obligation-when-not-awaiting
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, true, e.day(1))
		require.False(t, e.awaiting(t, c))

		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtTheyOweReply, 0.9)}}))

		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationDebtorContact, open[0].Debtor)
		assert.Equal(t, c, open[0].ContactID)
		assert.Equal(t, "reach_out", open[0].Kind)
	})

	t.Run("nobody_owes on the thread whose outreach started the wait is a claim that leaves the wait", func(t *testing.T) {
		// spec: CAD-059.nobody-owes-on-wait-thread-is-claim
		e := newEnv(t, production)
		c, th := awaited(t, e, judgment.BallInCourtNobodyOwes)
		assert.Equal(t, judgment.OutcomeStateClaim, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).State)
		assert.True(t, e.awaiting(t, c))
	})

	t.Run("confirming that claim ends the wait without a skip and the cadence task proceeds", func(t *testing.T) {
		// spec: CAD-060
		e := newEnv(t, production, withFollowUp())
		c, th := awaited(t, e, judgment.BallInCourtNobodyOwes)
		due := accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour)
		e.setContactBy(t, c, due)
		_, err := e.tasks.FindPendingFollowUp(e.ctx, c)
		require.NoError(t, err, "the outreach opened the follow-up reminder")
		reconcile := e.reconciler(t)
		reconcile()
		require.Nil(t, e.cadenceTask(t, c), "the wait defers the cadence task")
		before := e.contactRow(t, c)

		claim := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		reviewed, err := e.store.ReviewOutcome(e.ctx, claim.ID, string(judgment.BallInCourtNobodyOwes))
		require.NoError(t, err)
		require.Equal(t, judgment.VerdictConfirmed, reviewed.Review.Verdict)

		after := e.contactRow(t, c)
		assert.False(t, cadence.IsAwaitingReply(after.LastOutreachAt, after.LastResponseAt, after.AwaitingReplyUntil, accelerated.GetCurrentTime()))
		assert.Nil(t, after.LastSkippedAt, "no skip is recorded")
		assert.Equal(t, before.ContactBy, after.ContactBy)
		assert.Equal(t, before.LastContacted, after.LastContacted)
		assert.Equal(t, before.LastOutreachAt, after.LastOutreachAt)
		assert.Equal(t, before.LastResponseAt, after.LastResponseAt)
		_, err = e.tasks.FindPendingFollowUp(e.ctx, c)
		assert.ErrorIs(t, err, db.ErrNotFound, "the live follow-up reminder is closed")
		reconcile()
		assert.NotNil(t, e.cadenceTask(t, c), "the next reconciliation creates the due cadence task")
	})
}

func (e *env) contactRow(t *testing.T, c uuid.UUID) *repository.Contact {
	t.Helper()
	contact, err := e.people.GetContact(e.ctx, c)
	require.NoError(t, err)
	return contact
}

func (e *env) awaiting(t *testing.T, c uuid.UUID) bool {
	t.Helper()
	contact := e.contactRow(t, c)
	return cadence.IsAwaitingReply(contact.LastOutreachAt, contact.LastResponseAt, contact.AwaitingReplyUntil, accelerated.GetCurrentTime())
}

// setContactBy sets the contact's due date through the cadence owner.
func (e *env) setContactBy(t *testing.T, c uuid.UUID, due time.Time) {
	t.Helper()
	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.cadence.ApplyContactByOverride(e.ctx, tx, c, &due)
	}))
}

func (e *env) cadenceTask(t *testing.T, c uuid.UUID) *repository.ContactTask {
	t.Helper()
	task, err := e.tasks.GetContactTaskByContactCadenceDue(e.ctx, c, todoist.SourceName)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return task
}

// reconciler returns a function that runs one Todoist sync, with the Todoist
// API faked as an empty account that accepts every command.
func (e *env) reconciler(t *testing.T) func() {
	t.Helper()
	bus := events.NewBus(e.database.Pool, e.jobs, repository.NewEventRepository(e.database.Queries))
	provider := todoist.NewCadenceSyncProvider(fakeTodoistAuth{}, e.tasks, e.people,
		repository.NewSyncRepository(e.database.Queries), config.TestConfig(), bus, e.cadence, e.database.Pool,
		func(string) todoist.Client { return fakeTodoist{} }, e.jobs, false)
	account := "account-" + uuid.NewString()[:8]
	state := &repository.SyncState{ID: uuid.New(), Source: todoist.SourceName, AccountID: &account, Metadata: map[string]any{
		todoist.MetadataKeyProjectID: "project", todoist.MetadataKeyLabelID: "label", todoist.MetadataKeyLabelName: "crm",
	}}
	return func() {
		_, err := provider.Sync(e.ctx, state, nil)
		require.NoError(t, err)
	}
}

type fakeTodoistAuth struct{}

func (fakeTodoistAuth) GetAccessToken(context.Context, string) (string, error) { return "token", nil }
func (fakeTodoistAuth) HasAnyAccount(context.Context) bool                     { return true }

// fakeTodoist is an empty Todoist account that maps every created task's temp
// ID to a real one.
type fakeTodoist struct{}

func (fakeTodoist) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("quick add is not used")
}

func (fakeTodoist) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	mapping := map[string]string{}
	for _, cmd := range commands {
		if cmd.TempID != "" {
			mapping[cmd.TempID] = "real-" + cmd.TempID
		}
	}
	return &todoist.SyncResponse{SyncToken: "token-" + uuid.NewString()[:8], TempIDMap: mapping}, nil
}
