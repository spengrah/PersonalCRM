//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func threadIDs(rows []handlers.ThreadRowResponse) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ThreadID
	}
	return ids
}

func participantContacts(row handlers.ThreadRowResponse) []string {
	ids := make([]string, len(row.Participants))
	for i, p := range row.Participants {
		ids[i] = p.ContactID
	}
	return ids
}

// group seeds a WhatsApp group chat every contact has written in once, a day
// apart from day 1, and returns it.
func (b *benchEnv) group(t *testing.T, contacts ...uuid.UUID) thread {
	t.Helper()
	th := b.venueThread(t, repository.InteractionSourceWhatsApp, repository.VenueKindGroupChat, "group-"+uuid.NewString()+"@g.us", "")
	for i, c := range contacts {
		b.say(t, th, c, false, b.day(i+1))
		b.joined(t, th, c, b.day(i+1))
	}
	return th
}

// A thread list row names the thread, its venue, its participants, its latest
// message and its message count; rows order by latest message.
func TestListThreads(t *testing.T) {
	t.Run("a group chat of four participants judged on twelve nights is one row naming all four", func(t *testing.T) {
		// spec: IXN-087.one-row-per-thread
		b := newBenchEnv(t)
		people := []uuid.UUID{b.contact(t), b.contact(t), b.contact(t), b.contact(t)}
		th := b.venueThread(t, repository.InteractionSourceWhatsApp, repository.VenueKindGroupChat, "family@g.us", "")
		for day := 1; day <= 12; day++ {
			c := people[day%len(people)]
			b.say(t, th, c, false, b.day(day))
			b.joined(t, th, c, b.day(day))
		}
		keys := b.messageKeys(t, th)
		run, err := b.store.StartRun(b.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: prodMethod})
		require.NoError(t, err)
		for day := 1; day <= 12; day++ {
			j := windowed(prodMethod, th, b.day(day).Add(time.Hour), "night-"+keys[day-1], keys[0], keys[day-1:day], judgment.Outcomes{})
			_, err := b.store.RecordJudgment(b.ctx, run, j)
			require.NoError(t, err)
		}

		list, meta := b.threadList(t, "run_id="+run.String())

		require.Len(t, list.Items, 1)
		assert.Equal(t, int64(1), meta.Total)
		row := list.Items[0]
		assert.Equal(t, th.id.String(), row.ThreadID)
		assert.Equal(t, "whatsapp", row.Venue)
		assert.Equal(t, "message", row.Type)
		assert.True(t, row.IsGroup)
		assert.Nil(t, row.Title)
		assert.ElementsMatch(t, []string{people[0].String(), people[1].String(), people[2].String(), people[3].String()}, participantContacts(row))
		for _, p := range row.Participants {
			assert.NotEmpty(t, p.Name)
		}
		assert.Equal(t, 12, row.MessageCount)
		assert.True(t, b.day(12).Equal(row.LatestAt))
		plain, _ := b.threadList(t, "")
		assert.Equal(t, threadIDs(list.Items), threadIDs(plain.Items), "the thread is one row without the run too")
	})

	t.Run("an email thread with a title", func(t *testing.T) {
		// spec: IXN-087.one-row-per-thread
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "Quarterly planning")
		b.say(t, th, c, false, b.day(1))
		b.joined(t, th, c, b.day(1))

		list, _ := b.threadList(t, "")

		require.Len(t, list.Items, 1)
		row := list.Items[0]
		require.NotNil(t, row.Title)
		assert.Equal(t, "Quarterly planning", *row.Title)
		assert.Equal(t, "email", row.Venue)
		assert.False(t, row.IsGroup)
		assert.Equal(t, []string{c.String()}, participantContacts(row))
	})

	t.Run("a meeting, counted as one message", func(t *testing.T) {
		// spec: IXN-087.one-row-per-thread
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, uuid.NewString(), "")
		b.say(t, th, c, true, b.day(1))
		b.joined(t, th, c, b.day(1))

		list, _ := b.threadList(t, "")

		require.Len(t, list.Items, 1)
		assert.Equal(t, th.id.String(), list.Items[0].ThreadID)
		assert.Equal(t, "anarlog", list.Items[0].Venue)
		assert.Equal(t, "event", list.Items[0].Type)
		assert.Equal(t, 1, list.Items[0].MessageCount)
	})

	t.Run("each type value keeps only threads of that type, each row naming its app", func(t *testing.T) {
		// spec: IXN-087.filters-combine
		b := newBenchEnv(t)
		c := b.contact(t)
		type app struct {
			th    thread
			venue string
		}
		perType := map[string][]app{
			"message": {
				{b.venueThread(t, repository.InteractionSourceWhatsApp, repository.VenueKindDM, "wa-"+uuid.NewString()+"@s.whatsapp.net", ""), "whatsapp"},
				{b.venueThread(t, repository.InteractionSourceTelegram, repository.VenueKindDM, "4242", ""), "telegram"},
				{b.venueThread(t, repository.InteractionSourceMessages, repository.VenueKindDM, "chat-"+uuid.NewString(), ""), "imessage"},
				{b.venueThread(t, repository.InteractionSourceGChat, repository.VenueKindGroupChat, "space-"+uuid.NewString(), ""), "google_chat"},
			},
			"email": {
				{b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), ""), "email"},
			},
			"event": {
				{b.venueThread(t, repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, uuid.NewString(), ""), "anarlog"},
			},
		}
		day, total := 0, 0
		for _, apps := range perType {
			for _, a := range apps {
				day++
				total++
				b.say(t, a.th, c, false, b.day(day))
			}
		}

		for typ, apps := range perType {
			list, _ := b.threadList(t, "type="+typ)
			venues := map[string]string{}
			for _, row := range list.Items {
				assert.Equal(t, typ, row.Type)
				venues[row.ThreadID] = row.Venue
			}
			want := map[string]string{}
			for _, a := range apps {
				want[a.th.id.String()] = a.venue
			}
			assert.Equal(t, want, venues, typ)
			assert.Equal(t, []handlers.InteractionTypeOptionResponse{
				{Key: "message", Label: "Messages"}, {Key: "email", Label: "Email"}, {Key: "event", Label: "Events"},
			}, list.TypeOptions)
		}
		all, _ := b.threadList(t, "")
		assert.Len(t, all.Items, total)
	})

	t.Run("a contact filter keeps only the threads that contact takes part in", func(t *testing.T) {
		// spec: IXN-087.filters-combine
		b := newBenchEnv(t)
		alice, bea := b.contact(t), b.contact(t)
		shared := b.group(t, alice, bea)
		aliceOnly := b.group(t, alice)
		b.group(t, bea)

		list, meta := b.threadList(t, "contact_id="+alice.String())

		assert.ElementsMatch(t, []string{shared.id.String(), aliceOnly.id.String()}, threadIDs(list.Items))
		assert.Equal(t, int64(2), meta.Total)
	})

	t.Run("a type filter and a contact filter combine, keeping only threads matching both", func(t *testing.T) {
		// spec: IXN-087.filters-combine
		b := newBenchEnv(t)
		alice, bea := b.contact(t), b.contact(t)
		whatsapp := b.group(t, alice, bea)
		mail := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
		b.say(t, mail, alice, false, b.day(5))
		b.joined(t, mail, alice, b.day(5))
		other := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
		b.say(t, other, bea, false, b.day(6))
		b.joined(t, other, bea, b.day(6))
		_ = whatsapp

		list, _ := b.threadList(t, "type=email&contact_id="+alice.String())

		assert.Equal(t, []string{mail.id.String()}, threadIDs(list.Items))
	})

	t.Run("rows order by latest message, newest first, and pages cover them without gaps or duplicates", func(t *testing.T) {
		// spec: IXN-087.order-and-pages
		b := newBenchEnv(t)
		c := b.contact(t)
		var ids []string
		for day := 1; day <= 5; day++ {
			th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
			b.say(t, th, c, false, b.day(day))
			ids = append([]string{th.id.String()}, ids...)
		}

		first, meta := b.threadList(t, "limit=2&page=1")
		second, _ := b.threadList(t, "limit=2&page=2")
		third, _ := b.threadList(t, "limit=2&page=3")

		assert.Equal(t, api.PaginationMeta{Page: 1, Limit: 2, Total: 5, Pages: 3}, meta)
		assert.Equal(t, ids, append(append(threadIDs(first.Items), threadIDs(second.Items)...), threadIDs(third.Items)...))
	})

	t.Run("two threads whose latest messages share a time keep one order on every read", func(t *testing.T) {
		// spec: IXN-087.order-and-pages
		b := newBenchEnv(t)
		c := b.contact(t)
		var ids []string
		for range 3 {
			th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
			b.say(t, th, c, false, b.day(1))
			ids = append(ids, th.id.String())
		}

		first, _ := b.threadList(t, "")
		again, _ := b.threadList(t, "")

		assert.Equal(t, threadIDs(first.Items), threadIDs(again.Items))
		assert.ElementsMatch(t, ids, threadIDs(first.Items))
		sorted := threadIDs(first.Items)
		assert.IsDecreasing(t, sorted, "the thread ID breaks a tie, descending")
	})

	t.Run("an unknown type, a type the Threads tab does not offer and a malformed contact_id are refused", func(t *testing.T) {
		// spec: IXN-087.bad-filter-refused
		b := newBenchEnv(t)

		assert.Equal(t, http.StatusBadRequest, b.get("/api/v1/threads?type=fax").Code)
		assert.Equal(t, http.StatusBadRequest, b.get("/api/v1/threads?type=call").Code)
		assert.Equal(t, http.StatusBadRequest, b.get("/api/v1/threads?contact_id=not-a-uuid").Code)
	})

	t.Run("a Google Chat thread is listed and read as a message thread", func(t *testing.T) {
		// spec: IXN-087.one-row-per-thread, IXN-087.filters-combine
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceGChat, repository.VenueKindGroupChat, "space-"+uuid.NewString(), "")
		b.say(t, th, c, false, b.day(1))
		b.joined(t, th, c, b.day(1))

		list, _ := b.threadList(t, "type=message")
		read := b.readThread(t, th.id.String(), "type=message")

		require.Len(t, list.Items, 1)
		assert.Equal(t, th.id.String(), list.Items[0].ThreadID)
		assert.Equal(t, "google_chat", list.Items[0].Venue)
		assert.Equal(t, "message", list.Items[0].Type)
		assert.Len(t, read.Messages, 1)
		require.NotNil(t, read.Position)
		assert.Equal(t, 1, read.Position.Total)
	})
}
