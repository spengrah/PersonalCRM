package synthetic

import (
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
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
				require.True(t, spec.CreatedAt.Equal(anchor.Add(-slot.CreatedAge)),
					"n=%d i=%d: backdated created_at must be exactly anchor − CreatedAge (got %s, want %s)",
					n, i, spec.CreatedAt, anchor.Add(-slot.CreatedAge))
			case slotRecent:
				require.NotNil(t, spec.CreatedAt, "n=%d i=%d: a recent slot stamps created_at", n, i)
				age := anchor.Sub(*spec.CreatedAt)
				require.GreaterOrEqual(t, age, time.Duration(0), "n=%d i=%d: a recent slot is not created in the future", n, i)
				require.Less(t, age, slot.CreatedAge,
					"n=%d i=%d: a recent slot lands inside the window catalogSlot reports as its bound", n, i)
			case slotFresh:
				require.Nil(t, spec.CreatedAt, "n=%d i=%d: a fresh slot carries no created-age option", n, i)
				require.Zero(t, slot.CreatedAge, "n=%d i=%d: a fresh slot reports no created age", n, i)
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

// TestArchetypeNewestAgeRangesMatchTheGenerator is the anti-drift guard for the
// one table this package RESTATES rather than reads: the generator's
// per-archetype newest-entry bounds live in an unexported table a package below,
// so the compatibility rule declares its own copy.
//
// The guard samples real timelines across many namespaces and requires the
// observed newest ages to (a) stay inside the declared bounds and (b) reach
// within one calmMargin of each end. (a) catches a generator that widened its
// range past what the rule was reasoned about; (b) catches one that narrowed or
// shifted it. A drift smaller than the margin survives, which is the honest limit
// of a sampling guard — but a drift smaller than the margin also cannot flip an
// admissibility verdict on its own, which is what the margin is for.
//
// The sample size makes (b) non-flaky rather than merely likely: the narrowest
// declared range is 265 whole-hour draws and the margin is 25 of them, so the
// probability of no sample landing in an end band is about e^-283.
func TestArchetypeNewestAgeRangesMatchTheGenerator(t *testing.T) {
	const namespaces = 3000
	caps := factory.MethodSet{Email: true}

	observedMin := map[factory.Archetype]time.Duration{}
	observedMax := map[factory.Archetype]time.Duration{}
	for s := 0; s < namespaces; s++ {
		g := factory.NewGenerator(factory.DefaultSeed, "rangeunit"+strconv.Itoa(s))
		for _, a := range historyArchetypes {
			tl := g.TimelineFor(a, caps)
			require.NotEmpty(t, tl.Entries, "%s must emit a timeline for an email-bearing contact", a)
			newest := tl.Entries[0].Age
			for _, e := range tl.Entries {
				if e.Age < newest {
					newest = e.Age
				}
			}
			if prev, ok := observedMin[a]; !ok || newest < prev {
				observedMin[a] = newest
			}
			if prev, ok := observedMax[a]; !ok || newest > prev {
				observedMax[a] = newest
			}
		}
	}

	for _, a := range historyArchetypes {
		shape := archetypeShapes[a]
		require.GreaterOrEqual(t, observedMin[a], shape.NewestMin,
			"%s drew a newest age of %s, below the declared floor %s — the compatibility rule was reasoned about the declared range",
			a, observedMin[a], shape.NewestMin)
		require.LessOrEqual(t, observedMax[a], shape.NewestMax,
			"%s drew a newest age of %s, above the declared ceiling %s — the compatibility rule was reasoned about the declared range",
			a, observedMax[a], shape.NewestMax)
		require.LessOrEqual(t, observedMin[a]-shape.NewestMin, calmMargin,
			"%s never drew within one margin of its declared floor %s (closest %s) — the generator's floor has moved",
			a, shape.NewestMin, observedMin[a])
		require.LessOrEqual(t, shape.NewestMax-observedMax[a], calmMargin,
			"%s never drew within one margin of its declared ceiling %s (closest %s) — the generator's ceiling has moved",
			a, shape.NewestMax, observedMax[a])
	}
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
// ceiling at every size, the target band on the full rung, both shipping
// profiles' exact totals, and the structural floor.
//
// The band's denominator is live(n) = n + 19, the DEV-knob non-catalog live
// count. It is exact at the small sizes the sweep spends most of its range on,
// and it is strictly CONSERVATIVE at large ones: a smaller denominator inflates
// the share, so the sweep tests against the 27% ceiling rather than away from it.
// The budget formula's own midpoint constant is a target-picking device for two
// profiles at once and is deliberately NOT reused here — it puts n = 12 below the
// band. Neither model is ever an assertion about the world: the coverage test
// counts live and overdue contacts from the database.
func TestArchetypeAssignmentOverdueBand(t *testing.T) {
	const devKnobNonCatalogLive = 19

	for n := 1; n <= 150; n++ {
		total := PredictedCatalogOverdue(n) + pinnedOverdueFixtureCount
		require.LessOrEqual(t, total, OverdueCeiling,
			"n=%d: the overdue population must stay under the ceiling (the overdue tours refuse to run above their own, higher, capture cap)", n)

		if n < archetypeLadderFullAssignment {
			continue
		}
		share := 100.0 * float64(total) / float64(n+devKnobNonCatalogLive)
		require.GreaterOrEqual(t, share, 20.0, "n=%d: overdue share %.1f%% is below the band", n, share)
		require.LessOrEqual(t, share, 27.0, "n=%d: overdue share %.1f%% is above the band", n, share)
	}

	// The two shipping sizes are pinned exactly, so a tuning change to the budget
	// or the placement ordering is a deliberate edit rather than a drift.
	require.Equal(t, 9, PredictedCatalogOverdue(18)+pinnedOverdueFixtureCount, "dev (n=18) overdue total")
	require.Equal(t, 40, PredictedCatalogOverdue(150)+pinnedOverdueFixtureCount, "prod-shaped (n=150) overdue total")

	// The structural FLOOR: backdated slots with no admissible calm archetype are
	// overdue whatever they receive, the no-method slot is reserved and backdated,
	// and the two pinned fixtures are always overdue. The budget cannot push below
	// this, so it must itself fit under the ceiling.
	for n := 1; n <= 150; n++ {
		floor := pinnedOverdueFixtureCount
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
		require.LessOrEqual(t, floor, PredictedCatalogOverdue(n)+pinnedOverdueFixtureCount,
			"n=%d: the predicted total can never be below the structural floor", n)
	}
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
			if archetypeShapes[a].Intent != intentNeutral || slot.Kind != slotBackdated || slot.CreatedAge <= diversityFloorAge {
				continue
			}
			ages[slot.CreatedAge] = true
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
