package synthetic

import (
	"context"
	"fmt"
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
	// supplies the recent-never-connected coherence floor at small catalog sizes.
	FixtureMarkerNoActivity = "fxnoactivity"
	// FixtureMarkerOutreach marks the outbound-only contact (last_outreach_at set,
	// last_contacted NULL) — the "last outreach" subject. Rides an existing seeded
	// contact from the two-sided direction cohort.
	FixtureMarkerOutreach = "fxoutreach"
	// FixtureMarkerResponse marks the reply-bridged telegram mutual contact
	// (last_response_at set) — the "last response" subject, distinct from the
	// outreach one. Rides an existing seeded contact.
	FixtureMarkerResponse = "fxresponse"
	// FixtureMarkerPending marks the contact carrying a live follow-up loop — the
	// "awaiting reply" subject. Rides the existing awaiting-reply scenario contact.
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
	// birthdays page's highlight window. It is a NEW contact rather than one of the
	// clock-anchored catalog fixtures: those ride catalog slots whose names are
	// frozen, so a catalog fixture cannot be both unchanged and marker-bearing.
	FixtureMarkerBirthday = "fxbirthday"
)

// PinnedFixtureMarkers is every pinned tour-fixture marker: first the ones this
// block seeds, then the ones that ride contacts other blocks seed. Within each
// group the order is declaration order and is NOT load-bearing — nothing reads
// this slice positionally, and the fixtures are not built in this sequence (the
// overdue pair is appended from its own table, after the birthday fixture). The
// coverage checks iterate it, so a fixture added without a marker entry is not
// silently unasserted.
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
// cadence durations, and both are chosen to close a gap the FROZEN catalog cannot:
// the catalog supplies backdated cadence-bearing slots only at creation indices 0
// and 3, so a small world has two distinct old creation ages where the dashboard's
// urgency-diversity gate needs three. Each pair therefore carries a created age
// distinct from every catalog ladder entry AND from the other pair's, and a
// cadence distinct from the two the catalog's backdated slots use — and their
// days-overdue magnitudes deliberately land in different urgency tiers.
//
// Neither cadence may be the FIRST-RANKED one (weekly): the contact list's default
// order is cadence-frequency ascending, and the contacts tour mutates its first
// row. A pinned fixture at rank 1 could displace the catalog contact that row is
// meant to be and collide with the tour's own fixture reservations.
//
// These two are the ONLY contacts this block adds to the overdue population, and
// the population is capture-bounded, not just seed-bounded: the tours' overdue
// evidence is a JSON array that the capture normalizer slices at a cap, dropping
// the tail. The prod-shaped catalog already supplies 50 overdue contacts, so this
// pair takes the set to 52 — measured, not assumed. The overdue tours therefore
// carry an explicit capture cap (OVERDUE_CAPTURE_CAP in
// frontend/tests/tours/support/pinned-fixtures.ts) and refuse to run above it, so
// a set that outgrows the cap fails loudly instead of reaching the judge with an
// unnamed subset missing. Adding a third overdue fixture means re-checking that
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
// birthdays page's ≤7-day highlight window, and away from the offsets the
// catalog-riding clock-anchored fixtures already occupy so the imminent group
// gains a distinct additional member.
//
// It is a forward offset, so the fixture's card always reads exactly this many
// days out — a year-wrap changes which calendar year the next occurrence falls in,
// never the day count. The page's SECTION for it is date-dependent (an occurrence
// already past in the current calendar year files under "Already Celebrated This
// Year" even when the next one is two days away), which is why the tour resolves
// the fixture's card by identity rather than by section.
const FixtureBirthdayOffsetDays = 2

// fixtureMergeLocationStems are the two DISTINCT location stems the merge pair
// carries, drawn from the catalog's pool so places keep repeating across the world
// (prod-like). Distinct values are what make location a second genuine merge
// conflict alongside cadence.
var fixtureMergeLocationStems = [2]string{catalogLocationStems[0], catalogLocationStems[1]}

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
// profile's append-last block.
func buildPinnedTourFixtures(gen *factory.Generator) []pinnedFixturePlan {
	anchor := gen.Anchor()
	prefix := gen.Prefix()

	fixtures := []struct {
		marker string
		opts   []factory.ContactOption
	}{
		// No activity of any kind, plus a cadence and a recent creation date so the
		// contact also satisfies the recent-never-connected floor at small catalog
		// sizes.
		{FixtureMarkerNoActivity, []factory.ContactOption{
			factory.WithEmail(),
			factory.WithCadence(fixtureNoActivityCadence),
			factory.WithRecentCreation(catalogRecentWindow),
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

// seedPinnedTourFixtures seeds the hand-authored fixtures the QA tours depend on
// and records each one's contact id against its marker on the harness.
//
// It runs at the VERY END of the catalog profile, after every other
// generator-drawing block, because gen.Contact draws the shared name PRNG:
// inserting these calls anywhere earlier would shift every later allocation and a
// shifted numeric identifier can land on an id another contact already owns.
// Running last also means these contacts are created after the task reconcile, so
// they carry no cadence task — which is what lets the no-activity fixture hold its
// zero-visible-tasks property. That is a consequence to understand, not a thing to
// rely on elsewhere; the coverage check asserts the property directly.
//
// The fixtures deliberately carry NO replayed source payloads, so none of them
// disturbs the settled-interaction accounting.
func seedPinnedTourFixtures(ctx context.Context, h *Harness, gen *factory.Generator, profile Profile, res *ProfileResult) error {
	for _, plan := range buildPinnedTourFixtures(gen) {
		contact, err := h.SeedContact(ctx, plan.spec)
		if err != nil {
			return fmt.Errorf("profile %s: seed pinned fixture %s: %w", profile, plan.marker, err)
		}
		res.Contacts++
		h.SetPinnedFixtureID(plan.marker, contact.ID)
	}
	return nil
}
