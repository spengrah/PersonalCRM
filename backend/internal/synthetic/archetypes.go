package synthetic

import (
	"time"
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
