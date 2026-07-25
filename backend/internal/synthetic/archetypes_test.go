package synthetic

import (
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
