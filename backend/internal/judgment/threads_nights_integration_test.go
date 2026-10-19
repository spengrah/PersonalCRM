//go:build integration_testdb

package judgment_test

import (
	"testing"
	"time"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cutAfter is the cut-off just after the message of the day.
func (b *benchEnv) cutAfter(day int) time.Time { return b.day(day).Add(time.Hour) }

// conversation seeds an email thread of n messages, one a day from day 1, and
// returns it with the messages' keys in thread order.
func (b *benchEnv) conversation(t *testing.T, n int) (thread, []string) {
	t.Helper()
	th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
	return th, b.talk(t, th, n, b.contact(t))
}

// chat seeds an email thread of n messages, one a day from day 1, from the
// contacts in turn, and returns it with the messages' keys in thread order.
func (b *benchEnv) chat(t *testing.T, n int, contacts ...uuid.UUID) (thread, []string) {
	t.Helper()
	th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
	return th, b.talk(t, th, n, contacts...)
}

// With a run, a thread read places the thread's messages in the run's nights.
func TestReadThreadNights(t *testing.T) {
	t.Run("a busy chat judged on many nights with overlapping windows places every message once", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 6)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		for day := 1; day <= 6; day++ {
			b.judged(t, run, m, th, b.cutAfter(day), keys[max(0, day-3)], keys[day-1:day], nil)
		}

		review := b.reviewOf(t, th, run)

		require.Len(t, review.Nights, 6)
		var placed []string
		for i, n := range review.Nights {
			assert.Equal(t, []string{keys[i]}, nightKeys(n), "the first night whose cut-off is at or after the message holds it")
			assert.Equal(t, 1, n.NewMessageCount)
			assert.Equal(t, keys[max(0, i+1-3)], n.WindowStart)
			assert.True(t, nightMessage(t, n, keys[i]).Judged)
			placed = append(placed, nightKeys(n)...)
		}
		assert.Equal(t, keys, placed)
		assert.Empty(t, review.ContextMessages)
		assert.Empty(t, review.LaterMessages)
		for i := 1; i < len(review.Nights); i++ {
			assert.True(t, review.Nights[i-1].CutOff.Before(review.Nights[i].CutOff), "nights in cut-off order")
		}
	})

	t.Run("a night holding a message_over_limit message places it once, unjudged, among its new messages", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 3)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], nil)
		cutOff := b.cutAfter(3)
		b.judged(t, run, m, th, cutOff, keys[0], keys[1:2], nil)
		require.NoError(t, b.store.RecordUnjudged(b.ctx, run, []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: cutOff, MessageKey: keys[2], Reason: judgment.UnjudgedReasonMessageOverLimit},
		}))

		night := b.reviewOf(t, th, run).Nights[1]

		assert.Equal(t, []string{keys[1], keys[2]}, nightKeys(night))
		over := nightMessage(t, night, keys[2])
		assert.False(t, over.Judged)
		require.NotNil(t, over.Reason)
		assert.Equal(t, "message_over_limit", *over.Reason)
		assert.Equal(t, 2, night.NewMessageCount, "judged or unjudged")
	})

	t.Run("the thread's first judged cut-off, whose window spans two days, counts its whole window as new", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 2)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(2), keys[0], keys, nil)

		review := b.reviewOf(t, th, run)

		require.Len(t, review.Nights, 1)
		assert.Equal(t, keys, nightKeys(review.Nights[0]))
		assert.Equal(t, 2, review.Nights[0].NewMessageCount)
		assert.Empty(t, review.ContextMessages)
	})

	t.Run("a first night whose window starts before its new messages leaves the messages it only read in no night", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 4)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(4), keys[0], keys[2:], nil)

		review := b.reviewOf(t, th, run)

		assert.Equal(t, keys[:2], review.ContextMessages)
		require.Len(t, review.Nights, 1)
		assert.Equal(t, keys[2:], nightKeys(review.Nights[0]))
		assert.Equal(t, keys[0], review.Nights[0].WindowStart)
		assert.Equal(t, 2, review.Nights[0].NewMessageCount)
	})

	t.Run("a later night whose window starts at an earlier night's message places only its own messages", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 3)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], nil)
		b.judged(t, run, m, th, b.cutAfter(3), keys[0], keys[1:], nil)

		review := b.reviewOf(t, th, run)

		assert.Equal(t, keys[:1], nightKeys(review.Nights[0]))
		assert.Equal(t, keys[1:], nightKeys(review.Nights[1]))
		assert.Equal(t, keys[0], review.Nights[1].WindowStart, "the window reaches back to an earlier night's message")
	})

	t.Run("a cut-off the run recorded as failed leaves its messages unjudged in the next night, out of its new-message count", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 3)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], nil)
		failed := b.cutAfter(2)
		require.NoError(t, b.store.RecordIncomplete(b.ctx, run, th.id, failed, judgment.IncompleteReasonCallFailed))
		require.NoError(t, b.store.RecordUnjudged(b.ctx, run, []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: failed, MessageKey: keys[1], Reason: judgment.UnjudgedReasonCallFailed},
		}))
		b.judged(t, run, m, th, b.cutAfter(3), keys[0], keys[2:], nil)

		night := b.reviewOf(t, th, run).Nights[1]

		assert.Equal(t, keys[1:], nightKeys(night))
		skipped := nightMessage(t, night, keys[1])
		assert.False(t, skipped.Judged)
		require.NotNil(t, skipped.Reason)
		assert.Equal(t, "call_failed", *skipped.Reason)
		assert.Equal(t, 1, night.NewMessageCount, "the message new at the failed cut-off is not counted")
	})

	t.Run("messages after the run's last cut-off are returned as later messages", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 3)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], nil)

		review := b.reviewOf(t, th, run)

		assert.Equal(t, keys[1:], review.LaterMessages)
		assert.Equal(t, keys[:1], nightKeys(review.Nights[0]))
	})

	t.Run("a judgment of a version before the windows holds every message of its window up to its cut-off, none judged", func(t *testing.T) {
		// spec: IXN-089.run-nights
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 3)
		run, _ := b.run(t, judgment.RunKindHistorical, judgmentOf(prodMethod, th, b.cutAfter(3), "whole", judgment.Outcomes{}))

		review := b.reviewOf(t, th, run)

		require.Len(t, review.Nights, 1)
		night := review.Nights[0]
		assert.Equal(t, keys, nightKeys(night))
		assert.Equal(t, keys[0], night.WindowStart)
		for _, msg := range night.Messages {
			assert.False(t, msg.Judged)
			assert.Empty(t, msg.Chips)
		}
		assert.Empty(t, review.ContextMessages)
	})
}
