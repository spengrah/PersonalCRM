package declare

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WorldPlan is a pure function of the two registries plus the tail name, so the
// composition contract is testable with no database and no harness.

func planSegments(t *testing.T, tailName string) (declarations, edges []string, tail WorldStep) {
	t.Helper()
	plan := WorldPlan(tailName)
	require.NotEmpty(t, plan)
	for _, step := range plan[:len(plan)-1] {
		switch step.Kind {
		case WorldStepDeclaration:
			declarations = append(declarations, step.Key)
		case WorldStepEdge:
			edges = append(edges, step.Key)
		default:
			t.Fatalf("unexpected step kind %q before the tail", step.Kind)
		}
	}
	return declarations, edges, plan[len(plan)-1]
}

// Declarations are NORMALIZED to behavior-id order. That is the contract on this
// segment: a declaration's position must not depend on which file's init()
// registered it first.
func TestWorldPlan_DeclarationsAreIDSorted(t *testing.T) {
	declarations, _, _ := planSegments(t, "tail")
	require.NotEmpty(t, declarations, "the registry must not be empty")

	sorted := append([]string(nil), declarations...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, declarations, "the declaration segment must be behavior-id sorted")

	// And the plan really does reflect the registry rather than a parallel list.
	registered := make([]string, 0, len(Registered()))
	for _, d := range Registered() {
		registered = append(registered, d.Behavior)
	}
	assert.Equal(t, registered, declarations)
}

// Edges keep the CATALOG's registration order. This is the opposite contract to
// the one above, and stating it in its own direction is the point: a test that
// only checked "the plan is stable" would pass just as happily if someone
// re-imposed a name sort on the edge segment, which would silently renumber
// every PRNG draw in the world whenever an edge was inserted rather than
// appended.
func TestWorldPlan_EdgesKeepCatalogOrder(t *testing.T) {
	_, edges, _ := planSegments(t, "tail")
	assert.Equal(t, EdgeNames(), edges, "the edge segment must follow the catalog, not a sort")

	sorted := append([]string(nil), edges...)
	sort.Strings(sorted)
	require.NotEqual(t, sorted, edges,
		"the catalog happens to be in alphabetical order, so this test can no longer tell a sort from registration order — "+
			"reorder the catalog or this guard is vacuous")
}

func TestWorldPlan_DeclarationsPrecedeEdges(t *testing.T) {
	plan := WorldPlan("tail")
	lastDeclaration, firstEdge := -1, len(plan)
	for i, step := range plan {
		switch step.Kind {
		case WorldStepDeclaration:
			lastDeclaration = i
		case WorldStepEdge:
			if i < firstEdge {
				firstEdge = i
			}
		}
	}
	assert.Less(t, lastDeclaration, firstEdge, "every declaration must precede every edge")
}

func TestWorldPlan_TailIsLast(t *testing.T) {
	for _, name := range []string{"pinned-tour-fixtures", "another-tail"} {
		_, _, tail := planSegments(t, name)
		assert.Equal(t, WorldStep{Kind: WorldStepTail, Key: name}, tail)
	}
	plan := WorldPlan("only-one")
	tails := 0
	for _, step := range plan {
		if step.Kind == WorldStepTail {
			tails++
		}
	}
	assert.Equal(t, 1, tails, "a world has exactly one tail step — a second one could draw after the pinned fixtures")
}

func TestWorldPlan_CoversEveryRegisteredUnit(t *testing.T) {
	plan := WorldPlan("tail")
	assert.Len(t, plan, len(Registered())+len(Edges())+1,
		"the world is minted from the registries, so a new declaration or edge joins it automatically")
}
