package declare

import (
	"fmt"
	"time"
)

// Contacts-domain resolutions (spec/contacts.yaml).
//
// Every ui-surface CON behavior is resolved here or waived in waivers.go; the
// completeness test enforces that there is no fourth state. Two remain waived
// and say why there: CON-039 and CON-046 are proposed behaviors with no citing
// test.
func init() {
	RegisterNone("CON-055", "the new-contact form CREATES the contact under test; there is no pre-existing data to provision")

	// Three cadences whose names are deliberately ANTI-CORRELATED with the
	// cadence order, so an implementation that ignored sort=cadence and fell back
	// to name order cannot accidentally pass: cadence-descending is
	// Yankee -> Alpha -> Mike, name-ascending is Alpha, Mike, Yankee, and
	// name-descending is Yankee, Mike, Alpha. All three orders are distinct, which
	// is why the names are pinned rather than drawn.
	Register(Declaration{
		Behavior: "CON-038",
		Entities: []Entity{
			Contact("weekly", Cadence("weekly"), ExplicitName("Cadence", "Sort Yankee")),
			Contact("monthly", Cadence("monthly"), ExplicitName("Cadence", "Sort Alpha")),
			Contact("annual", Cadence("annual"), ExplicitName("Cadence", "Sort Mike")),
		},
	})

	// Three contacts whose name-ascending order is known at declaration time, so
	// the middle one can be opened directly and each arrow press asserted against
	// a NAMED neighbour rather than "some navigation happened".
	Register(Declaration{
		Behavior: "CON-040",
		Entities: []Entity{
			Contact("a", ExplicitName("Kbd", "Move Alpha")),
			Contact("b", ExplicitName("Kbd", "Move Bravo")),
			Contact("c", ExplicitName("Kbd", "Move Charlie")),
		},
	})

	// One contact to open the row context menu on.
	Register(Declaration{
		Behavior: "CON-041",
		Entities: []Entity{Contact("target")},
	})

	// One contact to delete. Its name carries the DESCENDER edge token so the
	// visual guard that rides this fixture — the heading's line-height, which
	// clips y/g/j/p/q under leading-7 — is asserted against a name that actually
	// has descenders in it. None of the delete behavior's own then-items reads the
	// name beyond the manifest.
	Register(Declaration{
		Behavior: "CON-042",
		Entities: []Entity{Contact("target", NameEdge(NameEdgeDescender))},
	})

	// A merge pair conflicting on EVERY field the spec names — cadence, location
	// and birthday — so the default-keeps-target proof covers the whole clause
	// rather than one field of it. The source carries a phone (not an email) so
	// the post-merge read proves a method actually transferred.
	Register(Declaration{
		Behavior: "CON-043",
		Entities: []Entity{
			Contact("target",
				Cadence("monthly"),
				Location("New York"),
				BirthdayOn(time.March, 15),
				Methods(MethodEmail),
			),
			Contact("source",
				Cadence("weekly"),
				Location("San Francisco"),
				BirthdayOn(time.July, 20),
				Methods(MethodPhone),
			),
		},
	})

	// One cadence-bearing contact for the list-row mark-contacted quick action.
	Register(Declaration{
		Behavior: "CON-044",
		Entities: []Entity{Contact("target", Cadence("weekly"))},
	})

	// One contact per birthday grouping the page renders. Every date is stated
	// explicitly against a REAL leap-safe birth year except real-today, which is
	// the placeholder-year (age-suppressed) case and is the only one that has to
	// track the run's own anchor.
	Register(Declaration{
		Behavior: "CON-045",
		Entities: []Entity{
			Contact("real-today", BirthdayPlaceholderToday()),
			Contact("mocked-today", BirthdayOn(time.June, 15)),
			Contact("soon", BirthdayOn(time.June, 18)),
			Contact("mid", BirthdayOn(time.June, 20)),
			Contact("later", BirthdayOn(time.June, 25)),
			Contact("celebrated", BirthdayOn(time.March, 10)),
			Contact("gift-feb", BirthdayOn(time.February, 14)),
			Contact("leap-day", BirthdayOn(time.February, 29)),
		},
	})

	// One contact to log interactions against.
	Register(Declaration{
		Behavior: "CON-053",
		Entities: []Entity{Contact("target")},
	})

	// One cadence-bearing contact. Neither sort test reads row order or
	// last_contacted — they assert the request the header click issues and the
	// applied aria-sort state — so nothing more is declared.
	Register(Declaration{
		Behavior: "CON-057",
		Entities: []Entity{Contact("target", Cadence("weekly"))},
	})

	// Two pages' worth at the twenty-row default: page 2 holds exactly two rows.
	// Names are drawn, not pinned — the pagination tests count rows and read the
	// control's state, never a name order.
	Register(Declaration{
		Behavior: "CON-058",
		Entities: numberedContacts(paginationFixtureSize),
	})

	// Three contacts whose name-ascending order is known at declaration time, so
	// the nav bar's Next button can be asserted to land on a NAMED neighbour.
	Register(Declaration{
		Behavior: "CON-059",
		Entities: []Entity{
			Contact("a", ExplicitName("Button", "Nav 1")),
			Contact("b", ExplicitName("Button", "Nav 2")),
			Contact("c", ExplicitName("Button", "Nav 3")),
		},
	})

	// Two contacts whose name-ascending order is known at declaration time, so
	// opening "a" always leaves a real forward move available. With drawn names
	// "a" sorts last about half the time, and at the end of the list the Next
	// control is disabled and the arrow key is a no-op — the citing test would
	// then assert the carried context against a URL that never changed.
	Register(Declaration{
		Behavior: "CON-060",
		Entities: []Entity{
			Contact("a", ExplicitName("Context", "Carry Alpha")),
			Contact("b", ExplicitName("Context", "Carry Bravo")),
		},
	})

	// A contact whose derived next-contact date is non-null, beside one with no
	// cadence at all (which renders the placeholder). The overdue amount is a
	// floor and no day count is asserted: all the fixture needs is a real date to
	// render.
	Register(Declaration{
		Behavior: "CON-061",
		Entities: []Entity{
			Contact("with-cadence", Cadence("weekly"), OverdueBy(Days(1))),
			Contact("without-cadence"),
		},
	})

	// The method-preservation fixture. Six of the seven citing tests edit a single
	// email; the seventh deletes one of two seeded methods, which needs a contact
	// that carries two to begin with.
	Register(Declaration{
		Behavior: "CON-063",
		Entities: []Entity{
			Contact("target", Methods(MethodEmail)),
			Contact("two-methods", Methods(MethodEmail, MethodPhone)),
		},
	})

	// The cadence-filter fixture: one contact per filter outcome (weekly,
	// monthly, none) sharing a surname, plus a fourth that shares neither the
	// surname nor a cadence so the TEXT search's filtering is falsifiable
	// independently of the cadence filter.
	//
	// The names are PINNED rather than drawn for two reasons. The citing test's
	// second search phase narrows on the shared surname, which has to be a
	// standalone lexeme — glued into the hyphenated namespace prefix it would
	// depend on how full-text search splits compound words. And no pinned name
	// may CONTAIN another (see ExplicitName): the four rendered pairs below are
	// mutually non-nesting, and each is ~24 characters, leaving the stored
	// 'synth-<ns>-<given> <surname>' far inside the contact API's 255-character
	// full_name cap even at the widest namespace.
	//
	// The shared surname is spelled out at each site rather than hoisted into a
	// constant: the containment invariant has no automated check, so the only way
	// to audit it is to read the pinned literals, and a constant hides three of
	// them from that reading.
	Register(Declaration{
		Behavior: "CON-054",
		Entities: []Entity{
			Contact("weekly", Cadence("weekly"), ExplicitName("Weeklyfilter", "Cadfilter")),
			Contact("monthly", Cadence("monthly"), ExplicitName("Monthlyfilter", "Cadfilter")),
			Contact("none", ExplicitName("Nofilter", "Cadfilter")),
			Contact("unrelated", ExplicitName("Zebrafilter", "Unrelated")),
		},
	})

	// One contact carrying three method kinds with the primary on a NON-default
	// one. Email is the factory's default primary whenever a contact has one, so
	// the "exactly one primary, and it is the one that was chosen" claim needs the
	// mark moved off it. The telegram value is the discriminating row for the
	// display normalization: the factory stores a bare handle (the handler strips a
	// leading '@' before the service ever sees it), so the '@' in the rendered
	// string can only come from the frontend.
	Register(Declaration{
		Behavior: "CON-056",
		Entities: []Entity{
			Contact("methods",
				Methods(MethodEmail, MethodTelegram, MethodGChat),
				PrimaryMethod(MethodTelegram),
			),
		},
	})

	// Back-to-list and list-context-reset both need the same shape: enough
	// cadence-bearing contacts to fill a second page, in a known name order so the
	// sole page-2 row is identifiable in advance. They are registered separately
	// rather than sharing one fixture so each behavior owns its own namespace.
	Register(Declaration{
		Behavior: "CON-065",
		Entities: backNavFixtureEntities(backNavStemBack),
	})
	Register(Declaration{
		Behavior: "CON-066",
		Entities: backNavFixtureEntities(backNavStemReset),
	})
}

// backNavFixtureSize is the back-navigation cohort: one more than the twenty-row
// default page size, so page 2 holds exactly one row and that row's identity is
// what the restored-page assertions turn on.
const backNavFixtureSize = 21

// paginationFixtureSize is two rows past the twenty-row default page size, so
// page 2 is non-empty by more than one and a row-count assertion on it is not
// satisfiable by an off-by-one.
const paginationFixtureSize = 22

// The two back-navigation cohorts' name stems. They differ because the composed
// world runs EVERY declaration against one namespace: a shared stem would put 21
// pairs of byte-identical full_names in it, which is the ambiguity ExplicitName's
// dedupe exemption cannot resolve for itself.
const (
	backNavStemBack  = "Back"
	backNavStemReset = "Reset"
)

// backNavFixtureEntities is the back-navigation cohort: zero-padded pinned names,
// so name-ASCENDING order equals insertion order and p21 is the sole page-2 row
// by construction rather than by whatever the name PRNG happened to draw. The
// stem is a parameter so two declarations can share the SHAPE without sharing the
// literals.
func backNavFixtureEntities(stem string) []Entity {
	entities := make([]Entity, 0, backNavFixtureSize)
	for i := 1; i <= backNavFixtureSize; i++ {
		entities = append(entities, Contact(
			fmt.Sprintf("p%02d", i),
			Cadence("monthly"),
			ExplicitName(stem, fmt.Sprintf("Nav %02d", i)),
		))
	}
	return entities
}

// numberedContacts is n plain contacts under zero-padded handles. The handles are
// numbered only so a manifest read is legible; the rendered names stay
// generator-drawn.
func numberedContacts(n int) []Entity {
	entities := make([]Entity, 0, n)
	for i := 1; i <= n; i++ {
		entities = append(entities, Contact(fmt.Sprintf("p%02d", i)))
	}
	return entities
}
