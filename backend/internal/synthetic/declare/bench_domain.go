package declare

import "fmt"

// The Interactions tab (the all-interactions view) lists these worlds'
// interactions.
func init() {
	// IXN-041: one contact's interactions of every kind, over two pages: a
	// group chat where each member spoke and the user answered last (the
	// store holds the user's message once per member), a meeting the subject
	// shares with a co-attendee, a call, an email, two manual entries at the
	// same instant, and older manual entries.
	Register(Declaration{Behavior: "IXN-041", Entities: listWorld()})

	// IXN-042: two contacts' email, chat and manual interactions, for the
	// contact and type filters.
	Register(Declaration{Behavior: "IXN-042", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		Contact("other", Methods(MethodEmail)),
		MessageInteraction("subject-email", "subject", "email", AgoDays(1)),
		LoggedInteraction("subject-manual", "subject", "manual", AgoDays(2)),
		MessageInteraction("other-email", "other", "email", AgoDays(3)),
		GroupChat("subject-chat", []string{"subject"}, AgoDays(4), Said("subject", 0)),
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
