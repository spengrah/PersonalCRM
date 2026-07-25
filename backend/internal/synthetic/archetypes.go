package synthetic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
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
	// CreatedAgeBound is an UPPER BOUND on how far before the anchor the contact is
	// created: exact for slotBackdated (the frozen ladder pins it), the WINDOW for
	// slotRecent (the true age is a PRNG draw inside it), zero for slotFresh.
	//
	// A bound rather than a value is enough because the only consumer is the
	// overdue prediction, and a larger created age can only make a contact MORE
	// overdue — so a slot whose bound is not overdue cannot be overdue. The one
	// place the prediction concludes overdue (slotBackdated) is the one place the
	// bound is exact.
	CreatedAgeBound time.Duration
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
		return catalogSlotSpec{Kind: slotBackdated, Cadence: pair.cadence, CreatedAgeBound: pair.createdAge, NoMethods: true}
	}

	switch i % 3 {
	case 0:
		pair := catalogOverdueLadder[(i/3)%len(catalogOverdueLadder)]
		return catalogSlotSpec{Kind: slotBackdated, Cadence: pair.cadence, CreatedAgeBound: pair.createdAge}
	case 1:
		return catalogSlotSpec{
			Kind:            slotRecent,
			Cadence:         catalogRecentCadences[(i/3)%len(catalogRecentCadences)],
			CreatedAgeBound: catalogRecentWindow,
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
	// intentNeutral never writes last_contacted at all (an outbound touches only
	// last_outreach_at; an empty timeline touches nothing), so the slot keeps
	// whatever overdue-ness its created_at gives it — on every cadence. It is
	// FIRST so that it is the zero value: an archetype missing from the table
	// then degrades to "changes nothing", which is the only safe default for a
	// predicate that decides overdue-ness.
	intentNeutral archetypeIntent = iota
	// intentOverdue writes last_contacted from a deliberately OLD newest entry, so
	// the contact stays overdue and carries real history behind it.
	intentOverdue
	// intentCalm writes last_contacted from a RECENT newest entry, taking the
	// contact out of the overdue set.
	intentCalm
)

// archetypeShape is one archetype's compatibility input: what it does to
// overdue-ness, and — for the archetypes whose intent makes that question
// answerable — the inclusive bounds of the age it dates its newest
// LAST-CONTACTED-WRITING entry at.
//
// Both fields RESTATE facts that live a package below, in an unexported
// generator table, and neither is trusted. A unit test samples TimelineFor
// across thousands of namespaces and requires (a) that an archetype is neutral
// exactly when its timelines never contain an entry that writes last_contacted,
// and (b) that the observed newest such age sits inside these bounds AND reaches
// within one calmMargin of each end. So a generator change that moved a bound by
// more than the margin, or that gave a neutral archetype a two-way entry, fails
// there rather than silently widening what this rule admits.
//
// A NEUTRAL archetype declares no bounds. It cannot move last_contacted at all,
// so archetypeAdmissible never reads them, and carrying decorative values would
// put an unguarded restatement back into the table.
type archetypeShape struct {
	Intent    archetypeIntent
	NewestMin time.Duration
	NewestMax time.Duration
}

const archetypeDay = 24 * time.Hour

// archetypeShapes is the closed catalog's compatibility table.
var archetypeShapes = map[factory.Archetype]archetypeShape{
	factory.ArchetypeMutualRegular:  {Intent: intentCalm, NewestMin: 3 * archetypeDay, NewestMax: 14 * archetypeDay},
	factory.ArchetypeMutualDrifting: {Intent: intentOverdue, NewestMin: 70 * archetypeDay, NewestMax: 112 * archetypeDay},
	factory.ArchetypeDormant:        {Intent: intentOverdue, NewestMin: 120 * archetypeDay, NewestMax: 150 * archetypeDay},
	factory.ArchetypeInboundOnly:    {Intent: intentCalm, NewestMin: 1 * archetypeDay, NewestMax: 15 * archetypeDay},
	factory.ArchetypeBurstThenQuiet: {Intent: intentCalm, NewestMin: 30 * archetypeDay, NewestMax: 90 * archetypeDay},
	factory.ArchetypeOutboundHeavy:  {Intent: intentNeutral},
	factory.ArchetypeNeverContacted: {Intent: intentNeutral},
}

// archetypeWritesLastContacted reports whether a timeline entry is one the
// pipeline classifies as inbound or mutual — the only kind that moves
// last_contacted, and therefore the only kind the compatibility rule reasons
// about. A matched calendar event is always mutual; a message writes it when it
// arrives rather than when it is sent.
func archetypeWritesLastContacted(e factory.TimelineEntry) bool {
	return e.Source == factory.SourceGCal || !e.Outbound
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

// productionCadencePeriod is a cadence name's PRODUCTION period, and whether the
// name is one this table recognizes at all.
//
// The duration comes from the cadence package's own lookup, so the two cannot
// disagree. What is deliberately NOT shared is the fallback: that lookup resolves
// an unrecognized cadence to Monthly, which is the right answer when computing a
// due date for a row that already exists, and the wrong one here — the
// compatibility rule decides what MAY be placed, and defaulting would silently
// admit archetypes onto a cadence nobody reasoned about.
func productionCadencePeriod(name string) (time.Duration, bool) {
	cadenceType, err := cadence.ParseCadence(name)
	if err != nil {
		return 0, false
	}
	return cadence.GetProductionCadenceDuration(cadenceType), true
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

// PinnedOverdueFixtureCount is how many overdue contacts the pinned-fixture block
// adds. They sit OUTSIDE the catalog and are always overdue, so the catalog
// budget subtracts them rather than counting them twice. Exported so the coverage
// tests add the same number back rather than restating it, and pinned against
// pinnedOverdueFixtures by a unit test (a slice literal cannot be a const).
const PinnedOverdueFixtureCount = 2

// catalogOverdueBudget is how many CATALOG slots the assignment may leave
// overdue at this catalog size.
func catalogOverdueBudget(n int) int {
	target := overdueSharePercent * (n + catalogNonCatalogLiveContacts) / 100
	if target > OverdueCeiling {
		target = OverdueCeiling
	}
	return target - PinnedOverdueFixtureCount
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
		// The EMPTY archetype, deliberately not a real one. This function's whole
		// justification is that it is an INDEPENDENT oracle: an out-of-range answer
		// that happened to be a valid archetype would let a comparison against a
		// mis-derived index agree by coincidence instead of failing loudly.
		return factory.Archetype("")
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
	// Copied rather than appended in place: `forced` was grown by append above and
	// can carry spare capacity, so appending onto it would write into its own
	// backing array.
	order := append(append([]int{}, forced...), discretionary...)
	for _, i := range order {
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
	return slot.CreatedAgeBound >= period+calmMargin
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

// OverdueAtProduction reports whether a contact carrying this cadence NAME is
// overdue under production durations, evaluated at ref — normally the seeded
// world's own generator anchor, so a prediction and a measurement answer the same
// question about the same instant instead of racing the wall clock between them.
//
// It DELEGATES the formula rather than restating it: the cadence package owns
// both the table and the overdue arithmetic, and a second copy here would be free
// to drift on exactly the edge a copy always drifts on (this one previously did —
// an unrecognized cadence resolved to Monthly there and to "never overdue"
// here). What is local is only the two things the cadence package cannot know:
// that the caller holds a nullable cadence STRING, and that an absent cadence
// means no due date at all rather than a defaulted one.
//
// Exported for the coverage tests, which run under CRM_ENV=test where annual is
// two hours and every contact is overdue — so a measurement taken through the
// ambient config would prove nothing, and mutating the variable would change
// cadence semantics for every concurrently-running test.
func OverdueAtProduction(cadenceName string, lastContacted *time.Time, createdAt, ref time.Time) bool {
	if cadenceName == "" {
		// No cadence means the app computes no due date, so nothing can be overdue.
		return false
	}
	return cadence.IsOverdueWithProductionConfig(cadence.CadenceType(cadenceName), lastContacted, createdAt, ref)
}

// --- timeline → batch payloads ----------------------------------------------

// Batch-composition failures. Each is named so a mapper bug is an immediate,
// self-describing error instead of a stranded row and a settle timeout blaming
// the wrong thing.
var (
	// errArchetypeUnsupportedSource — a timeline emitted a source the catalog
	// cannot carry. Catalog contacts are email-only, so the generator resolves them
	// to exactly {gcal, email, gchat}; telegram and iMessage would need dedicated
	// contacts, and archetypes add history rather than contacts. Failing loudly is
	// the point: a future method-set change must be a deliberate edit here, not a
	// silent strand.
	errArchetypeUnsupportedSource = errors.New("synthetic archetypes: timeline emitted a source the catalog cannot match")
	// errArchetypeCalendarPairKey — a calendar entry carried a pair key. A matched
	// calendar event is always mutual, so the batch item type has no field to carry
	// one; dropping it silently would lose a stated intent.
	errArchetypeCalendarPairKey = errors.New("synthetic archetypes: calendar entry carries a promotion pair key")
	// errArchetypePairMalformed — a promotion pair that is not exactly two entries
	// with differing directions, or whose timing breaks the source-specific rule.
	errArchetypePairMalformed = errors.New("synthetic archetypes: promotion pair is malformed")
	// errArchetypeChatCloneUnresolved — a chat conversation whose membership does
	// not yield BOTH the connected account and a peer, so a clone cannot be given a
	// sender. Left unchecked this is the mapper's one silent failure: the clone
	// would carry an empty sender, the provider would read it as INBOUND with an
	// empty peer address, and the batch ownership preflight skips empty-addressed
	// items by design — so an intended outbound would strand and time out a gate
	// blaming the wrong thing. Every other failure here is named and immediate.
	errArchetypeChatCloneUnresolved = errors.New("synthetic archetypes: chat conversation membership does not resolve both sender roles")
	// errArchetypeSlotOutOfRange — a slot whose frozen index is not in the catalog
	// the assignment was derived for. The two must be the same catalog or the
	// overlay is being applied to a world it was not computed against.
	errArchetypeSlotOutOfRange = errors.New("synthetic archetypes: slot index is outside the assigned catalog")
	// errArchetypeSettleBudget — the block settled more times than one Settle per
	// dependency generation allows, which is what a regression from batch replay
	// back to per-payload replay looks like from the outside.
	errArchetypeSettleBudget = errors.New("synthetic archetypes: replay exceeded its settle budget")
)

const (
	// gmailAgeBucketWidth buckets the block's mail by age before it is replayed.
	// Mail pooled across archetypes spans up to 179 days — mutual-drifting's second
	// correspondence pair at the far end of the second meeting gap against
	// inbound-only at its newest bound — while one Gmail batch may span at most 150
	// days, and the adapter REJECTS a wider batch rather than auto-splitting it
	// (only the caller knows whether splitting a contact's history across two syncs
	// is semantically fine). A fixed 120-day window makes the bound hold by
	// construction whatever the draws are: every bucket spans strictly less than
	// 120 days. Both halves of a Gmail promotion pair carry the SAME age, so a pair
	// can never straddle two buckets.
	gmailAgeBucketWidth = 120 * 24 * time.Hour

	// chatBurstWindow / chatReplyBridgeWindow are the GChat aggregation engine's
	// OWN windows, read from the package that declares them rather than mirrored as
	// local literals — a mirrored literal cannot fail when the real window moves,
	// which is precisely the drift this arc's other budget accessors exist to stop.
	//
	// They are load-bearing twice over: the collapse prediction below counts
	// sessions by the burst window, and the opening burst's forty-minute stride is
	// only INSIDE one session while that window stays above it.
	chatBurstWindow       = time.Duration(google.GChatBurstWindowHours) * time.Hour
	chatReplyBridgeWindow = time.Duration(google.GChatReplyBridgeHours) * time.Hour
)

// archetypeSlot is one catalog slot the archetype block gives history to: which
// frozen index it is, which contact it became, and the spec the source payloads
// must address so the replay actually matches it.
type archetypeSlot struct {
	Index     int
	ContactID uuid.UUID
	Spec      factory.ContactSpec
}

// archetypeBatches is the block's whole payload plan, ready to drive. Gmail is
// pre-bucketed (oldest bucket first, each bucket oldest-entry first); the other
// two are single batches in oldest-first order.
//
// It carries no payload total on purpose: the seeding block counts what the
// adapters actually DROVE, and a second counter for the same quantity would be
// two numbers kept in step by nothing but a cross-check.
type archetypeBatches struct {
	GCal         []replay.GCalBatchItem
	GmailBuckets [][]replay.GmailBatchItem
	GChat        []replay.GChatBatchItem
	Samples      []replay.ArchetypeSample
}

// empty reports whether the plan would drive nothing at all — the case the
// adapters reject by preflight and the caller must therefore skip.
func (b archetypeBatches) empty() bool {
	return len(b.GCal) == 0 && len(b.GmailBuckets) == 0 && len(b.GChat) == 0
}

// archetypeTimelineSource resolves one slot's archetype and the timeline that
// archetype produces for it. It is a seam, not indirection for its own sake: the
// mapper's guard branches are reachable only from timeline SHAPES the generator
// does not currently emit (a calendar entry carrying a pair key, a pair spanning
// two sources), and a guard nothing can exercise is indistinguishable from one
// that always passes.
type archetypeTimelineSource func(slot archetypeSlot) (factory.Archetype, factory.Timeline, error)

// buildArchetypeBatches turns each slot's assigned archetype into source
// payloads.
//
// TimelineFor is called for EVERY slot, including the ones that end up with no
// history at all: its draw cost is fixed, so calling it unconditionally makes the
// block's PRNG consumption a function of the catalog size alone and never of
// which archetype landed where.
func buildArchetypeBatches(gen *factory.Generator, slots []archetypeSlot, n int) (archetypeBatches, error) {
	// Derived ONCE for the whole catalog: the assignment is a whole-catalog
	// computation, so resolving it per slot would run it n times per seed.
	assignment := archetypeAssignment(n)
	return buildArchetypeBatchesFrom(gen, slots, func(slot archetypeSlot) (factory.Archetype, factory.Timeline, error) {
		if slot.Index < 0 || slot.Index >= len(assignment) {
			return "", factory.Timeline{}, fmt.Errorf("slot %d against a %d-slot catalog: %w",
				slot.Index, len(assignment), errArchetypeSlotOutOfRange)
		}
		archetype := assignment[slot.Index]
		return archetype, gen.TimelineFor(archetype, archetypeMethodSet(slot.Spec)), nil
	})
}

// archetypeMethodSet reports what identifiers a seeded catalog contact actually
// owns, so the generator can only ever emit sources it can match. Read off the
// spec rather than assumed, so a catalog slot that gained a method would produce
// an honest timeline — and one that produced an unmatchable source would fail at
// the mapper rather than strand a row.
func archetypeMethodSet(spec factory.ContactSpec) factory.MethodSet {
	return factory.MethodSet{
		Email:    spec.Email != "",
		Telegram: spec.TelegramHandle != "",
		Phone:    spec.Phone != "",
	}
}

// buildArchetypeBatchesFrom is buildArchetypeBatches over a caller-supplied
// timeline source.
func buildArchetypeBatchesFrom(gen *factory.Generator, slots []archetypeSlot, timelineFor archetypeTimelineSource) (archetypeBatches, error) {
	var out archetypeBatches

	// Every batch is collected with its entries' ages, so the whole block can be
	// put into one chronological order at the end rather than per contact — and so
	// the mail can be bucketed by age band across every contact at once.
	type agedGCalItem struct {
		item replay.GCalBatchItem
		age  time.Duration
	}
	type agedGmailItem struct {
		item replay.GmailBatchItem
		age  time.Duration
	}
	type agedGChatItem struct {
		item replay.GChatBatchItem
		age  time.Duration
	}
	var calendar []agedGCalItem
	var mail []agedGmailItem
	var messages []agedGChatItem

	// PairKey groups are validated across the WHOLE batch by the adapter, so two
	// contacts each carrying the timeline-local key 1 would form a four-member
	// group and be rejected. Re-key every pair against one block-global counter.
	nextPairKey := 0

	for _, slot := range slots {
		archetype, timeline, err := timelineFor(slot)
		if err != nil {
			return out, err
		}

		// A conversation is a PLAN-level statement; at replay time the payloads must
		// also share the source's conversation identifier, and the factories mint a
		// fresh one per call. So the first entry of a conversation mints it and every
		// later entry is cloned onto it.
		threads := map[int]string{}
		spaces := map[int]factory.GChatMessageSpec{}
		spaceClones := map[int]int{}
		batchPairKeys := map[int]int{}
		pairMembers := map[int][]factory.TimelineEntry{}

		for _, entry := range timeline.Entries {
			pairKey := 0
			if entry.PairKey != 0 {
				key, ok := batchPairKeys[entry.PairKey]
				if !ok {
					nextPairKey++
					key = nextPairKey
					batchPairKeys[entry.PairKey] = key
				}
				pairKey = key
				pairMembers[entry.PairKey] = append(pairMembers[entry.PairKey], entry)
			}

			switch entry.Source {
			case factory.SourceGCal:
				if entry.PairKey != 0 {
					return out, fmt.Errorf("slot %d (%s): %w", slot.Index, archetype, errArchetypeCalendarPairKey)
				}
				calendar = append(calendar, agedGCalItem{
					item: replay.GCalBatchItem{
						ContactID: slot.ContactID,
						Spec:      gen.GCalEvent(slot.Spec, factory.MatchSeeded, factory.WithMessageAge(entry.Age)),
					},
					age: entry.Age,
				})

			case factory.SourceEmail:
				spec := gen.GmailMessage(slot.Spec, factory.MatchSeeded, archetypeMessageOptions(entry)...)
				if entry.ConversationKey != 0 {
					if threadID, ok := threads[entry.ConversationKey]; ok {
						spec.Message.ThreadId = threadID
					} else {
						threads[entry.ConversationKey] = spec.Message.ThreadId
					}
				}
				mail = append(mail, agedGmailItem{
					item: replay.GmailBatchItem{ContactID: slot.ContactID, Spec: spec, PairKey: pairKey},
					age:  entry.Age,
				})

			case factory.SourceGChat:
				var spec factory.GChatMessageSpec
				if base, ok := spaces[entry.ConversationKey]; entry.ConversationKey != 0 && ok {
					spaceClones[entry.ConversationKey]++
					clone, cloneErr := cloneArchetypeGChatMessage(
						base,
						fmt.Sprintf("arch-%d", spaceClones[entry.ConversationKey]),
						entry.Outbound,
						gen.Anchor().Add(-gchatMessageDefaultOffset-entry.Age),
					)
					if cloneErr != nil {
						return out, fmt.Errorf("slot %d (%s): %w", slot.Index, archetype, cloneErr)
					}
					spec = clone
				} else {
					spec = gen.GChatMessage(slot.Spec, factory.MatchSeeded, archetypeMessageOptions(entry)...)
					if entry.ConversationKey != 0 {
						spaces[entry.ConversationKey] = spec
					}
				}
				messages = append(messages, agedGChatItem{
					item: replay.GChatBatchItem{ContactID: slot.ContactID, Spec: spec, PairKey: pairKey},
					age:  entry.Age,
				})

			default:
				return out, fmt.Errorf("slot %d (%s): source %q: %w", slot.Index, archetype, entry.Source, errArchetypeUnsupportedSource)
			}
		}

		if err := validateArchetypePairs(slot.Index, archetype, pairMembers); err != nil {
			return out, err
		}

		out.Samples = append(out.Samples, replay.ArchetypeSample{
			ContactID:            slot.ContactID,
			SlotIndex:            slot.Index,
			Archetype:            archetype,
			Payloads:             len(timeline.Entries),
			ExpectedInteractions: expectedArchetypeInteractions(timeline),
		})
	}

	sort.SliceStable(calendar, func(i, j int) bool { return calendar[i].age > calendar[j].age })
	for _, c := range calendar {
		out.GCal = append(out.GCal, c.item)
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].age > messages[j].age })
	for _, m := range messages {
		out.GChat = append(out.GChat, m.item)
	}

	// Bucket the mail by age band and emit the OLDEST bucket first, each bucket
	// oldest-entry first — the chronological replay order the adapters require.
	byBucket := map[int][]replay.GmailBatchItem{}
	buckets := []int{}
	sort.SliceStable(mail, func(i, j int) bool { return mail[i].age > mail[j].age })
	for _, m := range mail {
		bucket := int(m.age / gmailAgeBucketWidth)
		if _, ok := byBucket[bucket]; !ok {
			buckets = append(buckets, bucket)
		}
		byBucket[bucket] = append(byBucket[bucket], m.item)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(buckets)))
	for _, bucket := range buckets {
		out.GmailBuckets = append(out.GmailBuckets, byBucket[bucket])
	}

	return out, nil
}

// gchatMessageDefaultOffset is the small backward offset the GChat factory adds
// on top of a message's age. A cloned message has to reproduce it exactly, or the
// clone's instant would not be the instant its timeline entry asked for.
const gchatMessageDefaultOffset = time.Hour

// archetypeMessageOptions maps a timeline entry onto the factory's message
// options. Age is passed THROUGH: promotion-pair timing is decided by the
// generator (a mail pair at the same instant, a chat pair six hours apart with
// the outbound older), and a caller that recomputed or "improved" a gap would
// reintroduce exactly the tie-break the construction exists to remove.
func archetypeMessageOptions(entry factory.TimelineEntry) []factory.MessageOption {
	opts := []factory.MessageOption{factory.WithMessageAge(entry.Age)}
	if entry.Outbound {
		opts = append(opts, factory.WithOutbound())
	}
	return opts
}

// validateArchetypePairs checks the promotion-pair contract structurally, per
// source, so a future generator change trips a named error here rather than a
// local-midnight flake or a nondeterministic session split in the pipeline.
func validateArchetypePairs(slotIndex int, archetype factory.Archetype, pairs map[int][]factory.TimelineEntry) error {
	keys := make([]int, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		members := pairs[k]
		if len(members) != 2 {
			return fmt.Errorf("slot %d (%s): pair %d has %d members (want 2): %w",
				slotIndex, archetype, k, len(members), errArchetypePairMalformed)
		}
		older, newer := members[0], members[1]
		if older.Outbound == newer.Outbound {
			return fmt.Errorf("slot %d (%s): pair %d members share a direction: %w",
				slotIndex, archetype, k, errArchetypePairMalformed)
		}
		if older.Source != newer.Source {
			return fmt.Errorf("slot %d (%s): pair %d spans two sources (%q and %q): %w",
				slotIndex, archetype, k, older.Source, newer.Source, errArchetypePairMalformed)
		}
		if older.Source == factory.SourceEmail {
			// The mail aggregation key includes a LOCAL day, so any nonzero gap can
			// straddle local midnight depending on where the moving anchor lands. The
			// same instant is the same local day, unconditionally.
			if older.Age != newer.Age {
				return fmt.Errorf("slot %d (%s): mail pair %d halves are %s apart but must share an instant: %w",
					slotIndex, archetype, k, older.Age-newer.Age, errArchetypePairMalformed)
			}
			continue
		}
		// The chat path orders eligible rows only by sent time and promotion requires
		// the outbound to precede the inbound, so equal timestamps could
		// nondeterministically become one mutual or two one-sided sessions.
		if !older.Outbound || older.Age <= newer.Age {
			return fmt.Errorf("slot %d (%s): chat pair %d must place its OUTBOUND half strictly older: %w",
				slotIndex, archetype, k, errArchetypePairMalformed)
		}
	}
	return nil
}

// cloneArchetypeGChatMessage builds another message in the SAME space as base —
// the clone discipline that makes a group of items one conversation. Independent
// factory calls mint a fresh space each, which can never bridge and costs the
// provider's shared page budget three fresh pages.
//
// Deliberately not shared with the batch tests' equivalent: that one is a fixture
// for hand-authored batches, and coupling seed orchestration to a test helper
// would make either one hard to change. Both are covered by their own assertions.
func cloneArchetypeGChatMessage(base factory.GChatMessageSpec, suffix string, outbound bool, createTime time.Time) (factory.GChatMessageSpec, error) {
	// Pick the sender by MEMBERSHIP order rather than by ranging EmailByUser: Go
	// randomizes map iteration, so a space with more than two members would
	// otherwise clone a nondeterministic sender.
	sender, me := "", ""
	for _, member := range base.Members {
		if member == nil || member.Member == nil {
			continue
		}
		user := member.Member.Name
		if base.EmailByUser[user] == base.AccountID {
			if me == "" {
				me = user
			}
			continue
		}
		if sender == "" {
			sender = user
		}
	}
	// Both roles must resolve. A missing one would produce an empty sender, which
	// the provider reads as an inbound from an empty address — a direction flip and
	// an unmatchable payload, neither of which any downstream check would name.
	if me == "" || sender == "" {
		return factory.GChatMessageSpec{}, fmt.Errorf("space %q resolves me=%q peer=%q: %w",
			base.SpaceName, me, sender, errArchetypeChatCloneUnresolved)
	}
	from := sender
	if outbound {
		from = me
	}
	name := base.SpaceName + "/messages/" + suffix
	clone := base
	clone.Message = &chat.Message{
		Name:       name,
		Sender:     &chat.User{Name: from, Type: "HUMAN"},
		Text:       "synthetic chat message",
		CreateTime: createTime.UTC().Format(time.RFC3339Nano),
	}
	clone.ExternalID = name
	return clone, nil
}

// --- the expected payload → interaction collapse -----------------------------

// expectedArchetypeInteractions is how many interaction ROWS a timeline's
// payloads are expected to land once the pipeline has aggregated them. It is
// deliberately smaller than the payload count: a mail promotion pair collapses
// into one mutual row, and a chat burst collapses into one session per idle gap.
//
// It is derived from the timeline's STRUCTURE and from the pipeline's stated
// aggregation semantics — not measured — so comparing it against the database is
// a real test rather than a tautology. When the two disagree the derivation is
// wrong and must be re-derived; it is never relaxed to an inequality.
func expectedArchetypeInteractions(tl factory.Timeline) int {
	rows := 0
	mailThreads := map[int]bool{}
	chatConversations := map[int][]factory.TimelineEntry{}
	soloChat := 0

	for _, entry := range tl.Entries {
		switch entry.Source {
		case factory.SourceGCal:
			// A matched calendar event is one mutual interaction, and the archetypes
			// space meetings weeks apart, so no two collapse.
			rows++
		case factory.SourceEmail:
			// Mail promotion keys on (contact, thread, local day). Every conversation
			// this catalog emits is one thread at one instant, so a thread is one row;
			// an unthreaded message is its own.
			if entry.ConversationKey == 0 {
				rows++
				continue
			}
			mailThreads[entry.ConversationKey] = true
		default:
			if entry.ConversationKey == 0 {
				soloChat++
				continue
			}
			chatConversations[entry.ConversationKey] = append(chatConversations[entry.ConversationKey], entry)
		}
	}
	rows += len(mailThreads) + soloChat

	keys := make([]int, 0, len(chatConversations))
	for k := range chatConversations {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		rows += chatSessionCount(chatConversations[k])
	}
	return rows
}

// chatSessionCount is how many interaction rows one chat conversation's messages
// aggregate into, following the engine's own two steps: consecutive same-
// direction messages inside the burst window form a burst, and an inbound burst
// that follows an outbound SESSION within the reply-bridge window merges into it
// and turns it mutual (so a second inbound cannot bridge into the same session).
//
// entries arrive oldest-first, which is the order the engine reads them in.
func chatSessionCount(entries []factory.TimelineEntry) int {
	if len(entries) == 0 {
		return 0
	}
	type session struct {
		outbound bool
		mutual   bool
		lastAge  time.Duration
	}
	var sessions []session
	burstOutbound := entries[0].Outbound
	burstFirstAge, burstLastAge := entries[0].Age, entries[0].Age

	flush := func() {
		if len(sessions) > 0 && !burstOutbound {
			prev := &sessions[len(sessions)-1]
			// The engine bridges by INSTANT gap, and ages run backwards, so the gap
			// from the previous session's newest message to this burst's oldest is
			// prev.lastAge − burstFirstAge.
			if prev.outbound && !prev.mutual && prev.lastAge-burstFirstAge <= chatReplyBridgeWindow {
				prev.mutual = true
				prev.lastAge = burstLastAge
				return
			}
		}
		sessions = append(sessions, session{outbound: burstOutbound, lastAge: burstLastAge})
	}

	for _, entry := range entries[1:] {
		gap := burstLastAge - entry.Age
		if entry.Outbound != burstOutbound || gap > chatBurstWindow {
			flush()
			burstOutbound, burstFirstAge, burstLastAge = entry.Outbound, entry.Age, entry.Age
			continue
		}
		burstLastAge = entry.Age
	}
	flush()
	return len(sessions)
}

// --- the seeding block -------------------------------------------------------

// seedArchetypeHistories gives every catalog slot the interaction history its
// archetype describes, by replaying the timeline's payloads through the batch
// adapters.
//
// It runs at the VERY END of the catalog profile, after every other
// generator-drawing block: TimelineFor draws the shared PRNG, and inserting it
// anywhere earlier would shift every later allocation — a shifted numeric
// identifier can land on an id another contact already owns, which surfaces as a
// cross-match and a settle timeout far from the edit that caused it.
//
// The three sources are driven in a fixed order — calendar, then mail bucket by
// bucket, then chat — so the phase's settle count is reproducible. Cross-source
// ordering is otherwise free: the consumers' occurred_at guard is forward-only,
// so an older calendar batch replayed after a newer mail batch cannot move
// last_contacted backwards, and promotion is per-thread or per-space, never
// across sources.
//
// A source with no payloads is SKIPPED rather than driven: the adapters reject a
// zero-item batch by preflight, and on the reserved rung the chat batch is
// guaranteed empty.
func seedArchetypeHistories(
	ctx context.Context,
	h *Harness,
	gen *factory.Generator,
	profile Profile,
	slots []archetypeSlot,
	n int,
	res *ProfileResult,
) error {
	batches, err := buildArchetypeBatches(gen, slots, n)
	if err != nil {
		return fmt.Errorf("profile %s: build archetype payloads: %w", profile, err)
	}
	// Recorded for EVERY slot, including the history-free ones, so a coverage
	// assertion can prove absence as well as presence.
	h.SetArchetypeSamples(batches.Samples)
	if batches.empty() {
		return nil
	}

	batchCalls := 0
	record := func(result replay.BatchResult) {
		batchCalls++
		res.ArchetypePayloads += result.Payloads
		// Each batch snapshots the touched contacts' interaction ids before it
		// drives, so summing across batches cannot double-count.
		res.ArchetypeInteractions += result.Interactions
		// Settle is O(all harness contacts) and rebuilds the whole event-id union
		// on every call, so the phase's cost is its SETTLE count, not its payload
		// count. One per dependency generation, at most two per batch.
		res.ArchetypeSettleCalls += result.SettleCalls
	}

	if len(batches.GCal) > 0 {
		result, err := h.ReplayGCalBatch(ctx, batches.GCal)
		if err != nil {
			return fmt.Errorf("profile %s: replay archetype calendar batch: %w", profile, err)
		}
		record(result)
	}
	for i, bucket := range batches.GmailBuckets {
		if len(bucket) == 0 {
			continue
		}
		result, err := h.ReplayGmailBatch(ctx, bucket)
		if err != nil {
			return fmt.Errorf("profile %s: replay archetype mail bucket %d: %w", profile, i, err)
		}
		record(result)
	}
	if len(batches.GChat) > 0 {
		result, err := h.ReplayGChatBatch(ctx, batches.GChat)
		if err != nil {
			return fmt.Errorf("profile %s: replay archetype chat batch: %w", profile, err)
		}
		record(result)
	}

	// The settle budget is the whole reason this block batches at all: the same
	// history through per-payload single replays would cost one Settle per payload
	// — roughly a hundred at dev and a thousand at prod-shaped — each of them
	// O(all harness contacts). A batch settles once per dependency GENERATION, and
	// a generation split exists only where a promotion pair does, so two per call
	// is the ceiling. Exceeding it means the batching has been undone somewhere,
	// which every other assertion in this arc would happily pass.
	if max := batchCalls * archetypeMaxSettlesPerBatch; res.ArchetypeSettleCalls > max {
		return fmt.Errorf("profile %s: archetype replay settled %d times across %d batches (max %d): %w",
			profile, res.ArchetypeSettleCalls, batchCalls, max, errArchetypeSettleBudget)
	}
	return nil
}

// archetypeMaxSettlesPerBatch is how many Settles one batch call may cost: one
// per dependency generation, and a batch has at most two (the inbound half of
// each promotion pair is deferred to the second).
const archetypeMaxSettlesPerBatch = 2
