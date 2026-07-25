package synthetic

import (
	"sort"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/synthetic/factory"
)

// --- the frozen catalog slot model ------------------------------------------
//
// The catalog's creation ORDER and per-index spec are frozen: committed
// assertions hard-code catalog indices, and the archetype assignment below is an
// index-keyed OVERLAY over them, never a reshuffle. catalogOptionsFor is the
// single authority for what a slot is, but it returns builder OPTIONS — the only
// way to learn a slot's cadence or created-age from it is to run it through the
// generator, which draws PRNG.
//
// catalogSlot reports the same per-index spec as a pure function, derived from
// the SAME three tables catalogOptionsFor reads (catalogOverdueLadder,
// catalogRecentCadences / catalogNeverContactedCadences, and the no-method
// branch). It is a second reader of those tables, not a second copy of them, and
// a unit test builds every slot through catalogOptionsFor and requires the two
// to agree — so a future edit to either side fails immediately rather than
// silently drifting the assignment away from the world it is assigning over.

// catalogSlotKind classifies a catalog index by its CREATED-AGE shape, which is
// what decides whether the slot is overdue on its own.
type catalogSlotKind int

const (
	// slotBackdated is created far in the past (the overdue ladder). It is the
	// only kind that is overdue with an empty timeline.
	slotBackdated catalogSlotKind = iota
	// slotRecent is created within the last catalogRecentWindow.
	slotRecent
	// slotFresh is created at the anchor (no created-age option at all).
	slotFresh
)

// catalogSlotSpec is one frozen catalog index's shape.
type catalogSlotSpec struct {
	Kind    catalogSlotKind
	Cadence string
	// CreatedAge is how far before the anchor the contact is created. Exact for
	// slotBackdated; for slotRecent it is the WINDOW (the true age is a PRNG draw
	// inside it), which is the conservative bound for an overdue prediction since a
	// larger age can only make a contact more overdue. Zero for slotFresh.
	CreatedAge time.Duration
	// NoMethods marks the no-method slot, which owns no identifier and can
	// therefore match no source payload at all.
	NoMethods bool
}

// catalogSlot reports the frozen spec of catalog index i in a catalog of n.
func catalogSlot(i, n int) catalogSlotSpec {
	// The no-method bucket draws its (cadence, created-age) from the overdue
	// ladder by the same rotation as any other i%3==0 slot; only the methods
	// differ.
	if i == 3 && n > 3 {
		pair := catalogOverdueLadder[(i/3)%len(catalogOverdueLadder)]
		return catalogSlotSpec{Kind: slotBackdated, Cadence: pair.cadence, CreatedAge: pair.createdAge, NoMethods: true}
	}

	switch i % 3 {
	case 0:
		pair := catalogOverdueLadder[(i/3)%len(catalogOverdueLadder)]
		return catalogSlotSpec{Kind: slotBackdated, Cadence: pair.cadence, CreatedAge: pair.createdAge}
	case 1:
		return catalogSlotSpec{
			Kind:       slotRecent,
			Cadence:    catalogRecentCadences[(i/3)%len(catalogRecentCadences)],
			CreatedAge: catalogRecentWindow,
		}
	default:
		return catalogSlotSpec{
			Kind:    slotFresh,
			Cadence: catalogNeverContactedCadences[(i/3)%len(catalogNeverContactedCadences)],
		}
	}
}

// --- archetype ↔ cadence compatibility ---------------------------------------
//
// Which archetype may land on which cadence is a statement about PRODUCTION
// durations (weekly 7d … annual 365d) and nothing here may be checked by reading
// the ambient environment: under CRM_ENV=test annual is two hours and every
// contact is trivially overdue.
//
// The rule is STRICT, with a margin, and both halves are load-bearing:
//
//   - An archetype whose point is to stay OVERDUE (mutual-drifting, dormant) is
//     admitted on a cadence only when its whole newest-age range clears the
//     period: newestMin >= period + calmMargin.
//   - An archetype meant to stay CALM (mutual-regular, inbound-only,
//     burst-then-quiet) is admitted only when the period clears its whole newest-
//     age range: period >= newestMax + calmMargin.
//
// A naive >= at the boundary is wrong for two compounding reasons. The newest-age
// maxima are INCLUSIVE draws, so mutual-regular can draw exactly biweekly's
// period and burst-then-quiet exactly quarterly's; and every source stamps its
// payload at anchor − defaultOffset − age with an offset of one or two hours, so
// at a boundary draw the contact is already overdue AT THE ANCHOR. Pinning the
// reference instant therefore does not fix it — it converts a rare wall-clock
// flake into an equally rare deterministic-when-drawn failure. The margin removes
// the boundary entirely, and it buys something the boundary never could: a calm
// sample stays calm for at least fourteen days after the reseed, so a tour round
// running the morning after grades the population the seed produced. (The
// tightest admitted pair is inbound-only on monthly: 30d − 15d − 2h = 14d 22h.)
// Do not relax this back to >=.
const calmMargin = 24 * time.Hour

// archetypeIntent classifies what an archetype does to a contact's overdue-ness,
// which is the only property the compatibility rule cares about.
type archetypeIntent int

const (
	// intentOverdue writes last_contacted from a deliberately OLD newest entry, so
	// the contact stays overdue and carries real history behind it.
	intentOverdue archetypeIntent = iota
	// intentCalm writes last_contacted from a RECENT newest entry, taking the
	// contact out of the overdue set.
	intentCalm
	// intentNeutral never writes last_contacted at all (an outbound touches only
	// last_outreach_at; an empty timeline touches nothing), so the slot keeps
	// whatever overdue-ness its created_at gives it — on every cadence.
	intentNeutral
)

// archetypeShape is one archetype's compatibility input: what it does to
// overdue-ness, and the inclusive bounds of the age it dates its NEWEST entry at.
//
// The bounds RESTATE the generator's per-archetype table, because that table is
// unexported and lives a package below this one. They are not trusted: a unit
// test samples TimelineFor across thousands of namespaces and requires the
// observed newest ages to sit inside these bounds AND to reach within one
// calmMargin of each end, so a generator bound that moved by more than the margin
// fails there rather than silently widening what this rule admits.
type archetypeShape struct {
	Intent     archetypeIntent
	NewestMin  time.Duration
	NewestMax  time.Duration
	HasHistory bool
}

const archetypeDay = 24 * time.Hour

// archetypeShapes is the closed catalog's compatibility table.
var archetypeShapes = map[factory.Archetype]archetypeShape{
	factory.ArchetypeMutualRegular:  {Intent: intentCalm, NewestMin: 3 * archetypeDay, NewestMax: 14 * archetypeDay, HasHistory: true},
	factory.ArchetypeMutualDrifting: {Intent: intentOverdue, NewestMin: 70 * archetypeDay, NewestMax: 112 * archetypeDay, HasHistory: true},
	factory.ArchetypeDormant:        {Intent: intentOverdue, NewestMin: 120 * archetypeDay, NewestMax: 150 * archetypeDay, HasHistory: true},
	factory.ArchetypeInboundOnly:    {Intent: intentCalm, NewestMin: 1 * archetypeDay, NewestMax: 15 * archetypeDay, HasHistory: true},
	factory.ArchetypeBurstThenQuiet: {Intent: intentCalm, NewestMin: 30 * archetypeDay, NewestMax: 90 * archetypeDay, HasHistory: true},
	factory.ArchetypeOutboundHeavy:  {Intent: intentNeutral, NewestMin: 2 * archetypeDay, NewestMax: 20 * archetypeDay, HasHistory: true},
	factory.ArchetypeNeverContacted: {Intent: intentNeutral, HasHistory: false},
}

// historyArchetypes are the six that emit a timeline, in the FIXED order every
// "least-used" tie-break resolves by. never-contacted is appended for the same
// tie-break, but is never quota-placed: it is what an unassigned slot falls back
// to, not a cohort to fill.
var historyArchetypes = []factory.Archetype{
	factory.ArchetypeMutualDrifting,
	factory.ArchetypeDormant,
	factory.ArchetypeMutualRegular,
	factory.ArchetypeInboundOnly,
	factory.ArchetypeBurstThenQuiet,
	factory.ArchetypeOutboundHeavy,
}

// archetypeTieBreak is the total order "least-used" ties resolve by, so the whole
// assignment is deterministic with no map iteration anywhere in it.
var archetypeTieBreak = append(append([]factory.Archetype{}, historyArchetypes...), factory.ArchetypeNeverContacted)

// archetypeCadences is every cadence a catalog slot can carry, in a fixed order.
// Compatibility breadth is counted over it.
var archetypeCadences = []string{"weekly", "biweekly", "monthly", "quarterly", "biannual", "annual"}

// productionCadencePeriod is a cadence name's PRODUCTION period.
func productionCadencePeriod(name string) (time.Duration, bool) {
	cadenceType, err := cadence.ParseCadence(name)
	if err != nil {
		return 0, false
	}
	cfg := cadence.ProductionCadenceConfig()
	switch cadenceType {
	case cadence.CadenceWeekly:
		return cfg.Weekly, true
	case cadence.CadenceBiweekly:
		return cfg.Biweekly, true
	case cadence.CadenceMonthly:
		return cfg.Monthly, true
	case cadence.CadenceQuarterly:
		return cfg.Quarterly, true
	case cadence.CadenceBiannual:
		return cfg.Biannual, true
	case cadence.CadenceAnnual:
		return cfg.Annual, true
	default:
		return 0, false
	}
}

// archetypeAdmissible reports whether the archetype may be placed on a slot
// carrying this cadence, under the strict production-duration rule above.
func archetypeAdmissible(a factory.Archetype, cadenceName string) bool {
	shape, ok := archetypeShapes[a]
	if !ok {
		return false
	}
	if shape.Intent == intentNeutral {
		// Touches neither last_contacted nor contact_by, so it cannot contradict any
		// cadence.
		return true
	}
	period, ok := productionCadencePeriod(cadenceName)
	if !ok {
		return false
	}
	if shape.Intent == intentOverdue {
		return shape.NewestMin >= period+calmMargin
	}
	return period >= shape.NewestMax+calmMargin
}

// archetypeCadenceBreadth counts how many cadences admit this archetype. It is
// the SCARCITY measure the quota phase places in ascending order of.
func archetypeCadenceBreadth(a factory.Archetype) int {
	n := 0
	for _, c := range archetypeCadences {
		if archetypeAdmissible(a, c) {
			n++
		}
	}
	return n
}

// archetypeScarcityOrder is historyArchetypes ordered by ascending compatibility
// breadth, ties by the fixed order. Placing the most-constrained archetype first
// is necessary but NOT sufficient — see the specificity ordering in the quota
// phase for the other half.
func archetypeScarcityOrder() []factory.Archetype {
	out := append([]factory.Archetype{}, historyArchetypes...)
	index := map[factory.Archetype]int{}
	for i, a := range historyArchetypes {
		index[a] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		bi, bj := archetypeCadenceBreadth(out[i]), archetypeCadenceBreadth(out[j])
		if bi != bj {
			return bi < bj
		}
		return index[out[i]] < index[out[j]]
	})
	return out
}

// --- the overdue budget ------------------------------------------------------

// OverdueCeiling is the absolute cap on how many contacts a seeded world may
// leave overdue. Two capture bounds sit above it: the tour normalizer's default
// array cap (50), and the overdue tours' own explicit cap, which they enforce by
// REFUSING to run above it. Exported so the coverage tests assert the shipped
// population against the same number the assignment is built to.
const OverdueCeiling = 45

// catalogNonCatalogLiveContacts models the profile's non-catalog live contacts —
// 19 at the dev knobs and 31 at prod-shaped's, so the midpoint is what one
// formula has to pick when it must set a sensible target for both. It is a
// TARGET-PICKING device inside the budget, never an assertion: the coverage test
// measures the live population from the database.
const catalogNonCatalogLiveContacts = 25

// overdueSharePercent is the percentage of the live population the overdue set
// aims at. The band the coverage gates assert is 20–27%; the budget aims at the
// middle of it.
const overdueSharePercent = 23

// pinnedOverdueFixtureCount is how many overdue contacts the pinned-fixture block
// adds. They sit OUTSIDE the catalog and are always overdue, so the catalog
// budget subtracts them rather than counting them twice.
const pinnedOverdueFixtureCount = 2

// catalogOverdueBudget is how many CATALOG slots the assignment may leave
// overdue at this catalog size.
func catalogOverdueBudget(n int) int {
	target := overdueSharePercent * (n + catalogNonCatalogLiveContacts) / 100
	if target > OverdueCeiling {
		target = OverdueCeiling
	}
	return target - pinnedOverdueFixtureCount
}

// archetypeQuota is how many samples of each history archetype the quota phase
// places. Two is the floor because the arc's commitment is multi-SAMPLE
// variation: one sample of an archetype demonstrates presence and no jitter at
// all.
func archetypeQuota(n int) int {
	if q := n / 24; q > 2 {
		return q
	}
	return 2
}

// --- the assignment ----------------------------------------------------------

// archetypeLadderFullAssignment / archetypeLadderReservedOnly are the catalog
// sizes at which the degradation ladder changes rung. Below the reserved-only
// floor the no-method bucket is structurally unreachable (it needs n > 3) and the
// coherence floors cannot be satisfied, so the whole overlay is a no-op and the
// world is byte-identical to one seeded without archetypes at all.
const (
	archetypeLadderFullAssignment = 12
	archetypeLadderReservedOnly   = 5
)

// noMethodCatalogIndex is the frozen index of the no-method slot. It is reserved
// for never-contacted mechanically, not by preference: a contact owning no
// identifier can match no source payload, so any other archetype would give it an
// empty timeline anyway while claiming a cohort membership it does not have.
const noMethodCatalogIndex = 3

// ArchetypeForIndex is the archetype catalog slot i of n carries. Exported
// because the coverage tests re-derive the assignment independently and compare
// it against what the seed actually recorded — an assertion that reads the
// recorded value back out of the harness would be circular.
func ArchetypeForIndex(i, n int) factory.Archetype {
	assignment := archetypeAssignment(n)
	if i < 0 || i >= len(assignment) {
		return factory.ArchetypeNeverContacted
	}
	return assignment[i]
}

// archetypeAssignment maps every frozen catalog index of an n-contact catalog
// onto an archetype. Pure, deterministic and PRNG-free: it reads the frozen slot
// table and nothing else, so it can be re-derived by a test without a generator,
// and reassigning archetypes cannot shift the shared PRNG stream (TimelineFor's
// draw cost is fixed).
//
// Three rungs:
//
//   - n < 5 — every slot never-contacted. The overlay drives no payload and the
//     world is byte-identical to one seeded without it.
//   - 5 <= n < 12 — reserved slots only: the no-method slot, one contacted-and-
//     overdue supplier, one outbound-heavy on a fresh slot, the rest
//     never-contacted. The percentage target does not apply on this rung.
//   - n >= 12 — the full assignment: quota, then the backdated remainder, then
//     everything else.
func archetypeAssignment(n int) []factory.Archetype {
	if n < 0 {
		n = 0
	}
	out := make([]factory.Archetype, n)
	if n < archetypeLadderReservedOnly {
		for i := range out {
			out[i] = factory.ArchetypeNeverContacted
		}
		return out
	}

	assigned := make([]bool, n)
	used := map[factory.Archetype]int{}
	overdue := 0
	budget := catalogOverdueBudget(n)

	place := func(i int, a factory.Archetype) {
		out[i] = a
		assigned[i] = true
		used[a]++
		if archetypeLeavesSlotOverdue(i, n, a) {
			overdue++
		}
	}

	// Phase 0 — the no-method slot. It is backdated, so it costs the budget one.
	if n > noMethodCatalogIndex {
		place(noMethodCatalogIndex, factory.ArchetypeNeverContacted)
	}

	if n < archetypeLadderFullAssignment {
		assignReservedSlots(out, assigned, place, n)
		for i := range out {
			if !assigned[i] {
				out[i] = factory.ArchetypeNeverContacted
			}
		}
		return out
	}

	quota := archetypeQuota(n)
	scarcity := archetypeScarcityOrder()

	// Phase Q — fill each history archetype's quota, most-constrained archetype
	// first, and within an archetype spend its LEAST-CONTESTED slot first.
	//
	// Scarcity order alone is not enough, and the failure is concrete: dormant is
	// admissible on quarterly, and filling it from the earliest compatible slot
	// spends a quarterly slot that mutual-regular and inbound-only need, leaving
	// mutual-regular at a single sample at n = 13. Specificity ordering — fewest
	// OTHER still-unfilled history archetypes admissible on that slot's cadence,
	// then index — spends a weekly slot (which only drifting and dormant can use)
	// before a quarterly one (which three archetypes want), and with it every
	// history archetype reaches its quota at every n >= 13.
	for _, a := range scarcity {
		contenders := make([]factory.Archetype, 0, len(scarcity))
		for _, other := range scarcity {
			if other != a && used[other] < quota {
				contenders = append(contenders, other)
			}
		}
		for used[a] < quota {
			best := -1
			bestContested := 0
			for i := 0; i < n; i++ {
				if assigned[i] {
					continue
				}
				slot := catalogSlot(i, n)
				if !archetypeAdmissible(a, slot.Cadence) {
					continue
				}
				if archetypeLeavesSlotOverdue(i, n, a) && overdue+1 > budget {
					continue
				}
				contested := 0
				for _, other := range contenders {
					if archetypeAdmissible(other, slot.Cadence) {
						contested++
					}
				}
				if best == -1 || contested < bestContested {
					best, bestContested = i, contested
				}
			}
			if best == -1 {
				break
			}
			place(best, a)
		}
	}

	// Phase B — the backdated remainder. These slots are overdue whatever they
	// receive unless a CALM archetype takes them, so they are the only ones that
	// interact with the budget, and the ones with NO admissible calm archetype
	// (weekly and biweekly: none exists under the strict rule) are processed FIRST
	// so they consume the budget before any discretionary spend.
	var forced, discretionary []int
	for i := 0; i < n; i++ {
		if assigned[i] || catalogSlot(i, n).Kind != slotBackdated {
			continue
		}
		if len(admissibleArchetypes(intentCalm, catalogSlot(i, n).Cadence)) == 0 {
			forced = append(forced, i)
		} else {
			discretionary = append(discretionary, i)
		}
	}
	for _, i := range append(forced, discretionary...) {
		slot := catalogSlot(i, n)
		if overdue+1 <= budget {
			// Under budget: keep the slot never-connected AND overdue, which is the
			// shape the overdue-diversity gate reads.
			place(i, leastUsed(used, []factory.Archetype{factory.ArchetypeOutboundHeavy, factory.ArchetypeNeverContacted}))
			continue
		}
		if calm := admissibleArchetypes(intentCalm, slot.Cadence); len(calm) > 0 {
			// Budget spent: a calm archetype takes the slot OUT of the overdue set.
			place(i, leastUsed(used, calm))
			continue
		}
		// Budget spent and nothing calm is admissible here. The slot takes a neutral
		// archetype and the budget is exceeded — a documented floor, not a silent
		// one: the unit test asserts the structural floor itself never exceeds the
		// ceiling.
		place(i, leastUsed(used, []factory.Archetype{factory.ArchetypeOutboundHeavy, factory.ArchetypeNeverContacted}))
	}

	// Phase C — the recent / fresh remainder. Neither is ever overdue while
	// last_contacted is NULL and neither can be made overdue by a calm archetype,
	// so there is no budget interaction here.
	for i := 0; i < n; i++ {
		if assigned[i] {
			continue
		}
		slot := catalogSlot(i, n)
		pool := admissibleArchetypes(intentCalm, slot.Cadence)
		pool = append(pool, factory.ArchetypeOutboundHeavy, factory.ArchetypeNeverContacted)
		place(i, leastUsed(used, pool))
	}

	return out
}

// assignReservedSlots is the 5 <= n < 12 rung: the smallest world that still
// carries the states the coherence gates read.
func assignReservedSlots(out []factory.Archetype, assigned []bool, place func(int, factory.Archetype), n int) {
	// The sole supplier of CONTACTED-AND-OVERDUE, a state the seed produces
	// nowhere else. mutual-drifting is preferred and dormant is the fallback; under
	// today's frozen ladder index 0 is weekly and always admits drifting, so the
	// fallback is unreachable — it is kept because the ladder is data, and a future
	// edit that made index 0 quarterly should degrade rather than silently drop the
	// state.
	for _, a := range []factory.Archetype{factory.ArchetypeMutualDrifting, factory.ArchetypeDormant} {
		placed := false
		for i := 0; i < n; i++ {
			slot := catalogSlot(i, n)
			if assigned[i] || slot.Kind != slotBackdated || !archetypeAdmissible(a, slot.Cadence) {
				continue
			}
			place(i, a)
			placed = true
			break
		}
		if placed {
			break
		}
	}

	// One outbound-heavy on a FRESH slot, so it exercises last_outreach_at without
	// last_contacted while staying out of the overdue set.
	for i := 0; i < n; i++ {
		if assigned[i] || catalogSlot(i, n).Kind != slotFresh {
			continue
		}
		place(i, factory.ArchetypeOutboundHeavy)
		break
	}
}

// admissibleArchetypes returns the history archetypes of the given intent that
// this cadence admits, in the fixed tie-break order.
func admissibleArchetypes(intent archetypeIntent, cadenceName string) []factory.Archetype {
	var out []factory.Archetype
	for _, a := range historyArchetypes {
		if archetypeShapes[a].Intent == intent && archetypeAdmissible(a, cadenceName) {
			out = append(out, a)
		}
	}
	return out
}

// leastUsed picks the pool member placed fewest times so far, ties by the fixed
// archetype order — never by map iteration, which Go randomizes.
func leastUsed(used map[factory.Archetype]int, pool []factory.Archetype) factory.Archetype {
	rank := map[factory.Archetype]int{}
	for i, a := range archetypeTieBreak {
		rank[a] = i
	}
	best := pool[0]
	for _, a := range pool[1:] {
		if used[a] < used[best] || (used[a] == used[best] && rank[a] < rank[best]) {
			best = a
		}
	}
	return best
}

// --- overdue prediction ------------------------------------------------------

// catalogSlotNativelyOverdue reports whether the slot is overdue on its own —
// backdated past its cadence period with an empty timeline. Only a backdated slot
// can be, and every entry in the frozen ladder is (that is what the ladder is
// for), so this is a derivation rather than an assumption.
func catalogSlotNativelyOverdue(i, n int) bool {
	slot := catalogSlot(i, n)
	if slot.Kind != slotBackdated {
		return false
	}
	period, ok := productionCadencePeriod(slot.Cadence)
	if !ok {
		return false
	}
	return slot.CreatedAge >= period+calmMargin
}

// archetypeLeavesSlotOverdue predicts whether a slot ends up overdue once its
// archetype's history has landed:
//
//   - an overdue-intent archetype is overdue by construction (its newest two-way
//     entry is older than the period, which the compatibility rule guarantees);
//   - a calm one writes a recent last_contacted and is not;
//   - a neutral one never writes last_contacted, so the slot keeps whatever its
//     created_at gives it.
func archetypeLeavesSlotOverdue(i, n int, a factory.Archetype) bool {
	switch archetypeShapes[a].Intent {
	case intentOverdue:
		return true
	case intentCalm:
		return false
	default:
		return catalogSlotNativelyOverdue(i, n)
	}
}

// PredictedCatalogOverdue is how many CATALOG slots the assignment leaves
// overdue at this catalog size. Exported so the coverage test can compare a
// DB-measured count against the prediction rather than trusting either alone.
func PredictedCatalogOverdue(n int) int {
	count := 0
	for i, a := range archetypeAssignment(n) {
		if archetypeLeavesSlotOverdue(i, n, a) {
			count++
		}
	}
	return count
}

// OverdueAtProduction reports whether a contact is overdue under PRODUCTION
// cadence durations, evaluated at ref — normally the seeded world's own generator
// anchor, so a prediction and a measurement answer the same question about the
// same instant instead of racing the wall clock between them.
//
// Same formula as cadence.IsOverdueWithConfig, with the env-derived duration
// replaced by the production one. Exported for the coverage tests: the suite runs
// under CRM_ENV=test, where annual is two hours and every contact is overdue, so
// a measurement taken through the ambient config would prove nothing.
func OverdueAtProduction(cadenceName string, lastContacted *time.Time, createdAt, ref time.Time) bool {
	period, ok := productionCadencePeriod(cadenceName)
	if !ok {
		return false
	}
	base := createdAt
	if lastContacted != nil {
		base = *lastContacted
	}
	return ref.After(base.Add(period))
}
