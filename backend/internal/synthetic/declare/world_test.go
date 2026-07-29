package declare

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

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

func TestExecuteWorld_FinalDrainFailureUsesTheProductionErrorPath(t *testing.T) {
	res, err := executeWorld(
		context.Background(),
		WorldResult{Entities: map[string]Seeded{}},
		[]WorldStep{{Kind: WorldStepTail, Key: "minimal-tail"}},
		func(WorldStep) ([]string, []Seeded, error) { return nil, nil, nil },
		func() []string { return nil },
		func() error { return nil },
		func() error { return errors.New("injected drain failure") },
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "world drain")
	require.NotNil(t, res.Current)
	assert.Equal(t, WorldStepDrain, res.Current.Kind)
	assert.Equal(t, "gate-b", res.Current.Key)
	require.Len(t, res.Steps, 1)
	assert.Equal(t, []WorldStepResult{{
		Kind: WorldStepTail, Key: "minimal-tail", Entities: 0, Duration: res.Steps[0].Duration,
	}}, res.Steps)
	assert.Less(t, res.Steps[0].Duration, time.Second)
}

func TestExecuteWorld_FailuresNameTheActualStepAndKeepPartialEntities(t *testing.T) {
	plan := []WorldStep{
		{Kind: WorldStepDeclaration, Key: "early"},
		{Kind: WorldStepEdge, Key: "middle"},
		{Kind: WorldStepTail, Key: "final"},
	}
	for failAt := range plan {
		t.Run(plan[failAt].Key, func(t *testing.T) {
			res, err := executeWorld(
				context.Background(),
				WorldResult{Entities: map[string]Seeded{}},
				plan,
				func(step WorldStep) ([]string, []Seeded, error) {
					produced := []Seeded{{Kind: "contact", ID: step.Key}}
					if step.Key == plan[failAt].Key {
						return []string{"subject"}, produced, errors.New("injected step failure")
					}
					return []string{"subject"}, produced, nil
				},
				func() []string { return nil },
				func() error { return nil },
				func() error { return nil },
			)

			require.Error(t, err)
			require.NotNil(t, res.Current)
			assert.Equal(t, plan[failAt].Kind, res.Current.Kind)
			assert.Equal(t, plan[failAt].Key, res.Current.Key)
			assert.Equal(t, 1, res.Current.Entities)
			assert.Len(t, res.Steps, failAt)
			assert.Len(t, res.Order, failAt+1,
				"the failing step's completed partial entity must remain observable")
		})
	}
}

func TestExecuteWorld_RejectsAnUnreportedTailContact(t *testing.T) {
	res, err := executeWorld(
		context.Background(),
		WorldResult{Entities: map[string]Seeded{}},
		[]WorldStep{{Kind: WorldStepTail, Key: "fixtures"}},
		func(WorldStep) ([]string, []Seeded, error) {
			return nil, []Seeded{{Kind: "contact", ID: "fixture-a"}}, nil
		},
		func() []string { return []string{"fixture-a", "created-after-fixtures"} },
		func() error { return nil },
		func() error { return nil },
	)

	require.Error(t, err, "an extra contact omitted from the tail report must fail")
	assert.Contains(t, err.Error(), "reported 1 contact IDs but harness observed 2")
	require.NotNil(t, res.Current)
	assert.Equal(t, WorldStepValidation, res.Current.Kind)
	assert.Equal(t, "contact-manifest", res.Current.Key)
}
