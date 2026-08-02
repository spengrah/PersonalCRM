package declare

import "fmt"

// Calendar-domain resolutions (spec/calendar.yaml).
//
// Every fixture here is written by the REAL calendar sync provider through the
// fake-fetcher seam, so a declared meeting is only ever a row a sync could have
// stored. That is the whole difference from the bespoke seeding this replaces: a
// direct repository write could express an attendee count of zero, a blank
// location, or a title the provider has no path to — states the product cannot
// reach and therefore states no assertion over them means anything.
//
// CAL-029 and CAL-030 are resolved as no-fixture rather than declared: their
// citing tests replace the endpoints under test with route mocks, so a
// declaration there would provision rows no assertion would ever read.
func init() {
	RegisterNone("CAL-029", "both citing tests fulfil /auth/google/accounts, /sync/status and /sync/gcal/trigger with page.route; settings-calendar.spec.ts imports only fulfill-json and issues no product write")
	RegisterNone("CAL-030", "both citing tests fulfil /sync/staleness alongside /auth/google/accounts and /sync/status with page.route; the banner is read straight off those payloads")

	// A contact with NO email at all, plus a stored meeting whose only attendee is
	// an address nobody owns. Adding that address to the contact is what the
	// rematch flow does, and the event joining the contact's meetings is its
	// observable. The contact must start method-less: an unmatched calendar
	// candidate holding an email a contact already has cannot exist, because the
	// rematch handler claims such a row the moment the contact gains it.
	Register(Declaration{
		Behavior: "CAL-019",
		Entities: []Entity{
			Contact("target", NoMethods()),
			UnmatchedCalendarEvent("stored", StartedDaysAgo(3)),
		},
	})

	// TWO contacts, one with a meeting and one with none. The second is what makes
	// the absence claim falsifiable at all: an assertion that no Meetings section
	// renders passes just as well against a broken page as against a correct one,
	// unless the same fixture also proves the section DOES render for a contact
	// that has events.
	Register(Declaration{
		Behavior: "CAL-024",
		Entities: []Entity{
			Contact("with-event", Methods(MethodEmail)),
			CalendarEvent("only", "with-event", StartsInDays(3)),
			Contact("bare", Methods(MethodEmail)),
		},
	})

	// One SUPERSET serving all four filter tests: two upcoming meetings, three past
	// ones at distinct ages, and one straddling now. Four tests cite this behavior
	// with four different count assertions, and a behavior gets one fixture — so the
	// counts are read against the superset rather than each test carrying its own
	// shape.
	//
	// The past three are declared oldest-first-then-shuffled (10, 1, 5) while the
	// list renders most-recent-first, so the ordering assertion proves the sort
	// instead of echoing insertion order.
	//
	// The straddling meeting is the boundary case: its START is in the past and its
	// END is not, so a component classifying on start time would call it past. It is
	// also why the citing test freezes the app clock to the anchor the seed
	// returns — an unfrozen clock walks past its end mid-test.
	Register(Declaration{
		Behavior: "CAL-025",
		Entities: []Entity{
			Contact("attendee", Methods(MethodEmail)),
			CalendarEvent("upcoming-near", "attendee", StartsInDays(3)),
			CalendarEvent("upcoming-far", "attendee", StartsInDays(10)),
			CalendarEvent("past-oldest", "attendee", StartedDaysAgo(10)),
			CalendarEvent("past-recent", "attendee", StartedDaysAgo(1)),
			CalendarEvent("past-middle", "attendee", StartedDaysAgo(5)),
			CalendarEvent("in-progress", "attendee", InProgress()),
		},
	})

	// The three card shapes: one carrying a location and the default two attendees,
	// one carrying neither, and one with no title.
	//
	// The bare meeting is SoleAttendee — the only shape that stores an attendee count
	// of one, because the provider stores every attendee including the account, so a
	// default meeting always has two and its count row always renders. Without it
	// the "count only when more than one attendee" claim has no negative case that
	// the product can actually produce.
	Register(Declaration{
		Behavior: "CAL-026",
		Entities: []Entity{
			Contact("attendee", Methods(MethodEmail)),
			CalendarEvent("detailed", "attendee", StartsInDays(3), EventLocation()),
			CalendarEvent("bare", "attendee", StartsInDays(4), SoleAttendee()),
			CalendarEvent("untitled", "attendee", StartsInDays(5), Untitled()),
		},
	})

	// One meeting with a source link and one without — the two branches of the
	// title-as-anchor rendering. The link is read back from the events API, never
	// restated as a literal.
	Register(Declaration{
		Behavior: "CAL-027",
		Entities: []Entity{
			Contact("attendee", Methods(MethodEmail)),
			CalendarEvent("linked", "attendee", StartsInDays(3), SourceLink()),
			CalendarEvent("plain", "attendee", StartsInDays(4)),
		},
	})

	// A list longer than the initial page, so the reveal control has something to
	// reveal and a remainder to report.
	Register(Declaration{Behavior: "CAL-028", Entities: calendarRevealFixture()})
}

// calendarRevealFixtureSize is five past the initial ten-card page, so the
// remainder the control reports is non-zero and ONE activation exhausts the list.
const calendarRevealFixtureSize = 15

// calendarRevealFixture is the progressive-reveal holding: one contact and a
// pageful-and-a-half of meetings.
//
// Every meeting is UPCOMING, and that is a cost decision as much as a shape one.
// The default filter is upcoming, so upcoming meetings are what the first page
// renders; and only a PAST meeting projects its attendance, which is the expensive
// half of a calendar replay (measured: fifteen upcoming meetings settle in ~70ms
// against ~1.1s EACH for a past one, because a past one waits on the interaction
// recorder and the cadence updater). Fifteen past meetings would cost this one
// fixture more than the rest of the domain put together, for a list the test
// never filters to.
func calendarRevealFixture() []Entity {
	entities := make([]Entity, 0, calendarRevealFixtureSize+1)
	entities = append(entities, Contact("attendee", Methods(MethodEmail)))
	for i := 1; i <= calendarRevealFixtureSize; i++ {
		entities = append(entities, CalendarEvent(fmt.Sprintf("e%02d", i), "attendee", StartsInDays(i)))
	}
	return entities
}
