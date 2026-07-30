package synthetic

import (
	"time"

	"personal-crm/backend/internal/synthetic/factory"
)

// --- Pinned tour fixtures ---------------------------------------------------
//
// THE MARKER CONVENTION — this comment block is the single authority. Cite it;
// do not restate it at the call sites or in the tours.
//
// Every world state the QA tours in frontend/tests/tours/ depend on is seeded as
// a NAMED, hand-authored fixture instead of being left to whatever the generated
// population happens to contain. Each fixture contact carries a MARKER appended
// to its full_name:
//
//	"<namespace prefix><Given> <Surname> <marker>"
//
// A tour resolves a fixture with ONE API search (GET /contacts?search=<marker>)
// and then verifies the row it got back carries the expected marker AND the
// expected state before using it — resolution is verified, never inferred from
// the search having returned something.
//
// A marker is therefore constrained by the search path, which is Postgres FULL
// TEXT search over full_name plus the contact's method values
// (to_tsvector('english', …) @@ plainto_tsquery('english', …) — see ListContacts
// in db/queries/contact.sql), NOT a substring match. So a marker is a SINGLE
// lowercase alphanumeric token, no hyphens and no spaces: a hyphenated marker is
// split into several stemmed lexemes whose parts are shared between markers
// ("fx-no-activity" yields "fx" and "activ"), which is precisely the
// mis-resolution the pinning exists to remove. The shared "fx" prefix keeps every
// marker greppable across Go, SQL and TypeScript.
//
// A marker must resolve to EXACTLY ONE contact in a seeded world. That is
// asserted at both ends: the coverage checks require exactly one exact-marker
// match within the namespace and require the search path to return exactly that
// row, and the tours require exactly one match before using a fixture.
const (
	// FixtureMarkerNoActivity is the contact with no outreach, no response, no
	// pending follow-up and zero product-visible tasks — the "no recent activity"
	// and "no tasks yet" subject. Cadence-bearing with recent creation, so it also
	// supplies the recent-never-connected coherence floor.
	FixtureMarkerNoActivity = "fxnoactivity"
	// FixtureMarkerOutreach marks the outbound-only contact (last_outreach_at set,
	// last_contacted NULL) — the "last outreach" subject. Seeded by its own recipe
	// (it needs an outbound message), not by the replay-free pinned block.
	FixtureMarkerOutreach = "fxoutreach"
	// FixtureMarkerResponse marks the reply-bridged telegram mutual contact
	// (last_response_at set) — the "last response" subject, distinct from the
	// outreach one. Seeded by its own recipe (it needs a bridged message pair).
	FixtureMarkerResponse = "fxresponse"
	// FixtureMarkerPending marks the contact carrying a live follow-up loop — the
	// "awaiting reply" subject. Seeded by its own recipe (it needs the whole causal
	// chain: a cadence, an outbound-bearing interaction, then the live loop).
	FixtureMarkerPending = "fxpending"
	// FixtureMarkerOverdueA / FixtureMarkerOverdueB mark the two designated overdue
	// contacts. They are STATE guarantees, not subject bindings: the dashboard and
	// relationship-loop tours pick the most-urgent card POSITIONALLY, and that
	// positional selection is the behavior under test. Two are seeded because both
	// tours mutate an overdue contact within one single-worker sweep.
	FixtureMarkerOverdueA = "fxoverduea"
	FixtureMarkerOverdueB = "fxoverdueb"
	// FixtureMarkerMergeTarget / FixtureMarkerMergeSource mark the merge pair: the
	// contact kept and the contact archived. They differ in cadence AND in location,
	// so the merge preview surfaces more than one genuine field conflict.
	FixtureMarkerMergeTarget = "fxmergetarget"
	FixtureMarkerMergeSource = "fxmergesource"
	// FixtureMarkerSearch marks the cadence-bearing contact the list-context tour
	// searches for and navigates from. Never mutated.
	FixtureMarkerSearch = "fxsearchsubject"
	// FixtureMarkerDelete marks the disposable delete victim, consumed once per
	// sweep and re-established by the next reseed.
	FixtureMarkerDelete = "fxdeletevictim"
	// FixtureMarkerBirthday marks the dedicated birthday fixture inside the
	// birthdays page's highlight window.
	FixtureMarkerBirthday = "fxbirthday"
)

// TourOverdueCaptureCap is the maximum overdue population a QA tour capture may
// carry. It is a CAPTURE bound, not a product limit: the tours' overdue evidence
// is a JSON array the capture normalizer slices, so a population above the cap
// would reach the judge as an unnamed subset — which is why both overdue-bearing
// tours check the live population against it BEFORE their first capture and stop
// rather than grade a truncated list.
//
// It is deliberately a DECISION THRESHOLD rather than a capacity forecast. No
// headroom is reserved for anyone: the standard world's own test asserts its
// overdue population stays at or under this number, so the change that would
// exceed it fails a named test and has to make a conscious call (raise it again
// with a stated reason, split the world, or exclude a declaration class) with
// the information in hand. The value only sets WHERE that decision happens.
//
// The TypeScript side (OVERDUE_CAPTURE_CAP in
// frontend/tests/tours/support/pinned-fixtures.ts) must equal this, and the
// tour-support drift test reads this file to assert it — changing one without
// the other fails a test.
const TourOverdueCaptureCap = 96

// PinnedFixtureMarkers is every pinned tour-fixture marker: first the ones
// buildPinnedTourFixtures builds from a bare contact spec, then the three whose
// causal chain needs its own recipe. Within each group the order is declaration
// order and is NOT load-bearing — nothing reads this slice positionally, and the
// fixtures are not built in this sequence (the overdue pair is appended from its
// own table, after the birthday fixture). The coverage checks iterate it, so a
// fixture added without a marker entry is not silently unasserted.
var PinnedFixtureMarkers = []string{
	FixtureMarkerNoActivity,
	FixtureMarkerOverdueA,
	FixtureMarkerOverdueB,
	FixtureMarkerMergeTarget,
	FixtureMarkerMergeSource,
	FixtureMarkerSearch,
	FixtureMarkerDelete,
	FixtureMarkerBirthday,
	FixtureMarkerOutreach,
	FixtureMarkerResponse,
	FixtureMarkerPending,
}

// pinnedOverdueFixtures is the (cadence, created-age) table for the two
// designated overdue fixtures. Both pairs are genuinely overdue under PRODUCTION
// cadence durations, and each carries a created age distinct from the other's, so
// their days-overdue magnitudes deliberately land in different urgency tiers —
// which is what lets the dashboard's urgency-diversity rendering be exercised
// rather than assumed.
//
// Neither cadence may be the FIRST-RANKED one (weekly): the contact list's default
// order is cadence-frequency ascending, and the contacts tour mutates its first
// row. A pinned fixture at rank 1 could displace the contact that row is meant to
// be and collide with the tour's own fixture reservations.
//
// These two are the ONLY contacts this block adds to the overdue population, and
// the population is capture-bounded, not just seed-bounded: the tours' overdue
// evidence is a JSON array that the capture normalizer slices at a cap, dropping
// the tail.
//
// What the tours do about that, stated exactly: in BOTH tours that expose the
// overdue set (dashboard, relationship-loop) every capture carries the explicit
// OVERDUE_CAPTURE_CAP from frontend/tests/tours/support/pinned-fixtures.ts, and
// each tour checks the live population against that cap BEFORE its first capture —
// which it must, because the dashboard's own overdue GET is drained into whichever
// capture happens to run next, not only into the ones that name the set. A
// population above the cap therefore stops the tour instead of quietly reaching the
// judge as an unnamed subset. Adding a third overdue fixture means re-checking that
// cap, not just this table.
//
// Anchor-relative via WithCreatedAge (no time.Now()); a pure table, so it draws no
// PRNG.
var pinnedOverdueFixtures = []struct {
	marker     string
	cadence    string
	createdAge time.Duration
}{
	{FixtureMarkerOverdueA, "quarterly", 300 * 24 * time.Hour}, // ~210 days overdue (quarterly period 90d)
	{FixtureMarkerOverdueB, "biweekly", 20 * 24 * time.Hour},   // ~6 days overdue (biweekly period 14d)
}

// Cadences for the remaining cadence-bearing fixtures. None is the first-ranked
// cadence, for the reason given on pinnedOverdueFixtures. The merge pair's two
// values must differ — a cadence conflict is one of the three the merge preview
// surfaces.
const (
	fixtureNoActivityCadence  = "monthly"
	fixtureMergeTargetCadence = "monthly"
	fixtureMergeSourceCadence = "quarterly"
	fixtureSearchCadence      = "biannual"
)

// FixtureBirthdayOffsetDays places the dedicated birthday fixture inside the
// birthdays page's ≤7-day highlight window.
//
// It is a forward offset, so the fixture's card always reads exactly this many
// days out — a year-wrap changes which calendar year the next occurrence falls in,
// never the day count. The page's SECTION for it is date-dependent (an occurrence
// already past in the current calendar year files under "Already Celebrated This
// Year" even when the next one is two days away), which is why the tour resolves
// the fixture's card by identity rather than by section.
const FixtureBirthdayOffsetDays = 2

// fixtureMergeLocationStems are the two DISTINCT location stems the merge pair
// carries. Distinct values are what make location a second genuine merge conflict
// alongside cadence. The values are flat (no comma/hierarchy) so EnsurePlaceTx
// mints a flat place node with no `within` parent edge.
var fixtureMergeLocationStems = [2]string{"riverton", "lakeside"}

// pinnedFixturePlan is one pinned fixture's marker paired with the contact spec it
// is built from.
type pinnedFixturePlan struct {
	marker string
	spec   factory.ContactSpec
}

// buildPinnedTourFixtures builds every pinned fixture spec, in seeding order.
// Split out from the seeding loop so the shape rules the fixtures must satisfy can
// be asserted purely (no DB) over the SPECS THEMSELVES rather than over a
// restatement of them — an assertion written against a second copy of the
// constants proves only that the copy matches.
//
// Draws the shared name PRNG once per fixture, so it may only be called from the
// world's append-last tail step.
func buildPinnedTourFixtures(gen *factory.Generator) []pinnedFixturePlan {
	anchor := gen.Anchor()
	prefix := gen.Prefix()

	fixtures := []struct {
		marker string
		opts   []factory.ContactOption
	}{
		// No activity of any kind, plus a cadence and a recent creation date so the
		// contact also supplies the recent-never-connected coherence floor.
		{FixtureMarkerNoActivity, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(fixtureNoActivityCadence),
			factory.WithRecentCreation(fixtureRecentWindow),
		}},
		// The merge pair: cadence AND location both differ, so the preview surfaces
		// two genuine conflicts rather than relying on one.
		{FixtureMarkerMergeTarget, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(fixtureMergeTargetCadence),
			factory.WithLocation(prefix + "place-" + fixtureMergeLocationStems[0]),
		}},
		{FixtureMarkerMergeSource, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(fixtureMergeSourceCadence),
			factory.WithLocation(prefix + "place-" + fixtureMergeLocationStems[1]),
		}},
		// Searched + cadence-filtered navigation subject: needs a cadence to survive
		// the has_cadence filter.
		{FixtureMarkerSearch, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(fixtureSearchCadence),
		}},
		// Delete victim: consumed every sweep, so it carries nothing worth losing.
		{FixtureMarkerDelete, []factory.ContactOption{
			factory.WithEmail(),
		}},
		// Birthday inside the highlight window.
		{FixtureMarkerBirthday, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithBirthday(BirthdayFixtureDate(anchor, FixtureBirthdayOffsetDays)),
		}},
	}
	for _, pair := range pinnedOverdueFixtures {
		fixtures = append(fixtures, struct {
			marker string
			opts   []factory.ContactOption
		}{pair.marker, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(pair.cadence),
			factory.WithCreatedAge(pair.createdAge),
		}})
	}

	plans := make([]pinnedFixturePlan, 0, len(fixtures))
	for _, f := range fixtures {
		plans = append(plans, pinnedFixturePlan{
			marker: f.marker,
			spec:   gen.Contact(append(f.opts, factory.WithNameMarker(f.marker))...),
		})
	}
	return plans
}

// fixtureRecentWindow bounds a "recently created" fixture within the last ~48h
// (anchor-relative).
const fixtureRecentWindow = 48 * time.Hour
