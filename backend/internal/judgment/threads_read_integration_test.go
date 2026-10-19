//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func messageKeysOf(messages []handlers.ThreadMessageResponse) []string {
	keys := make([]string, len(messages))
	for i, m := range messages {
		keys[i] = m.MessageKey
	}
	return keys
}

// A thread read returns the thread's header, every message once in thread
// order, and its place in the list the filters keep.
func TestReadThread(t *testing.T) {
	t.Run("a thread's position within a venue-filtered list, with the neighbours either side", func(t *testing.T) {
		// spec: IXN-089.position-in-filtered-list
		b := newBenchEnv(t)
		c := b.contact(t)
		var mail []thread
		for day := 1; day <= 3; day++ {
			th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
			b.say(t, th, c, false, b.day(day))
			mail = append(mail, th)
		}
		chat := b.venueThread(t, repository.InteractionSourceTelegram, repository.VenueKindDM, "777", "")
		b.say(t, chat, c, false, b.day(2).Add(1))
		middle := mail[1]

		read := b.readThread(t, middle.id.String(), "venue=email")

		require.NotNil(t, read.Position)
		assert.Equal(t, 2, read.Position.Index)
		assert.Equal(t, 3, read.Position.Total, "the telegram thread is no part of the list")
		require.NotNil(t, read.Position.PreviousThreadID)
		require.NotNil(t, read.Position.NextThreadID)
		assert.Equal(t, mail[2].id.String(), *read.Position.PreviousThreadID, "newest first, so the later thread is previous")
		assert.Equal(t, mail[0].id.String(), *read.Position.NextThreadID)
		assert.Equal(t, middle.id.String(), read.Thread.ThreadID)
		assert.Equal(t, "email", read.Thread.Venue)
	})

	t.Run("the first thread has no previous and the last has no next", func(t *testing.T) {
		// spec: IXN-089.position-in-filtered-list
		b := newBenchEnv(t)
		c := b.contact(t)
		var threads []thread
		for day := 1; day <= 2; day++ {
			th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
			b.say(t, th, c, false, b.day(day))
			threads = append(threads, th)
		}

		first := b.readThread(t, threads[1].id.String(), "")
		last := b.readThread(t, threads[0].id.String(), "")

		require.NotNil(t, first.Position)
		assert.Equal(t, 1, first.Position.Index)
		assert.Nil(t, first.Position.PreviousThreadID)
		require.NotNil(t, first.Position.NextThreadID)
		assert.Equal(t, threads[0].id.String(), *first.Position.NextThreadID)
		require.NotNil(t, last.Position)
		assert.Equal(t, 2, last.Position.Index)
		assert.Nil(t, last.Position.NextThreadID)
		require.NotNil(t, last.Position.PreviousThreadID)
	})

	t.Run("given a run, the position is within the threads the run judged", func(t *testing.T) {
		// spec: IXN-089.position-in-filtered-list
		b := newBenchEnv(t)
		c := b.contact(t)
		var threads []thread
		for day := 1; day <= 3; day++ {
			th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
			b.say(t, th, c, false, b.day(day))
			threads = append(threads, th)
		}
		run, err := b.store.StartRun(b.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: prodMethod})
		require.NoError(t, err)
		for _, i := range []int{0, 2} {
			keys := b.messageKeys(t, threads[i])
			_, err := b.store.RecordJudgment(b.ctx, run, windowed(prodMethod, threads[i], b.day(i+1).Add(1), "h"+keys[0], keys[0], keys, judgment.Outcomes{}))
			require.NoError(t, err)
		}

		read := b.readThread(t, threads[0].id.String(), "run_id="+run.String())

		require.NotNil(t, read.Position)
		assert.Equal(t, 2, read.Position.Index)
		assert.Equal(t, 2, read.Position.Total, "the thread the run did not judge is no part of the list")
		require.NotNil(t, read.Position.PreviousThreadID)
		assert.Equal(t, threads[2].id.String(), *read.Position.PreviousThreadID)
		assert.Nil(t, read.Position.NextThreadID)
	})

	t.Run("a message a group chat holds once per member is returned once, in thread order, with its sender", func(t *testing.T) {
		// spec: IXN-089.messages-once-in-order, IXN-089.header
		b := newBenchEnv(t)
		alice, bea := b.contact(t), b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceWhatsApp, repository.VenueKindGroupChat, "friends@g.us", "")
		b.joined(t, th, alice, b.day(1))
		b.joined(t, th, bea, b.day(1))
		second := b.sayCopy(t, th, "m2", alice, false, b.day(2))
		b.sayCopy(t, th, "m2", bea, false, b.day(2))
		first := b.sayCopy(t, th, "m1", bea, false, b.day(1))
		own := b.sayCopy(t, th, "m3", alice, true, b.day(3))
		b.sayCopy(t, th, "m3", bea, true, b.day(3))

		read := b.readThread(t, th.id.String(), "")

		assert.Equal(t, []string{first, second, own}, messageKeysOf(read.Messages))
		assert.Equal(t, "Me", read.Messages[2].Sender)
		assert.Contains(t, []string{b.contactName(t, alice), b.contactName(t, bea)}, read.Messages[1].Sender, "a copy names its contact")
		assert.Equal(t, b.contactName(t, bea), read.Messages[0].Sender)
		assert.Equal(t, "synthetic message body", read.Messages[0].Text)
		assert.True(t, read.Thread.IsGroup)
		assert.ElementsMatch(t, []string{alice.String(), bea.String()}, participantContacts(read.Thread))
		assert.Nil(t, read.Review, "no run, no review")
	})

	t.Run("a meeting's notes are its messages", func(t *testing.T) {
		// spec: IXN-089.messages-once-in-order
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, uuid.NewString(), "")
		b.joined(t, th, c, b.day(1))
		key := b.say(t, th, c, true, b.day(1))

		read := b.readThread(t, th.id.String(), "")

		require.Len(t, read.Messages, 1)
		assert.Equal(t, key, read.Messages[0].MessageKey)
		assert.Equal(t, "Me", read.Messages[0].Sender)
		assert.Equal(t, "synthetic meeting summary", read.Messages[0].Text)
		assert.Equal(t, "calendar", read.Thread.Venue)
	})

	t.Run("an unknown thread, and an unknown run, are not found", func(t *testing.T) {
		// spec: IXN-089.unknown-thread-not-found
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
		b.say(t, th, c, false, b.day(1))

		assert.Equal(t, http.StatusNotFound, b.get("/api/v1/threads/"+uuid.NewString()).Code)
		assert.Equal(t, http.StatusNotFound, b.get("/api/v1/threads/"+th.id.String()+"?run_id="+uuid.NewString()).Code)
		assert.Equal(t, http.StatusBadRequest, b.get("/api/v1/threads/not-a-uuid").Code)
	})
}
