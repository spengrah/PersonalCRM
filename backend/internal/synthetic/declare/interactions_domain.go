package declare

import "fmt"

func init() {
	Register(Declaration{Behavior: "IXN-002", Entities: []Entity{
		Contact("subject", Methods(MethodEmail, MethodPhone, MethodTelegram)),
		MessageInteraction("email-row", "subject", "email", AgoDays(1)),
		MessageInteraction("gchat-row", "subject", "gchat", AgoDays(2)),
		MessageInteraction("whatsapp-row", "subject", "whatsapp", AgoDays(3)),
		MessageInteraction("telegram-row", "subject", "telegram", AgoDays(4)),
		MessageInteraction("messages-row", "subject", "messages", AgoDays(5), Burst(3)),
		CalendarEvent("gcal-row", "subject", StartedDaysAgo(6), EventLocation(), SourceLink()),
		PhoneCallInteraction("call-row", "subject", AgoDays(7)),
		LoggedInteraction("manual-row", "subject", "manual", AgoDays(8)),
		LoggedInteraction("todoist-row", "subject", "todoist", AgoDays(9)),
		LoggedInteraction("anarlog-row", "subject", "anarlog_sessions", AgoDays(10)),
	}})

	Register(Declaration{Behavior: "IXN-001", Entities: pagingInteractionEntities()})

	Register(Declaration{Behavior: "IXN-006", Entities: []Entity{
		Contact("subject", Methods(MethodEmail)),
		LoggedInteraction("past-row", "subject", "manual", AgoDays(1)),
		CalendarEvent("underway", "subject", InProgress(), EventLocation(), SourceLink()),
		CalendarEvent("future-01", "subject", StartsInDays(1)),
		CalendarEvent("future-02", "subject", StartsInDays(2)),
		CalendarEvent("future-03", "subject", StartsInDays(3), SoleAttendee()),
		CalendarEvent("future-04", "subject", StartsInDays(4)),
		CalendarEvent("future-05", "subject", StartsInDays(5)),
		CalendarEvent("future-06", "subject", StartsInDays(6)),
		CalendarEvent("future-07", "subject", StartsInDays(7)),
		CalendarEvent("future-08", "subject", StartsInDays(8)),
		CalendarEvent("future-09", "subject", StartsInDays(9)),
		CalendarEvent("future-10", "subject", StartsInDays(10)),
		CalendarEvent("future-11", "subject", StartsInDays(11)),
		CalendarEvent("future-12", "subject", StartsInDays(12)),
	}})

	Register(Declaration{Behavior: "IXN-009", Entities: []Entity{
		Contact("silent", Methods(MethodEmail)),
		Contact("dup-host", Methods(MethodEmail)),
		CalendarEvent("gcal-dup-a", "dup-host", StartedDaysAgo(2)),
		CalendarEvent("gcal-dup-b", "dup-host", StartedDaysAgo(2)),
		LoggedInteraction("manual-dup-a", "dup-host", "manual", AgoDays(3)),
		LoggedInteraction("manual-dup-b", "dup-host", "manual", AgoDays(3)),
		Contact("future-only", Methods(MethodEmail)),
		CalendarEvent("future-only-event", "future-only", StartsInDays(2)),
	}})
}

func pagingInteractionEntities() []Entity {
	entities := []Entity{Contact("subject", Methods(MethodEmail))}
	for i := 1; i <= 19; i++ {
		entities = append(entities, LoggedInteraction(fmt.Sprintf("recent-%02d", i), "subject", "manual", AgoDays(i)))
	}
	entities = append(entities,
		LoggedInteraction("tie-a", "subject", "manual", AgoDays(20)),
		LoggedInteraction("tie-b", "subject", "manual", AgoDays(20)),
	)
	for i := 1; i <= 4; i++ {
		entities = append(entities, LoggedInteraction(fmt.Sprintf("old-%d", i), "subject", "manual", AgoDays(20+i)))
	}
	return entities
}
