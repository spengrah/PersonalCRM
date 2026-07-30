package declare

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContactsDeclarations_RegisterTheExpectedHandleCounts(t *testing.T) {
	want := map[string]int{
		"CON-038": 3,
		"CON-040": 3,
		"CON-041": 1,
		"CON-042": 1,
		"CON-043": 2,
		"CON-044": 1,
		"CON-045": 8,
		"CON-053": 1,
		"CON-054": 4,
		"CON-057": 1,
		"CON-058": 22,
		"CON-059": 3,
		"CON-060": 2,
		"CON-061": 2,
		"CON-063": 2,
		"CON-065": 21,
		"CON-066": 21,
	}
	for id, count := range want {
		d, ok := Lookup(id)
		require.True(t, ok, "%s must be registered as a declaration", id)
		assert.Len(t, d.Entities, count, "%s entity count", id)
		handles := map[string]bool{}
		for _, e := range d.Entities {
			assert.False(t, handles[e.handle()], "%s repeats handle %q", id, e.handle())
			handles[e.handle()] = true
		}
	}
}

// The back-nav fixtures' whole purpose is that name-ASCENDING order equals
// insertion order, so p21 is the sole row of page 2 at a twenty-row page size.
// That only holds while the pinned literals stay zero-padded. Both cohorts are
// read out of the REGISTRY rather than from the builder, so what the declarations
// actually pass is what gets checked.
func TestBackNavCohorts_AreNameOrderedByConstruction(t *testing.T) {
	for _, id := range []string{"CON-065", "CON-066"} {
		d, ok := Lookup(id)
		require.True(t, ok, "%s must be registered", id)
		require.Len(t, d.Entities, backNavFixtureSize)

		names := make([]string, 0, len(d.Entities))
		for i, e := range d.Entities {
			p, isContact := e.(*contactPlan)
			require.True(t, isContact)
			assert.Equal(t, fmt.Sprintf("p%02d", i+1), p.name, "%s: handle order must mirror the rendered order", id)
			require.True(t, p.explicitNameSet, "%s: the fixture depends on a PINNED name, not a drawn one", id)
			// The zero padding IS the ordering: "Nav 9" would sort after "Nav 10".
			assert.Equal(t, fmt.Sprintf("Nav %02d", i+1), p.explicitSurname, "%s: entity %d surname", id, i)
			names = append(names, p.explicitGiven+" "+p.explicitSurname)
		}

		for i := 1; i < len(names); i++ {
			assert.Negative(t, strings.Compare(names[i-1], names[i]),
				"%s: name-ascending order must equal insertion order: %q is not before %q", id, names[i-1], names[i])
		}
	}
}

// CON-038's fixture exists to be ANTI-CORRELATED: its three pinned names order
// differently from the cadence order the list defaults to, so an implementation
// that ignored sort=cadence and fell back to name order cannot accidentally pass.
// The citing E2E test asserts the cadence order, which holds under ANY names — so
// a rename to alphabetical literals would leave it green while deleting the
// property the fixture exists to be. This is where that property is pinned.
func TestCadenceSortDeclaration_NamesAreAntiCorrelatedWithCadenceOrder(t *testing.T) {
	d, ok := Lookup("CON-038")
	require.True(t, ok)
	require.Len(t, d.Entities, 3)

	type row struct {
		display string
		period  time.Duration
	}
	rows := make([]row, 0, len(d.Entities))
	periods := map[time.Duration]bool{}
	for _, e := range d.Entities {
		p, isContact := e.(*contactPlan)
		require.True(t, isContact)
		require.True(t, p.explicitNameSet,
			"handle %q must PIN its name — a drawn name cannot be anti-correlated on purpose", p.name)
		require.NotEmpty(t, p.cadence, "handle %q must carry a cadence", p.name)
		require.NotZero(t, period(p.cadence), "handle %q: unknown cadence %q", p.name, p.cadence)
		rows = append(rows, row{display: p.explicitGiven + " " + p.explicitSurname, period: period(p.cadence)})
		periods[period(p.cadence)] = true
	}
	require.Len(t, periods, 3, "the three cadences must have distinct periods, or 'cadence order' is not a total order here")

	displays := func(less func(a, b row) bool) []string {
		sorted := append([]row(nil), rows...)
		sort.Slice(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
		out := make([]string, 0, len(sorted))
		for _, r := range sorted {
			out = append(out, r.display)
		}
		return out
	}

	// The list's default is most-frequent-first, i.e. shortest period first.
	byCadenceDesc := displays(func(a, b row) bool { return a.period < b.period })
	byNameAsc := displays(func(a, b row) bool { return a.display < b.display })
	byNameDesc := displays(func(a, b row) bool { return a.display > b.display })

	assert.NotEqual(t, byCadenceDesc, byNameAsc,
		"name-ascending order must DIFFER from cadence order, or a name-order fallback passes the citing test")
	assert.NotEqual(t, byCadenceDesc, byNameDesc,
		"name-descending order must differ too — a reversed fallback is the same defect")
}

// CON-054's fixture works by a two-phase search: one phase reaches all four
// contacts, the second narrows to the marker and must DROP the unmarked one. A
// marker on all four (or on none) would make the narrowing vacuous.
func TestCadenceFilterDeclaration_MarksAllButTheUnrelatedContact(t *testing.T) {
	d, ok := Lookup("CON-054")
	require.True(t, ok)

	marked, unmarked := map[string]bool{}, map[string]bool{}
	for _, e := range d.Entities {
		p, isContact := e.(*contactPlan)
		require.True(t, isContact)
		if p.nameMarker != nil {
			require.NotEmpty(t, *p.nameMarker)
			marked[p.name] = true
			continue
		}
		unmarked[p.name] = true
	}
	assert.Equal(t, map[string]bool{"weekly": true, "monthly": true, "none": true}, marked)
	assert.Equal(t, map[string]bool{"unrelated": true}, unmarked,
		"the unmarked contact is what proves the narrowing search actually filters")
}
