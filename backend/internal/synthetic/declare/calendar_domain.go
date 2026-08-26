package declare

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

}
