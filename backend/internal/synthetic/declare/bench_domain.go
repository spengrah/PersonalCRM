package declare

import "fmt"

// The bench (the all-interactions view) reviews judgments of these worlds'
// threads. No method runs in a test, so a browser test records the judgments
// of a world's threads through the judgment store's testing-only route.
func init() {
	// IXN-041: one contact's interactions of every kind, over two pages: a
	// group chat where each member spoke and the user answered last (the
	// store holds the user's message once per member), a meeting the subject
	// shares with a co-attendee, a call, an email, two manual entries at the
	// same instant, and older manual entries.
	Register(Declaration{Behavior: "IXN-041", Entities: listWorld()})

	// IXN-042: two contacts' email, chat and manual interactions, for the
	// contact, type and run filters.
	Register(Declaration{Behavior: "IXN-042", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		Contact("other", Methods(MethodEmail)),
		MessageInteraction("subject-email", "subject", "email", AgoDays(1)),
		LoggedInteraction("subject-manual", "subject", "manual", AgoDays(2)),
		MessageInteraction("other-email", "other", "email", AgoDays(3)),
		GroupChat("subject-chat", []string{"subject"}, AgoDays(4), Said("subject", 0)),
	}})

	// IXN-043: two email threads of one contact, for a run that judges one and
	// reports the other incomplete.
	Register(Declaration{Behavior: "IXN-043", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		MessageInteraction("judged-thread", "subject", "email", AgoDays(1)),
		MessageInteraction("incomplete-thread", "subject", "email", AgoDays(2)),
	}})

	// IXN-044: a group chat of three contacts, each of whom spoke once, so each
	// holds a row of their own; the last to speak holds the latest content.
	Register(Declaration{Behavior: "IXN-044", Entities: []Entity{
		Contact("first", Methods(MethodEmail)),
		Contact("second", Methods(MethodEmail)),
		Contact("third", Methods(MethodEmail)),
		GroupChat("group", []string{"first", "second", "third"}, AgoDays(1),
			Said("first", -30), Said("second", -20), Said("third", -10)),
	}})

	// IXN-045: a one-to-one chat whose contact spoke three times in one burst,
	// so a judgment read up to the first message has later content after it.
	Register(Declaration{Behavior: "IXN-045", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		GroupChat("chat", []string{"subject"}, AgoDays(1), Said("subject", -40), Said("subject", -30), Said("subject", -20)),
	}})

	// IXN-046: a group chat of three contacts, each of whom spoke before the
	// user answered last, a one-to-one chat, and a meeting with a note.
	Register(Declaration{Behavior: "IXN-046", Entities: proposalWorld("ixn046-note", 8)})

	// IXN-047 and IXN-048: the proposal world, plus a one-to-one chat the user
	// spoke last in (the outreach that starts a weekly contact's wait) and a
	// one-to-one chat with a contact who promised something.
	Register(Declaration{Behavior: "IXN-047", Entities: reviewWorld("ixn047-note", 9)})
	Register(Declaration{Behavior: "IXN-048", Entities: reviewWorld("ixn048-note", 10)})

	// IXN-049: two one-to-one chats, one revealing the contact's own moment
	// and one another person's.
	Register(Declaration{Behavior: "IXN-049", Entities: []Entity{
		Contact("friend", Methods(MethodEmail)),
		Contact("pal", Methods(MethodEmail)),
		GroupChat("own-moment", []string{"friend"}, AgoDays(1), Said("friend", 0)),
		GroupChat("other-moment", []string{"pal"}, AgoDays(2), Said("pal", 0)),
	}})

	// IXN-050: a contact with an email of their own and a chat shared with
	// another contact, beside the other contact's own email.
	Register(Declaration{Behavior: "IXN-050", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		Contact("other", Methods(MethodEmail)),
		MessageInteraction("subject-email", "subject", "email", AgoDays(1)),
		MessageInteraction("other-email", "other", "email", AgoDays(2)),
		GroupChat("shared-chat", []string{"subject", "other"}, AgoDays(3), Said("subject", -20), Said("other", -10), ISaid(0)),
	}})
}

func listWorld() []Entity {
	entities := []Entity{
		Contact("subject", Methods(MethodEmail, MethodPhone)),
		Contact("co-attendee", Methods(MethodEmail)),
		Contact("member-b", Methods(MethodEmail)),
		Contact("member-c", Methods(MethodEmail)),
		GroupChat("group", []string{"subject", "member-b", "member-c"}, AgoDays(1),
			Said("subject", -30), Said("member-b", -20), Said("member-c", -10), ISaid(0)),
		CalendarEvent("shared-meeting", "subject", StartedDaysAgo(2), CoAttendee("co-attendee")),
		PhoneCallInteraction("call", "subject", AgoDays(3)),
		MessageInteraction("email", "subject", "email", AgoDays(4)),
		LoggedInteraction("tie-a", "subject", "manual", AgoDays(5)),
		LoggedInteraction("tie-b", "subject", "manual", AgoDays(5)),
	}
	for i := 1; i <= 20; i++ {
		entities = append(entities, LoggedInteraction(fmt.Sprintf("older-%02d", i), "subject", "manual", AgoDays(5+i)))
	}
	return entities
}

// proposalWorld names its meeting note noteHandle and holds its meeting
// meetingDaysAgo: a linked note's handle seeds its session ID, and a note links
// to the one event or call starting in its window, so in a world composed of
// every declaration each proposal world's meeting needs a handle and a day of
// its own.
func proposalWorld(noteHandle string, meetingDaysAgo int) []Entity {
	return []Entity{
		Contact("sam", Methods(MethodEmail)),
		Contact("kim", Methods(MethodEmail)),
		Contact("lee", Methods(MethodEmail)),
		Contact("friend", Methods(MethodEmail)),
		Contact("host", Methods(MethodEmail)),
		GroupChat("group", []string{"sam", "kim", "lee"}, AgoDays(1),
			Said("sam", -30), Said("kim", -20), Said("lee", -15), ISaid(-10)),
		GroupChat("dm", []string{"friend"}, AgoDays(2), Said("friend", 0)),
		CalendarEvent("meeting", "host", StartedDaysAgo(meetingDaysAgo)),
		LinkedMeetingNote(noteHandle, "meeting"),
	}
}

func reviewWorld(noteHandle string, meetingDaysAgo int) []Entity {
	return append(proposalWorld(noteHandle, meetingDaysAgo),
		Contact("waiting", Cadence("weekly"), Methods(MethodEmail)),
		Contact("promiser", Methods(MethodEmail)),
		GroupChat("thanks", []string{"waiting"}, AgoDays(1), ISaid(0)),
		GroupChat("promise", []string{"promiser"}, AgoDays(2), Said("promiser", 0)),
	)
}
