package synthetic

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
)

// fixtureTestAnchor is a fixed anchor for the pure fixture-shape tests. Its value
// is irrelevant to every assertion below (all of them are anchor-relative), and
// pinning it keeps the tests off the wall clock.
var fixtureTestAnchor = time.Date(2026, time.March, 11, 12, 0, 0, 0, time.UTC)

func newFixtureTestGenerator() *factory.Generator {
	return factory.NewGeneratorAt(factory.DefaultSeed, "fixtureshape", fixtureTestAnchor)
}

// markerToken is the marker convention's mechanical constraint: a single lowercase
// alphanumeric token. The contact search path is Postgres full-text search, which
// splits a hyphenated marker into several stemmed lexemes whose parts are shared
// between markers — so a marker that violates this resolves to the wrong row (or
// to several) instead of failing loudly.
var markerToken = regexp.MustCompile(`^[a-z0-9]+$`)

// TestPinnedFixtureMarkersWellFormed pins the marker convention itself: every
// marker is a legal single token, they are mutually distinct, and none is a
// substring of another (a substring pair would make a mis-resolution look like a
// legitimate hit to any check weaker than the exactly-one rule). Pure — no DB, no
// PRNG dependency beyond the fixed generator.
func TestPinnedFixtureMarkersWellFormed(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, PinnedFixtureMarkers)
	seen := map[string]bool{}
	for _, marker := range PinnedFixtureMarkers {
		require.Regexp(t, markerToken, marker, "marker %q must be a single lowercase alphanumeric token", marker)
		require.False(t, seen[marker], "marker %q is declared twice", marker)
		seen[marker] = true
	}
	for _, a := range PinnedFixtureMarkers {
		for _, b := range PinnedFixtureMarkers {
			if a == b {
				continue
			}
			require.False(t, strings.Contains(a, b), "marker %q contains marker %q — markers must not nest", a, b)
		}
	}
}

// TestPinnedTourFixturesCarryTheirMarkers proves every built fixture spec actually
// carries its marker in full_name (the property the whole resolution scheme rests
// on), that the built set covers exactly the fixtures this block seeds, and that
// no two fixtures share a full_name.
func TestPinnedTourFixturesCarryTheirMarkers(t *testing.T) {
	t.Parallel()

	plans := buildPinnedTourFixtures(newFixtureTestGenerator())

	// The three fixtures that ride EXISTING seeded contacts are declared in
	// PinnedFixtureMarkers but are not built here, so the built set is the
	// complement — stated as a set difference rather than a hardcoded count so
	// adding a fixture to either side cannot silently pass.
	riders := map[string]bool{
		FixtureMarkerOutreach: true,
		FixtureMarkerResponse: true,
		FixtureMarkerPending:  true,
	}
	want := make([]string, 0, len(PinnedFixtureMarkers))
	for _, marker := range PinnedFixtureMarkers {
		if !riders[marker] {
			want = append(want, marker)
		}
	}

	built := map[string]bool{}
	names := map[string]bool{}
	for _, plan := range plans {
		require.Contains(t, plan.spec.FullName, plan.marker, "fixture %q must carry its marker in full_name", plan.marker)
		require.False(t, built[plan.marker], "fixture %q built twice", plan.marker)
		built[plan.marker] = true
		require.False(t, names[plan.spec.FullName], "two pinned fixtures share a full_name")
		names[plan.spec.FullName] = true
	}
	require.Len(t, built, len(want), "every non-riding marker is built exactly once")
	for _, marker := range want {
		require.True(t, built[marker], "marker %q is declared but never built", marker)
	}
}

// TestPinnedOverdueFixturesCloseTheDiversityGap asserts the two designated overdue
// fixtures do what the frozen catalog cannot: supply the third distinct old
// creation age and additional cadences the dashboard's urgency-diversity gate
// needs at small catalog sizes, while staying genuinely overdue under PRODUCTION
// cadence durations. Prod durations are read from the cadence package's own source
// of truth (not hardcoded) so the test cannot drift from prod semantics.
func TestPinnedOverdueFixturesCloseTheDiversityGap(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	require.Len(t, pinnedOverdueFixtures, 2, "both overdue tours consume one overdue contact per sweep")

	// The catalog's BACKDATED cadence-bearing slots are creation indices 0 and 3,
	// both drawn from the overdue ladder — derived here from catalogOptionsFor rather
	// than restated, so a catalog change is visible to this test.
	catalogBackdatedCadences := map[string]bool{}
	catalogCreatedAges := map[time.Duration]bool{}
	for _, pair := range catalogOverdueLadder {
		catalogCreatedAges[pair.createdAge] = true
	}
	for _, i := range []int{0, 3} {
		spec := newFixtureTestGenerator().Contact(catalogOptionsFor(i, 5, fixtureTestAnchor, "synth-fixtureshape-")...)
		require.NotNil(t, spec.Cadence, "catalog index %d must be cadence-bearing", i)
		require.NotNil(t, spec.CreatedAt, "catalog index %d must be backdated", i)
		catalogBackdatedCadences[*spec.Cadence] = true
	}

	const d1Floor = 14 * 24 * time.Hour
	distinctAges := map[time.Duration]bool{}
	distinctCadences := map[string]bool{}
	tiers := map[string]bool{}
	for _, pair := range pinnedOverdueFixtures {
		require.True(t, validCadences[pair.cadence], "fixture %s cadence %q must be a migration-005 CHECK cadence", pair.marker, pair.cadence)

		ct, err := cadence.ParseCadence(pair.cadence)
		require.NoError(t, err)
		period := cadence.GetCadenceDuration(ct)
		require.Greater(t, pair.createdAge, period,
			"fixture %s (%s, age %s) must be overdue under prod durations (age > period %s)", pair.marker, pair.cadence, pair.createdAge, period)

		// Backdated past the coherence gate's 14-day floor, so each fixture is a
		// genuine backdated-overdue contact rather than a recent one that merely
		// computes overdue.
		require.Greater(t, pair.createdAge, d1Floor, "fixture %s must be backdated past the 14d floor", pair.marker)

		// The gap the frozen catalog cannot close: a NEW distinct creation age and a
		// NEW distinct cadence.
		require.False(t, catalogCreatedAges[pair.createdAge],
			"fixture %s created age %s duplicates a catalog ladder entry — it adds no new distinct creation age", pair.marker, pair.createdAge)
		require.False(t, catalogBackdatedCadences[pair.cadence],
			"fixture %s cadence %q duplicates a catalog backdated slot's — it adds no new distinct cadence", pair.marker, pair.cadence)

		require.False(t, distinctAges[pair.createdAge], "fixture %s created age %s duplicates the other overdue fixture's", pair.marker, pair.createdAge)
		distinctAges[pair.createdAge] = true
		require.False(t, distinctCadences[pair.cadence], "fixture %s cadence %q duplicates the other overdue fixture's", pair.marker, pair.cadence)
		distinctCadences[pair.cadence] = true

		tiers[overdueUrgencyTier(pair.createdAge-period)] = true
	}

	// Spanning ≥2 urgency tiers is the property the dashboard's most-urgent ordering
	// is evidence OF: two overdue fixtures at the same magnitude would be
	// indistinguishable on the card list.
	require.GreaterOrEqual(t, len(tiers), 2, "the overdue fixtures must span ≥2 urgency tiers, got %v", tiers)
}

// overdueUrgencyTier buckets a days-overdue magnitude into the single-digit / tens
// / hundreds tiers the dashboard's urgency ordering exists to separate — the same
// split the catalog ladder's own well-formedness test uses.
func overdueUrgencyTier(overdue time.Duration) string {
	switch {
	case overdue < 10*24*time.Hour:
		return "single-digit"
	case overdue < 100*24*time.Hour:
		return "tens"
	default:
		return "hundreds"
	}
}

// TestPinnedFixturesAvoidTheFirstRankedCadence protects a POSITIONAL selection the
// contacts tour deliberately keeps: it mutates the first row of the default
// cadence-ordered list. That order is cadence frequency ascending, so the shortest
// production cadence ranks first — and the catalog always seeds one at creation
// index 0. If a pinned fixture also carried it, the fixture could take that row and
// collide with the tour's own reservations. Derived from the cadence package's
// durations rather than from the SQL's rank literals.
func TestPinnedFixturesAvoidTheFirstRankedCadence(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	firstRanked := ""
	var shortest time.Duration
	for name := range validCadences {
		ct, err := cadence.ParseCadence(name)
		require.NoError(t, err)
		d := cadence.GetCadenceDuration(ct)
		if shortest == 0 || d < shortest {
			shortest, firstRanked = d, name
		}
	}
	require.NotEmpty(t, firstRanked)
	require.Equal(t, firstRanked, catalogOverdueLadder[0].cadence,
		"catalog creation index 0 must hold the first-ranked cadence, else nothing guarantees the tour's first row is a catalog contact")

	for _, plan := range buildPinnedTourFixtures(newFixtureTestGenerator()) {
		if plan.spec.Cadence == nil {
			continue
		}
		require.NotEqual(t, firstRanked, *plan.spec.Cadence,
			"pinned fixture %s must not carry the first-ranked cadence %q", plan.marker, firstRanked)
	}
}

// TestPinnedMergeFixturesConflict asserts the merge pair genuinely conflicts on
// more than one of the three fields the merge preview surfaces (cadence, location,
// birthday) — the property the merge tour's conflict-toggle capture consumes,
// which a mere "two distinct contacts" check would not establish.
func TestPinnedMergeFixturesConflict(t *testing.T) {
	t.Parallel()

	var target, source *factory.ContactSpec
	for _, plan := range buildPinnedTourFixtures(newFixtureTestGenerator()) {
		switch plan.marker {
		case FixtureMarkerMergeTarget:
			spec := plan.spec
			target = &spec
		case FixtureMarkerMergeSource:
			spec := plan.spec
			source = &spec
		}
	}
	require.NotNil(t, target, "merge target fixture must be built")
	require.NotNil(t, source, "merge source fixture must be built")

	// The preview marks a field conflicting when the SOURCE's value is non-empty and
	// differs from the target's, so every conflicting field must be set on the source.
	require.NotNil(t, source.Cadence)
	require.NotNil(t, target.Cadence)
	require.NotEqual(t, *target.Cadence, *source.Cadence, "merge pair must differ in cadence")

	require.NotNil(t, source.Location)
	require.NotNil(t, target.Location)
	require.NotEqual(t, *target.Location, *source.Location, "merge pair must differ in location — a second genuine conflict beyond cadence")
}

// TestPinnedBirthdayFixtureLandsInTheHighlightWindow asserts the dedicated
// birthday fixture is exactly its offset days out — a DATE-INDEPENDENT
// classification (a year-wrap changes which calendar year the next occurrence
// falls in, never the day count) — inside the page's ≤7-day highlight window, and
// on a day the catalog-riding clock-anchored fixtures do not already occupy.
func TestPinnedBirthdayFixtureLandsInTheHighlightWindow(t *testing.T) {
	t.Parallel()

	require.Greater(t, fixtureBirthdayOffsetDays, 0, "the fixture must be upcoming, not today or past")
	require.LessOrEqual(t, fixtureBirthdayOffsetDays, 7, "the fixture must land in the ≤7-day highlight window")
	for _, planned := range BirthdayFixturePlan(fixtureTestAnchor) {
		require.NotEqual(t, planned.OffsetDays, fixtureBirthdayOffsetDays,
			"the dedicated fixture must not duplicate the catalog-riding %s fixture's offset", planned.Role)
	}

	// Verified against several anchors, including the year boundary and the leap-day
	// neighbourhood, so the day-count claim is not an artifact of one date.
	//
	// The day COUNT is what is pinned, deliberately not the page SECTION. A forward
	// fixture seeded within `offset` days of December 31 wraps into next January, so
	// its occurrence in the CURRENT calendar year is already past and the page files
	// it under "Already Celebrated This Year" — while its next occurrence is still
	// exactly `offset` days away. That is the page's own behavior, the section is
	// therefore date-dependent, and pinning it would be a fixture that passes until a
	// particular week of the year. The date-independent imminent guarantee is the
	// catalog-riding "today" fixture (offset 0 can never be past-this-year), which
	// already exists.
	for _, anchor := range []time.Time{
		fixtureTestAnchor,
		time.Date(2026, time.December, 30, 23, 0, 0, 0, time.UTC),
		time.Date(2026, time.December, 31, 1, 0, 0, 0, time.UTC),
		time.Date(2028, time.February, 27, 6, 0, 0, 0, time.UTC),
		time.Date(2027, time.February, 27, 6, 0, 0, 0, time.UTC),
	} {
		bday := BirthdayFixtureDate(anchor, fixtureBirthdayOffsetDays)
		require.Equal(t, fixtureBirthdayOffsetDays, BirthdayDaysUntil(bday, anchor),
			"birthday fixture must be exactly %d days out at anchor %s", fixtureBirthdayOffsetDays, anchor)
	}
}
