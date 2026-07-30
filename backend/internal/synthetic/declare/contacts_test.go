package declare

import (
	"fmt"
	"strings"
	"testing"

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

// The back-nav fixture's whole purpose is that name-ASCENDING order equals
// insertion order, so p21 is the sole row of page 2 at a twenty-row page size.
// That only holds while the pinned literals stay zero-padded.
func TestBackNavFixtureEntities_AreNameOrderedByConstruction(t *testing.T) {
	entities := backNavFixtureEntities()
	require.Len(t, entities, 21)

	names := make([]string, 0, len(entities))
	for i, e := range entities {
		p, ok := e.(*contactPlan)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("p%02d", i+1), p.name, "handle order must mirror the rendered order")
		require.NotEmpty(t, p.explicitGiven, "the fixture depends on a PINNED name, not a drawn one")
		names = append(names, p.explicitGiven+" "+p.explicitSurname)
	}

	for i := 1; i < len(names); i++ {
		assert.Negative(t, strings.Compare(names[i-1], names[i]),
			"name-ascending order must equal insertion order: %q is not before %q", names[i-1], names[i])
	}
	assert.Equal(t, "Back Nav 01", names[0])
	assert.Equal(t, "Back Nav 21", names[20])
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
