package declare

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// The list's default is most-frequent-first, i.e. shortest period first. The
	// two name orders are Go byte order — sound here only because these literals
	// are pure ASCII (see the back-nav cohort test above), and because the claim is
	// an INEQUALITY: a collation that reorders them still differs from the cadence
	// order these names were chosen to contradict.
	byCadenceDesc := displays(func(a, b row) bool { return a.period < b.period })
	byNameAsc := displays(func(a, b row) bool { return a.display < b.display })
	byNameDesc := displays(func(a, b row) bool { return a.display > b.display })

	assert.NotEqual(t, byCadenceDesc, byNameAsc,
		"name-ascending order must DIFFER from cadence order, or a name-order fallback passes the citing test")
	assert.NotEqual(t, byCadenceDesc, byNameDesc,
		"name-descending order must differ too — a reversed fallback is the same defect")
}
