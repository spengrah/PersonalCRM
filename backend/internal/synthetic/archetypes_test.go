package synthetic

import (
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// catalogSlotSizes are the catalog sizes the slot/assignment tests sweep: the
// determinism run (5), the prod coverage run (9), the ladder rung boundary (12)
// and the multi-sample threshold (13), plus both shipping profiles (18, 150).
var catalogSlotSizes = []int{5, 9, 12, 13, 18, 150}

// TestCatalogSlotMatchesFrozenOptions is the anti-drift guard for the slot
// model: catalogSlot must report exactly the spec catalogOptionsFor BUILDS, for
// every index of every size the suite runs. The comparison goes through the real
// factory rather than through a restatement of the tables, so a change to either
// reader fails here instead of silently teaching the archetype assignment a
// world that no longer exists.
func TestCatalogSlotMatchesFrozenOptions(t *testing.T) {
	for _, n := range catalogSlotSizes {
		g := factory.NewGenerator(factory.DefaultSeed, "slotunit")
		anchor := g.Anchor()
		for i := 0; i < n; i++ {
			spec := g.Contact(catalogOptionsFor(i, n, anchor, g.Prefix())...)
			slot := catalogSlot(i, n)

			require.NotNil(t, spec.Cadence, "n=%d i=%d: every catalog slot is cadence-bearing", n, i)
			require.Equal(t, slot.Cadence, *spec.Cadence, "n=%d i=%d: catalogSlot cadence must match the built spec", n, i)

			hasMethods := len(spec.Methods) > 0
			require.Equal(t, slot.NoMethods, !hasMethods, "n=%d i=%d: catalogSlot no-methods must match the built spec", n, i)

			switch slot.Kind {
			case slotBackdated:
				require.NotNil(t, spec.CreatedAt, "n=%d i=%d: a backdated slot stamps created_at", n, i)
				require.True(t, spec.CreatedAt.Equal(anchor.Add(-slot.CreatedAgeBound)),
					"n=%d i=%d: backdated created_at must be exactly anchor − CreatedAgeBound (got %s, want %s)",
					n, i, spec.CreatedAt, anchor.Add(-slot.CreatedAgeBound))
			case slotRecent:
				require.NotNil(t, spec.CreatedAt, "n=%d i=%d: a recent slot stamps created_at", n, i)
				age := anchor.Sub(*spec.CreatedAt)
				require.GreaterOrEqual(t, age, time.Duration(0), "n=%d i=%d: a recent slot is not created in the future", n, i)
				require.Less(t, age, slot.CreatedAgeBound,
					"n=%d i=%d: a recent slot lands inside the window catalogSlot reports as its bound", n, i)
			case slotFresh:
				require.Nil(t, spec.CreatedAt, "n=%d i=%d: a fresh slot carries no created-age option", n, i)
				require.Zero(t, slot.CreatedAgeBound, "n=%d i=%d: a fresh slot reports no created age", n, i)
			}
		}
	}
}

// TestCatalogSlotIsPure pins the slot model as a pure function of (i, n): the
// assignment built on top of it must be reproducible without a generator, and a
// map iteration or clock read leaking in would make the whole overlay
// non-deterministic.
func TestCatalogSlotIsPure(t *testing.T) {
	for _, n := range catalogSlotSizes {
		for i := 0; i < n; i++ {
			require.Equal(t, catalogSlot(i, n), catalogSlot(i, n), "catalogSlot(%d, %d) must be pure", i, n)
		}
	}
}

// --- compatibility -----------------------------------------------------------

// TestArchetypeShapesMatchTheGenerator is the anti-drift guard for the table
// this package RESTATES rather than reads: the generator's per-archetype
// parameters are unexported and live a package below, so the compatibility rule
// declares its own copy of the two facts it consumes.
//
// It guards BOTH of them, over real sampled timelines:
//
//   - Intent — the field that selects WHICH inequality applies, and therefore
//     the more load-bearing half. An archetype is neutral exactly when its
//     timelines can never move last_contacted; a generator change that gave a
//     neutral archetype an inbound or a calendar entry would silently make the
//     "admissible on every cadence" shortcut false, and fails here instead.
//     (Overdue-versus-calm is a DESIGN statement about which side of the period
//     the archetype should sit on, not a generator fact, so it is pinned by the
//     margin test below rather than derived here.)
//   - NewestMin/NewestMax — measured over the newest entry that WRITES
//     last_contacted, which is the quantity the rule reasons about. Measuring the
//     newest entry of any kind would coincide today only because no archetype
//     emits a one-sided entry newer than its newest two-way one.
//
// The bounds must sit inside the declared range AND reach within one calmMargin
// of each end: (a) catches a generator that widened past what the rule was
// reasoned about, (b) catches one that narrowed or shifted. A drift smaller than
// the margin survives, which is the honest limit of a sampling guard — and a
// drift smaller than the margin cannot flip an admissibility verdict on its own,
// which is what the margin is for.
//
// The sample size makes (b) non-flaky rather than merely likely: the narrowest
// declared range is 265 whole-hour draws and the margin is 25 of them, so the
// probability of no sample landing in an end band is about e^-283.
func TestArchetypeShapesMatchTheGenerator(t *testing.T) {
	const namespaces = 3000
	caps := factory.MethodSet{Email: true}

	observedMin := map[factory.Archetype]time.Duration{}
	observedMax := map[factory.Archetype]time.Duration{}
	writesLastContacted := map[factory.Archetype]bool{}

	for s := 0; s < namespaces; s++ {
		g := factory.NewGenerator(factory.DefaultSeed, "shapeunit"+strconv.Itoa(s))
		for _, a := range archetypeTieBreak {
			tl := g.TimelineFor(a, caps)
			if a == factory.ArchetypeNeverContacted {
				require.Empty(t, tl.Entries, "never-contacted must stay history-free")
				continue
			}
			require.NotEmpty(t, tl.Entries, "%s must emit a timeline for an email-bearing contact", a)

			var newest time.Duration
			found := false
			for _, e := range tl.Entries {
				// Calendar carries no promotion mechanic — a matched event is always
				// mutual — so the generator must never ask for a pair key on one. The
				// calendar batch item has no field to carry it, and the mapper refuses
				// rather than dropping it (TestArchetypeCalendarPairKeyIsRejected).
				if e.Source == factory.SourceGCal {
					require.Zero(t, e.PairKey, "%s: a calendar entry must carry no promotion pair key", a)
				}
				if !archetypeWritesLastContacted(e) {
					continue
				}
				if !found || e.Age < newest {
					newest, found = e.Age, true
				}
			}
			if !found {
				continue
			}
			writesLastContacted[a] = true
			if prev, ok := observedMin[a]; !ok || newest < prev {
				observedMin[a] = newest
			}
			if prev, ok := observedMax[a]; !ok || newest > prev {
				observedMax[a] = newest
			}
		}
	}

	for _, a := range archetypeTieBreak {
		shape := archetypeShapes[a]

		// The intent half: neutral EXACTLY when nothing the archetype emits can move
		// last_contacted.
		if shape.Intent == intentNeutral {
			require.False(t, writesLastContacted[a],
				"%s is declared NEUTRAL — admissible on every cadence without reading its bounds — but its timelines carry an entry that writes last_contacted", a)
			require.Zero(t, shape.NewestMin, "%s is neutral, so it must declare no bounds to keep unread values out of the table", a)
			require.Zero(t, shape.NewestMax, "%s is neutral, so it must declare no bounds to keep unread values out of the table", a)
			continue
		}
		require.True(t, writesLastContacted[a],
			"%s is declared with an overdue/calm intent, so its timelines must contain an entry that writes last_contacted", a)

		// The bounds half, measured over that entry.
		require.GreaterOrEqual(t, observedMin[a], shape.NewestMin,
			"%s dated its newest two-way entry %s back, below the declared floor %s — the compatibility rule was reasoned about the declared range",
			a, observedMin[a], shape.NewestMin)
		require.LessOrEqual(t, observedMax[a], shape.NewestMax,
			"%s dated its newest two-way entry %s back, above the declared ceiling %s — the compatibility rule was reasoned about the declared range",
			a, observedMax[a], shape.NewestMax)
		require.LessOrEqual(t, observedMin[a]-shape.NewestMin, calmMargin,
			"%s never drew within one margin of its declared floor %s (closest %s) — the generator's floor has moved",
			a, shape.NewestMin, observedMin[a])
		require.LessOrEqual(t, shape.NewestMax-observedMax[a], calmMargin,
			"%s never drew within one margin of its declared ceiling %s (closest %s) — the generator's ceiling has moved",
			a, shape.NewestMax, observedMax[a])
	}
}

// TestPinnedOverdueFixtureCountMatchesTheTable keeps the exported constant — a
// const because the coverage tests add it back to a catalog prediction — in step
// with the fixture table it counts. A slice literal cannot be a const, so this is
// what makes the number a derivation rather than a restatement.
func TestPinnedOverdueFixtureCountMatchesTheTable(t *testing.T) {
	require.Equal(t, len(pinnedOverdueFixtures), PinnedOverdueFixtureCount,
		"the exported pinned-overdue count must equal the number of pinned overdue fixtures")
}

// TestArchetypeCompatibilityHasAMargin proves the rule is STRICT WITH A MARGIN,
// in both directions, and that the two pairs a naive >= would have admitted are
// rejected.
func TestArchetypeCompatibilityHasAMargin(t *testing.T) {
	for _, a := range historyArchetypes {
		shape := archetypeShapes[a]
		for _, cadenceName := range archetypeCadences {
			period, ok := productionCadencePeriod(cadenceName)
			require.True(t, ok, "%q must be a production cadence", cadenceName)
			if !archetypeAdmissible(a, cadenceName) {
				continue
			}
			switch shape.Intent {
			case intentOverdue:
				require.GreaterOrEqual(t, shape.NewestMin, period+calmMargin,
					"%s is admitted on %s, so its WHOLE newest-age range must clear the period by the margin", a, cadenceName)
			case intentCalm:
				require.GreaterOrEqual(t, period, shape.NewestMax+calmMargin,
					"%s is admitted on %s, so the period must clear its WHOLE newest-age range by the margin", a, cadenceName)
			case intentNeutral:
				// Touches last_contacted on no cadence, so every cadence admits it.
			}
		}
	}

	// The two boundary pairs. Both are draws AT the period, and every source stamps
	// its payload one to two hours before the drawn age, so at that draw the
	// contact is already overdue at the anchor — a ~1/265 and ~1/1441 designed-in
	// failure, not a wall-clock flake a pinned reference instant could remove.
	require.False(t, archetypeAdmissible(factory.ArchetypeMutualRegular, "biweekly"),
		"mutual-regular's newest age is an INCLUSIVE draw that reaches exactly biweekly's 14d period, and the source offset puts it over — do not relax this to >=")
	require.False(t, archetypeAdmissible(factory.ArchetypeBurstThenQuiet, "quarterly"),
		"burst-then-quiet's newest age is an INCLUSIVE draw that reaches exactly quarterly's 90d period, and the source offset puts it over — do not relax this to >=")

	// mutual-drifting straddles quarterly (70–112d against 90d), so its overdue-ness
	// there is INDETERMINATE rather than merely wrong-intent. Either way it is out.
	require.False(t, archetypeAdmissible(factory.ArchetypeMutualDrifting, "quarterly"),
		"mutual-drifting's 70–112d range straddles quarterly's 90d period, so its overdue-ness would be a coin flip per draw")

	// Non-vacuity: the rule must still admit something on every cadence, or the
	// assignment would silently degrade to never-contacted everywhere.
	for _, cadenceName := range archetypeCadences {
		var admitted int
		for _, a := range historyArchetypes {
			if archetypeAdmissible(a, cadenceName) {
				admitted++
			}
		}
		require.GreaterOrEqual(t, admitted, 2, "cadence %q must admit >=2 history archetypes", cadenceName)
	}
}

// --- the assignment ----------------------------------------------------------

// TestArchetypeAssignmentRungs walks the degradation ladder.
func TestArchetypeAssignmentRungs(t *testing.T) {
	t.Run("below_five_is_a_no_op", func(t *testing.T) {
		for n := 0; n < archetypeLadderReservedOnly; n++ {
			assignment := archetypeAssignment(n)
			require.Len(t, assignment, n)
			for i, a := range assignment {
				require.Equal(t, factory.ArchetypeNeverContacted, a,
					"n=%d i=%d: below the floor every slot is never-contacted, so the overlay drives no payload at all", n, i)
			}
		}
	})

	t.Run("reserved_only", func(t *testing.T) {
		for n := archetypeLadderReservedOnly; n < archetypeLadderFullAssignment; n++ {
			assignment := archetypeAssignment(n)
			counts := map[factory.Archetype]int{}
			for _, a := range assignment {
				counts[a]++
			}
			contactedOverdue := counts[factory.ArchetypeMutualDrifting] + counts[factory.ArchetypeDormant]
			require.Equal(t, 1, contactedOverdue,
				"n=%d: the reserved rung places exactly one contacted-and-overdue supplier", n)
			require.Equal(t, 1, counts[factory.ArchetypeOutboundHeavy],
				"n=%d: the reserved rung places exactly one outbound-heavy", n)
			require.Equal(t, n-2, counts[factory.ArchetypeNeverContacted],
				"n=%d: every other slot stays never-contacted", n)
			require.Equal(t, factory.ArchetypeNeverContacted, assignment[noMethodCatalogIndex],
				"n=%d: the no-method slot is reserved on every rung", n)
		}
	})

	t.Run("full_assignment", func(t *testing.T) {
		for _, n := range []int{archetypeLadderFullAssignment, 13, 18, 150} {
			assignment := archetypeAssignment(n)
			require.Len(t, assignment, n)
			present := map[factory.Archetype]int{}
			for i, a := range assignment {
				require.NotEmpty(t, a, "n=%d i=%d: every slot is assigned on the full rung", n, i)
				present[a]++
			}
			for _, a := range archetypeTieBreak {
				require.GreaterOrEqual(t, present[a], 1, "n=%d: every archetype has a cohort on the full rung (%s)", n, a)
			}
		}
	})
}

// TestArchetypeAssignmentRespectsCompatibility is the gate that keeps an
// indeterminate or wrong-intent placement from ever shipping: at EVERY catalog
// size, every slot's archetype must be admissible on that slot's cadence.
func TestArchetypeAssignmentRespectsCompatibility(t *testing.T) {
	for n := 1; n <= 150; n++ {
		for i, a := range archetypeAssignment(n) {
			slot := catalogSlot(i, n)
			require.True(t, archetypeAdmissible(a, slot.Cadence),
				"n=%d i=%d: %s is not admissible on a %s slot", n, i, a, slot.Cadence)
		}
	}
}

// TestArchetypeAssignmentReservesIndexThree pins the mechanical reservation: the
// no-method slot owns no identifier, so it can match no source payload and any
// other archetype would claim a cohort membership it cannot have.
func TestArchetypeAssignmentReservesIndexThree(t *testing.T) {
	for n := noMethodCatalogIndex + 1; n <= 150; n++ {
		require.True(t, catalogSlot(noMethodCatalogIndex, n).NoMethods,
			"n=%d: index %d is the frozen no-method slot", n, noMethodCatalogIndex)
		require.Equal(t, factory.ArchetypeNeverContacted, archetypeAssignment(n)[noMethodCatalogIndex],
			"n=%d: the no-method slot must be never-contacted", n)
	}
}

// TestArchetypeAssignmentMultiSample pins the arc's commitment that every
// history archetype carries at least TWO samples, so the population demonstrates
// jitter rather than mere presence.
//
// The floor is asserted from n >= 13, not from the ladder rung at 12, and the
// derivation is arithmetic rather than a preference: the no-method slot is
// structurally history-free whatever archetype it is given, leaving n − 1 usable
// slots, and six history archetypes at two samples each need twelve of them. At
// n = 12 exactly the floor is unsatisfiable over the frozen catalog, and
// archetypes add history rather than contacts, so it cannot be fixed by seeding
// another slot. Both shipping profiles (18 and 150) are above the threshold and
// no suite run uses 12. Do not "fix" this to 12.
func TestArchetypeAssignmentMultiSample(t *testing.T) {
	for n := 13; n <= 150; n++ {
		counts := map[factory.Archetype]int{}
		for _, a := range archetypeAssignment(n) {
			counts[a]++
		}
		for _, a := range historyArchetypes {
			require.GreaterOrEqual(t, counts[a], 2,
				"n=%d: %s must carry >=2 samples (six history archetypes x 2 need 12 of the n-1 usable slots)", n, a)
		}
	}
}

// TestArchetypeAssignmentIsDeterministic pins the whole overlay as a pure
// function of n. Every "least-used" tie-break resolves by a fixed archetype
// order, so no map iteration can leak into the result.
func TestArchetypeAssignmentIsDeterministic(t *testing.T) {
	for _, n := range catalogSlotSizes {
		first := archetypeAssignment(n)
		for run := 0; run < 5; run++ {
			require.Equal(t, first, archetypeAssignment(n), "n=%d: the assignment must be identical on every call", n)
		}
	}
}

// TestArchetypeAssignmentOverdueBand pins the overdue population: the absolute
// ceiling at every size, both bands on the full rung, both shipping profiles'
// exact totals, and the structural floor.
//
// TWO quantities are pinned, because they are genuinely different numbers. The
// RECOMPUTED population (PredictedCatalogOverdue) is what the assignment's budget
// steers, and the budget's 23% midpoint holds it in 20–27%. The ENDPOINT-visible
// population (PredictedCatalogOverduePersisted) is what GET /contacts/overdue
// returns once the forward-only contact_by write has had its say, and it is
// strictly smaller — that gap is what shipped unnoticed to staging as gh #751.
// The product-facing band is the endpoint one.
//
// Both bands' denominator is live(n) = n + 19, the DEV-knob non-catalog live
// count. It is exact at the small sizes the sweep spends most of its range on,
// and it is strictly CONSERVATIVE at large ones: a smaller denominator inflates
// the share, so the sweep tests against each ceiling rather than away from it.
// The budget formula's own midpoint constant is a target-picking device for two
// profiles at once and is deliberately NOT reused here — it puts n = 12 below the
// band. Neither model is ever an assertion about the world: the coverage test
// counts live and overdue contacts from the database.
func TestArchetypeAssignmentOverdueBand(t *testing.T) {
	const devKnobNonCatalogLive = 19

	for n := 1; n <= 150; n++ {
		total := PredictedCatalogOverdue(n) + PinnedOverdueFixtureCount
		endpointTotal := PredictedCatalogOverduePersisted(n) + PinnedOverdueFixtureCount
		require.LessOrEqual(t, total, OverdueCeiling,
			"n=%d: the overdue population must stay under the ceiling (the overdue tours refuse to run above their own, higher, capture cap)", n)
		require.LessOrEqual(t, endpointTotal, total,
			"n=%d: the endpoint-visible set is contact_by in the past, which the forward-only write makes a SUBSET of the recomputed set", n)

		if n < archetypeLadderFullAssignment {
			continue
		}
		share := 100.0 * float64(total) / float64(n+devKnobNonCatalogLive)
		require.GreaterOrEqual(t, share, 20.0, "n=%d: recomputed overdue share %.1f%% is below the budget's band", n, share)
		require.LessOrEqual(t, share, 27.0, "n=%d: recomputed overdue share %.1f%% is above the budget's band", n, share)

		endpointShare := 100.0 * float64(endpointTotal) / float64(n+devKnobNonCatalogLive)
		require.GreaterOrEqual(t, endpointShare, OverdueBandFloorPercent,
			"n=%d: endpoint-visible overdue share %.1f%% is below the product band", n, endpointShare)
		require.LessOrEqual(t, endpointShare, OverdueBandCeilingPercent,
			"n=%d: endpoint-visible overdue share %.1f%% is above the product band", n, endpointShare)
	}

	// The band's whole point is that it separates the two quantities: a gate that
	// silently reverted to recomputing overdue-ness has to FAIL it, at both
	// shipping sizes, or the band is decorative.
	for _, n := range []int{18, 150} {
		recomputedShare := 100.0 * float64(PredictedCatalogOverdue(n)+PinnedOverdueFixtureCount) / float64(n+devKnobNonCatalogLive)
		require.Greater(t, recomputedShare, OverdueBandCeilingPercent,
			"n=%d: the recomputed share (%.1f%%) must sit ABOVE the endpoint band's ceiling — otherwise the band cannot tell the two reads apart", n, recomputedShare)
	}

	// The two shipping sizes are pinned exactly, so a tuning change to the budget
	// or the placement ordering is a deliberate edit rather than a drift. The
	// endpoint totals are the numbers measured on the seeded worlds: 6 of 37 live
	// at dev and 31 of 181 at prod-shaped (the latter reproduces, to the contact,
	// what deployed staging returned in gh #751).
	require.Equal(t, 9, PredictedCatalogOverdue(18)+PinnedOverdueFixtureCount, "dev (n=18) recomputed overdue total")
	require.Equal(t, 40, PredictedCatalogOverdue(150)+PinnedOverdueFixtureCount, "prod-shaped (n=150) recomputed overdue total")
	require.Equal(t, 6, PredictedCatalogOverduePersisted(18)+PinnedOverdueFixtureCount, "dev (n=18) endpoint-visible overdue total")
	require.Equal(t, 31, PredictedCatalogOverduePersisted(150)+PinnedOverdueFixtureCount, "prod-shaped (n=150) endpoint-visible overdue total")

	// The structural FLOOR: backdated slots with no admissible calm archetype are
	// overdue whatever they receive, the no-method slot is reserved and backdated,
	// and the two pinned fixtures are always overdue. The budget cannot push below
	// this, so it must itself fit under the ceiling.
	for n := 1; n <= 150; n++ {
		floor := PinnedOverdueFixtureCount
		for i := 0; i < n; i++ {
			slot := catalogSlot(i, n)
			if i == noMethodCatalogIndex && n > noMethodCatalogIndex {
				if catalogSlotNativelyOverdue(i, n) {
					floor++
				}
				continue
			}
			if slot.Kind == slotBackdated && len(admissibleArchetypes(intentCalm, slot.Cadence)) == 0 && catalogSlotNativelyOverdue(i, n) {
				floor++
			}
		}
		require.LessOrEqual(t, floor, OverdueCeiling,
			"n=%d: the structural overdue floor (%d) must fit under the ceiling — no budget can lower it", n, floor)
		require.LessOrEqual(t, floor, PredictedCatalogOverdue(n)+PinnedOverdueFixtureCount,
			"n=%d: the predicted total can never be below the structural floor", n)
	}
}

// TestPersistedOverduePredictionIsNotBoundarySensitive proves the one claim
// PredictedCatalogOverduePersisted rests on that is not visible in its own body:
// that a slot which is not NATIVELY overdue cannot become endpoint-overdue for
// any PRNG draw inside its creation window.
//
// The prediction is a pure function of (i, n, archetype), but created_at is a
// DRAW — slotRecent picks a real age anywhere inside catalogRecentWindow. That is
// only sound while every possible draw leaves created_at + period in the future,
// because contact_by is seeded at creation and the forward-only write never
// lowers it. So assert the separation directly, with a margin, rather than
// trusting that 48 hours is obviously less than a week: a future edit that
// widened the recent window past a cadence period, or put a sub-window cadence in
// the recent pool, would make the prediction quietly PRNG-dependent, and this is
// where that fails.
func TestPersistedOverduePredictionIsNotBoundarySensitive(t *testing.T) {
	checked := 0
	for n := 1; n <= 150; n++ {
		for i := 0; i < n; i++ {
			slot := catalogSlot(i, n)
			if slot.Kind == slotBackdated {
				continue
			}
			period, ok := productionCadencePeriod(slot.Cadence)
			require.True(t, ok, "n=%d i=%d: slot cadence %q must be a recognized cadence", n, i, slot.Cadence)
			require.Less(t, slot.CreatedAgeBound+calmMargin, period,
				"n=%d i=%d: a %s slot may be created up to %s before the anchor on a %s cadence (period %s) — a draw at that bound puts created_at + period in the PAST, so the endpoint-overdue prediction stops being a pure function of the assignment",
				n, i, slot.Cadence, slot.CreatedAgeBound, slot.Cadence, period)
			checked++

			// The conjunction's consequence, stated as behaviour: whatever archetype
			// lands here, the endpoint cannot see this slot as overdue.
			for _, a := range archetypeTieBreak {
				require.False(t, archetypeLeavesSlotPersistedOverdue(i, n, a),
					"n=%d i=%d: a non-backdated slot carrying %s must not be predicted endpoint-overdue", n, i, a)
			}
		}
	}
	require.NotZero(t, checked, "the sweep must actually reach non-backdated slots, or it proves nothing")
}

// TestArchetypeAssignmentPreservesC3Floors re-asserts the coherence floors the
// catalog carried BEFORE archetypes, over the population the overlay produces.
//
// The floors are read over the union of catalog slots and the pinned tour
// fixtures, because two of them are not catalog-supplied at every size — see the
// zero-visible-task case below.
func TestArchetypeAssignmentPreservesC3Floors(t *testing.T) {
	const diversityFloorAge = 7 * 24 * time.Hour

	for _, n := range []int{9, 12, 13, 18, 150} {
		assignment := archetypeAssignment(n)

		// Overdue-cohort diversity (the dashboard's urgency tiers): among rows with a
		// cadence, a NULL last_contacted and a created_at older than the floor, >=3
		// distinct created-ages and >=2 distinct cadences. Only a NEUTRAL archetype
		// leaves last_contacted NULL, so a slot that took a contacting archetype has
		// left this set.
		ages := map[time.Duration]bool{}
		cadences := map[string]bool{}
		for _, fixture := range pinnedOverdueFixtures {
			ages[fixture.createdAge] = true
			cadences[fixture.cadence] = true
		}
		for i, a := range assignment {
			slot := catalogSlot(i, n)
			if archetypeShapes[a].Intent != intentNeutral || slot.Kind != slotBackdated || slot.CreatedAgeBound <= diversityFloorAge {
				continue
			}
			ages[slot.CreatedAgeBound] = true
			cadences[slot.Cadence] = true
		}
		require.GreaterOrEqual(t, len(ages), 3, "n=%d: the never-connected backdated set spans >=3 distinct created-ages", n)
		require.GreaterOrEqual(t, len(cadences), 2, "n=%d: the never-connected backdated set spans >=2 distinct cadences", n)

		// >=1 never-connected contact on a RECENT-creation slot, a cohort distinct
		// from the backdated one. The pinned no-activity fixture supplies it
		// independently at every size, but the catalog must not lose it either.
		if n >= archetypeLadderFullAssignment {
			recentNeverConnected := 0
			for i, a := range assignment {
				if catalogSlot(i, n).Kind == slotRecent && archetypeShapes[a].Intent == intentNeutral {
					recentNeverConnected++
				}
			}
			require.GreaterOrEqual(t, recentNeverConnected, 1,
				"n=%d: >=1 recent-creation slot must stay never-connected (both neutral archetypes leave last_contacted NULL)", n)
		}

		// >=1 never-contacted contact OUTSIDE the visible-task-spread cohort
		// (creation indices 1-3) carrying zero product-visible tasks. At n = 12 and
		// n = 13 EXACTLY, every usable slot takes a history archetype and the only
		// catalog never-contacted is the no-method slot — which is inside that
		// cohort — so at those two sizes the pinned fxnoactivity fixture is the SOLE
		// supplier. Asserting the union is what keeps a future fixture edit from
		// silently stranding the floor.
		catalogSupplied := 0
		for i, a := range assignment {
			if i >= 1 && i <= 3 {
				continue
			}
			if a == factory.ArchetypeNeverContacted {
				catalogSupplied++
			}
		}
		fixtureSupplied := 1 // fxnoactivity: never-connected, recent, zero visible tasks
		require.GreaterOrEqual(t, catalogSupplied+fixtureSupplied, 1,
			"n=%d: the zero-visible-task floor must be supplied by the catalog or by fxnoactivity", n)
		if n == 12 || n == 13 {
			require.Zero(t, catalogSupplied,
				"n=%d: at this size the catalog cannot supply the floor and fxnoactivity is the sole supplier — if this changes, the note above is stale", n)
		}
	}
}

// --- payload construction ----------------------------------------------------

// archetypeUnitSlots builds the catalog slots for an n-contact catalog without a
// database: the same frozen options the profile uses, with a synthetic contact id
// standing in for the seeded row.
func archetypeUnitSlots(gen *factory.Generator, n int) []archetypeSlot {
	slots := make([]archetypeSlot, 0, n)
	for i := 0; i < n; i++ {
		spec := gen.Contact(catalogOptionsFor(i, n, gen.Anchor(), gen.Prefix())...)
		slots = append(slots, archetypeSlot{Index: i, ContactID: uuid.New(), Spec: spec})
	}
	return slots
}

func gmailItemOutbound(it replay.GmailBatchItem) bool {
	for _, label := range it.Spec.Message.LabelIds {
		if label == "SENT" {
			return true
		}
	}
	return false
}

func gchatItemOutbound(it replay.GChatBatchItem) bool {
	return it.Spec.EmailByUser[it.Spec.Message.Sender.Name] == it.Spec.AccountID
}

func gchatItemCreatedAt(t *testing.T, it replay.GChatBatchItem) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, it.Spec.Message.CreateTime)
	require.NoError(t, err)
	return at
}

// TestArchetypePayloadsAreAdapterLegal builds the whole block's payloads across
// many namespaces and asserts the shape the batch adapters' preflight demands.
// Every rejection those adapters raise is a named error raised BEFORE anything is
// driven, so a mapper that produced an illegal batch would fail the seed rather
// than corrupt it — but it would fail it in the slow lane, at reseed time, on a
// draw that happened to hit the bad case. This is the fast-lane equivalent.
func TestArchetypePayloadsAreAdapterLegal(t *testing.T) {
	const namespaces = 60
	// The determinism run, the prod coverage run, the ladder rung, the multi-sample
	// threshold and the dev profile. 150 is covered by the coverage sweep rather
	// than here: it multiplies the cost without reaching a new shape.
	sizes := []int{5, 9, 12, 13, 18}

	for _, n := range sizes {
		for s := 0; s < namespaces; s++ {
			gen := factory.NewGenerator(factory.DefaultSeed, "payloadunit"+strconv.Itoa(n)+"x"+strconv.Itoa(s))
			batches, err := buildArchetypeBatches(gen, archetypeUnitSlots(gen, n), n)
			require.NoError(t, err, "n=%d ns=%d", n, s)

			total := len(batches.GCal) + len(batches.GChat)
			for _, bucket := range batches.GmailBuckets {
				total += len(bucket)
			}
			perSlot := 0
			for _, sample := range batches.Samples {
				perSlot += sample.Payloads
			}
			require.Equal(t, perSlot, total,
				"n=%d ns=%d: every timeline entry must reach exactly one batch item", n, s)

			identifiers := map[string]bool{}
			requireUnique := func(id string) {
				require.False(t, identifiers[id], "n=%d ns=%d: duplicate source identifier %q — the adapters reject a batch carrying one", n, s, id)
				identifiers[id] = true
			}

			// --- calendar -----------------------------------------------------------
			var prevStart time.Time
			for i, it := range batches.GCal {
				require.Equal(t, factory.MatchSeeded, it.Spec.Intent, "calendar items must be MatchSeeded")
				requireUnique(it.Spec.GcalEventID)
				start, err := time.Parse(time.RFC3339, it.Spec.Event.Start.DateTime)
				require.NoError(t, err)
				if i > 0 {
					require.False(t, start.Before(prevStart), "n=%d ns=%d: calendar items must be oldest-first", n, s)
				}
				prevStart = start
			}

			// --- mail ---------------------------------------------------------------
			mailPairs := map[int][]replay.GmailBatchItem{}
			for _, bucket := range batches.GmailBuckets {
				require.NotEmpty(t, bucket, "an empty bucket must never be driven — the adapters reject a zero-item batch")
				var oldest, newest time.Time
				var prev time.Time
				for i, it := range bucket {
					require.Equal(t, factory.MatchSeeded, it.Spec.Intent, "mail items must be MatchSeeded")
					requireUnique(it.Spec.ExternalID)
					sentAt := time.UnixMilli(it.Spec.Message.InternalDate).UTC()
					if i == 0 || sentAt.Before(oldest) {
						oldest = sentAt
					}
					if i == 0 || sentAt.After(newest) {
						newest = sentAt
					}
					if i > 0 {
						require.False(t, sentAt.Before(prev), "n=%d ns=%d: mail must be oldest-first inside a bucket", n, s)
					}
					prev = sentAt
					if it.PairKey != 0 {
						mailPairs[it.PairKey] = append(mailPairs[it.PairKey], it)
					}
				}
				require.Less(t, newest.Sub(oldest), 150*24*time.Hour,
					"n=%d ns=%d: a mail bucket must stay inside one sync's reach — the adapter rejects a wider batch by design", n, s)
			}
			for key, members := range mailPairs {
				require.Len(t, members, 2, "mail pair %d must have exactly two members", key)
				require.NotEqual(t, gmailItemOutbound(members[0]), gmailItemOutbound(members[1]),
					"mail pair %d members must differ in direction", key)
				require.Equal(t, members[0].Spec.Message.InternalDate, members[1].Spec.Message.InternalDate,
					"mail pair %d must share an INSTANT: the aggregation key includes a local day, so any nonzero gap can straddle local midnight", key)
				require.Equal(t, members[0].Spec.Message.ThreadId, members[1].Spec.Message.ThreadId,
					"mail pair %d must share a thread — the factory mints a fresh one per call, so the caller clones", key)
			}

			// --- chat ---------------------------------------------------------------
			chatPairs := map[int][]replay.GChatBatchItem{}
			spaceOf := map[uuid.UUID]map[string]bool{}
			var prevCreated time.Time
			for i, it := range batches.GChat {
				require.Equal(t, factory.MatchSeeded, it.Spec.Intent, "chat items must be MatchSeeded")
				requireUnique(it.Spec.ExternalID)
				createdAt := gchatItemCreatedAt(t, it)
				if i > 0 {
					require.False(t, createdAt.Before(prevCreated), "n=%d ns=%d: chat items must be oldest-first", n, s)
				}
				prevCreated = createdAt
				if spaceOf[it.ContactID] == nil {
					spaceOf[it.ContactID] = map[string]bool{}
				}
				spaceOf[it.ContactID][it.Spec.SpaceName] = true
				if it.PairKey != 0 {
					chatPairs[it.PairKey] = append(chatPairs[it.PairKey], it)
				}
			}
			for key, members := range chatPairs {
				require.Len(t, members, 2, "chat pair %d must have exactly two members", key)
				outboundFirst := gchatItemOutbound(members[0])
				require.True(t, outboundFirst && !gchatItemOutbound(members[1]),
					"chat pair %d must be an OUTBOUND then an inbound: this path orders eligible rows only by sent time, so the ordering has to be decided by construction", key)
				require.True(t, gchatItemCreatedAt(t, members[0]).Before(gchatItemCreatedAt(t, members[1])),
					"chat pair %d outbound half must be strictly older", key)
				require.Equal(t, members[0].Spec.SpaceName, members[1].Spec.SpaceName,
					"chat pair %d must share a space — a pair across two spaces can never bridge", key)
			}
			// A contact's chat history is ONE conversation: the archetypes that use chat
			// hold a single space for the whole timeline, which is also what keeps a
			// burst inside the provider's page budget.
			for contactID, spaces := range spaceOf {
				require.Len(t, spaces, 1, "n=%d ns=%d: contact %s must carry exactly one chat space", n, s, contactID)
			}

			// Pair keys are BLOCK-global: the adapters group by pair key across the
			// whole batch, so two contacts each carrying the timeline-local key 1 would
			// form a four-member group and be rejected.
			for key := range mailPairs {
				require.NotContains(t, chatPairs, key, "pair key %d is reused across two sources", key)
			}
		}
	}
}

// TestArchetypePayloadsSkipEmptySources pins the skip-empty rule at the size it
// actually bites. On the reserved rung the assignment is mutual-drifting plus
// outbound-heavy plus never-contacted, none of which emits a chat entry for an
// email-only contact, so the chat batch is EMPTY at every 5 <= n < 12 — which is
// exactly the configuration the determinism run (5) and the prod coverage run (9)
// use. The adapters reject a zero-item batch by preflight, so driving one would
// fail the seed.
func TestArchetypePayloadsSkipEmptySources(t *testing.T) {
	for n := archetypeLadderReservedOnly; n < archetypeLadderFullAssignment; n++ {
		gen := factory.NewGenerator(factory.DefaultSeed, "emptyunit"+strconv.Itoa(n))
		batches, err := buildArchetypeBatches(gen, archetypeUnitSlots(gen, n), n)
		require.NoError(t, err)
		require.Empty(t, batches.GChat, "n=%d: the reserved rung emits no chat payload at all", n)
		require.NotEmpty(t, batches.GCal, "n=%d: the reserved rung's contacted-and-overdue supplier emits meetings", n)
		require.NotEmpty(t, batches.GmailBuckets, "n=%d: the reserved rung's outbound-heavy slot emits mail", n)
	}

	// Below the reserved rung the whole overlay is a no-op.
	for n := 1; n < archetypeLadderReservedOnly; n++ {
		gen := factory.NewGenerator(factory.DefaultSeed, "emptyunitlow"+strconv.Itoa(n))
		batches, err := buildArchetypeBatches(gen, archetypeUnitSlots(gen, n), n)
		require.NoError(t, err)
		require.True(t, batches.empty(), "n=%d: below the floor the overlay drives nothing", n)
		require.Empty(t, batches.GCal)
		require.Empty(t, batches.GmailBuckets)
		require.Empty(t, batches.GChat)
	}
}

// TestArchetypePayloadsRejectUnmatchableSource proves the method-awareness guard
// fires rather than stranding a row. A contact the frozen catalog cannot produce
// today — one owning a telegram handle — resolves the chat role to telegram,
// which no catalog replay path carries. The mapper must refuse it by NAME rather
// than build a payload nothing in the block will drive, because an unmatchable
// source surfaces thirty seconds later as a settle timeout blaming the wrong
// thing.
func TestArchetypePayloadsRejectUnmatchableSource(t *testing.T) {
	const n = 18
	gen := factory.NewGenerator(factory.DefaultSeed, "unmatchableunit")
	spec := gen.Contact(factory.WithEmail(), factory.WithTelegram(), factory.WithCadence("weekly"))

	chatBearing := -1
	for i := 0; i < n; i++ {
		switch ArchetypeForIndex(i, n) {
		case factory.ArchetypeDormant, factory.ArchetypeBurstThenQuiet:
			chatBearing = i
		}
		if chatBearing >= 0 {
			break
		}
	}
	require.GreaterOrEqual(t, chatBearing, 0, "n=%d must place a chat-bearing archetype somewhere", n)

	_, err := buildArchetypeBatches(gen, []archetypeSlot{{Index: chatBearing, ContactID: uuid.New(), Spec: spec}}, n)
	require.ErrorIs(t, err, errArchetypeUnsupportedSource,
		"a telegram-bearing contact resolves the chat role to telegram, which the catalog replay block does not drive")
}

// TestArchetypePairValidationRejectsMalformedPairs watches the pair contract
// fail. Each case is a shape a future generator change could produce, and each
// one silently breaks a different thing downstream: a mis-sized group has no
// defined generation split, a same-direction group promotes nothing, a mail pair
// with a gap can straddle local midnight, and a chat pair in the wrong order
// becomes two one-sided sessions instead of one mutual.
func TestArchetypePairValidationRejectsMalformedPairs(t *testing.T) {
	mail := func(age time.Duration, outbound bool) factory.TimelineEntry {
		return factory.TimelineEntry{Source: factory.SourceEmail, Age: age, Outbound: outbound, PairKey: 1}
	}
	chatEntry := func(age time.Duration, outbound bool) factory.TimelineEntry {
		return factory.TimelineEntry{Source: factory.SourceGChat, Age: age, Outbound: outbound, PairKey: 1}
	}

	cases := map[string][]factory.TimelineEntry{
		"one_member":         {mail(10*24*time.Hour, true)},
		"three_members":      {mail(10*24*time.Hour, true), mail(10*24*time.Hour, false), mail(10*24*time.Hour, false)},
		"same_direction":     {mail(10*24*time.Hour, true), mail(10*24*time.Hour, true)},
		"mail_gap":           {mail(10*24*time.Hour+time.Hour, true), mail(10*24*time.Hour, false)},
		"chat_equal_age":     {chatEntry(10*24*time.Hour, true), chatEntry(10*24*time.Hour, false)},
		"chat_inbound_older": {chatEntry(10*24*time.Hour, false), chatEntry(10*24*time.Hour+6*time.Hour, true)},
		// A pair whose halves live on different sources has no coherent timing rule
		// at all — mail promotes on a shared thread and one local day, chat on the
		// reply bridge in one space — so neither half can promote the other.
		"two_sources": {mail(10*24*time.Hour, true), chatEntry(10*24*time.Hour, false)},
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateArchetypePairs(0, factory.ArchetypeDormant, map[int][]factory.TimelineEntry{1: members})
			require.ErrorIs(t, err, errArchetypePairMalformed)
		})
	}

	// The two legal shapes must pass, or the rejections above are vacuous.
	require.NoError(t, validateArchetypePairs(0, factory.ArchetypeMutualRegular,
		map[int][]factory.TimelineEntry{1: {mail(10*24*time.Hour, true), mail(10*24*time.Hour, false)}}))
	require.NoError(t, validateArchetypePairs(0, factory.ArchetypeDormant,
		map[int][]factory.TimelineEntry{1: {chatEntry(10*24*time.Hour+6*time.Hour, true), chatEntry(10*24*time.Hour, false)}}))
}

// TestExpectedInteractionsMatchesArchetypeShape pins the payload → interaction
// collapse formula against each archetype's actual timeline structure, across
// many namespaces. The formula is what the seed records as its expectation and
// what the end-to-end test compares the database against, so a generator change
// that altered the collapse must fail in this fast lane rather than only in the
// slow one.
func TestExpectedInteractionsMatchesArchetypeShape(t *testing.T) {
	const namespaces = 400
	caps := factory.MethodSet{Email: true}

	for s := 0; s < namespaces; s++ {
		gen := factory.NewGenerator(factory.DefaultSeed, "collapseunit"+strconv.Itoa(s))
		for _, a := range archetypeTieBreak {
			tl := gen.TimelineFor(a, caps)
			var gcal, mail, chatMsgs int
			pairs := map[int]bool{}
			for _, e := range tl.Entries {
				switch e.Source {
				case factory.SourceGCal:
					gcal++
				case factory.SourceEmail:
					mail++
				default:
					chatMsgs++
				}
				if e.PairKey != 0 {
					pairs[e.PairKey] = true
				}
			}
			got := expectedArchetypeInteractions(tl)

			switch a {
			case factory.ArchetypeMutualRegular:
				// M meetings 21–35 days apart, each its own row, plus ONE correspondence
				// pair collapsing to a single mutual.
				require.Equal(t, 2, mail)
				require.Len(t, pairs, 1)
				require.Equal(t, gcal+1, got)
			case factory.ArchetypeMutualDrifting:
				// The same series, stopped, with one or two correspondence pairs.
				require.Equal(t, 2*len(pairs), mail)
				require.Equal(t, gcal+len(pairs), got)
			case factory.ArchetypeDormant:
				// M meetings plus a closing chat exchange six hours apart in one space,
				// which the reply bridge promotes into a single mutual.
				require.Equal(t, 2, chatMsgs)
				require.Len(t, pairs, 1)
				require.Equal(t, gcal+1, got)
			case factory.ArchetypeOutboundHeavy, factory.ArchetypeInboundOnly:
				// Distinct threads five or more days apart: nothing collapses.
				require.Empty(t, pairs)
				require.Zero(t, gcal)
				require.Equal(t, mail, got)
			case factory.ArchetypeBurstThenQuiet:
				// K messages in ONE space: the opening three inside one burst window
				// become one session (−2), and the closing pair promotes into one (−1).
				require.Zero(t, gcal)
				require.Zero(t, mail)
				require.Len(t, pairs, 1)
				require.Equal(t, chatMsgs-3, got,
					"a burst of %d messages must collapse to %d rows — the assertion that proves the burst window is exercised rather than K unrelated messages seeded",
					chatMsgs, chatMsgs-3)
			case factory.ArchetypeNeverContacted:
				require.Empty(t, tl.Entries)
				require.Zero(t, got)
			}

			if a != factory.ArchetypeNeverContacted {
				require.LessOrEqual(t, got, len(tl.Entries), "%s: the collapse can never produce MORE rows than payloads", a)
				require.Positive(t, got, "%s must land at least one row", a)
			}
		}
	}
}

// TestArchetypeCalendarPairKeyIsRejected watches the guard the calendar batch
// item type explicitly asks callers to write. GCalBatchItem has NO PairKey field
// — a matched calendar event is always mutual, so calendar carries no promotion
// mechanic — and its own comment says a caller mapping a plan that CAN carry a
// pair key must ASSERT it is unset rather than drop it silently.
//
// The generator emits no such timeline today, which is exactly why the branch
// needs a hand-authored one: a guard nothing can exercise is indistinguishable
// from one that always passes.
func TestArchetypeCalendarPairKeyIsRejected(t *testing.T) {
	gen := factory.NewGenerator(factory.DefaultSeed, "gcalpairunit")
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
	slots := []archetypeSlot{{Index: 0, ContactID: uuid.New(), Spec: spec}}

	timeline := factory.Timeline{
		Archetype: factory.ArchetypeMutualRegular,
		Entries: []factory.TimelineEntry{
			{Source: factory.SourceGCal, Age: 30 * 24 * time.Hour, PairKey: 1},
			{Source: factory.SourceGCal, Age: 10 * 24 * time.Hour, PairKey: 1},
		},
	}
	_, err := buildArchetypeBatchesFrom(gen, slots, func(archetypeSlot) (factory.Archetype, factory.Timeline, error) {
		return factory.ArchetypeMutualRegular, timeline, nil
	})
	require.ErrorIs(t, err, errArchetypeCalendarPairKey,
		"a stated promotion intent must be refused by name, not dropped because the item type has nowhere to put it")
}

// TestArchetypeChatCloneRejectsUnresolvedMembership watches the mapper's one
// otherwise-SILENT failure. A cloned chat message whose space membership does not
// yield both the connected account and a peer would be built with an empty
// sender; the provider reads that as an INBOUND from an empty address, and the
// batch ownership preflight skips empty-addressed items by design — so an
// intended outbound would strand and time out a gate blaming the wrong thing.
//
// burst-then-quiet's opening and filler clones carry no PairKey, so the
// malformed-pair check would not catch them either.
func TestArchetypeChatCloneRejectsUnresolvedMembership(t *testing.T) {
	gen := factory.NewGenerator(factory.DefaultSeed, "cloneunit")
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence("annual"))
	base := gen.GChatMessage(spec, factory.MatchSeeded, factory.WithMessageAge(20*24*time.Hour))

	t.Run("resolved_membership_clones", func(t *testing.T) {
		clone, err := cloneArchetypeGChatMessage(base, "arch-1", true, gen.Anchor().Add(-30*24*time.Hour))
		require.NoError(t, err)
		require.Equal(t, base.SpaceName, clone.SpaceName)
		require.Equal(t, base.AccountID, clone.EmailByUser[clone.Message.Sender.Name],
			"an outbound clone must be sent BY the connected account")
	})

	t.Run("missing_peer", func(t *testing.T) {
		broken := base
		broken.EmailByUser = map[string]string{}
		for user := range base.EmailByUser {
			broken.EmailByUser[user] = base.AccountID // every member resolves to "me"
		}
		_, err := cloneArchetypeGChatMessage(broken, "arch-1", true, gen.Anchor())
		require.ErrorIs(t, err, errArchetypeChatCloneUnresolved)
	})

	t.Run("missing_me", func(t *testing.T) {
		broken := base
		broken.AccountID = "someone-else@synthetic.example"
		_, err := cloneArchetypeGChatMessage(broken, "arch-1", false, gen.Anchor())
		require.ErrorIs(t, err, errArchetypeChatCloneUnresolved)
	})

	t.Run("nil_member_entries", func(t *testing.T) {
		broken := base
		broken.Members = []*chat.Membership{nil, {State: "JOINED"}}
		_, err := cloneArchetypeGChatMessage(broken, "arch-1", false, gen.Anchor())
		require.ErrorIs(t, err, errArchetypeChatCloneUnresolved)
	})
}

// TestArchetypeSlotOutOfRangeIsRejected pins the other half of the oracle
// contract: the assignment is a WHOLE-CATALOG computation, so applying it to a
// slot that is not in that catalog is a caller error, not something to paper over
// with a default archetype.
func TestArchetypeSlotOutOfRangeIsRejected(t *testing.T) {
	gen := factory.NewGenerator(factory.DefaultSeed, "rangeslotunit")
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
	_, err := buildArchetypeBatches(gen, []archetypeSlot{{Index: 18, ContactID: uuid.New(), Spec: spec}}, 18)
	require.ErrorIs(t, err, errArchetypeSlotOutOfRange)

	require.Empty(t, ArchetypeForIndex(18, 18),
		"the oracle must not answer with a valid archetype outside the catalog — a mis-derived index has to disagree loudly")
	require.Empty(t, ArchetypeForIndex(-1, 18))
}
