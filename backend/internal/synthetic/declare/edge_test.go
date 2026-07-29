package declare

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedEdgeNames is the catalog, spelled out.
//
// It is a LITERAL on purpose. Deriving it from adversarialCatalog would make the
// test tautological; stating it is what makes inserting an edge in the middle
// fail, which IS the append-only property the composition contract depends on
// (edges compose in registration order, so a mid-list insert silently renumbers
// every PRNG draw in the world after it). Adding an edge means appending here.
var expectedEdgeNames = []string{
	"long-history",
	"zero-method",
	"hostile-names",
	"all-cadences-overdue",
	"fully-empty",
	"deep-import-queue",
	"merge-chain",
	"soft-deleted-parent",
	"page-overflow",
	"same-name-pair",
	"birthday-window",
}

func TestEdges_AreTheCatalogInOrder(t *testing.T) {
	assert.Equal(t, expectedEdgeNames, EdgeNames(),
		"the catalog is append-only: a new edge goes at the END of adversarialCatalog and at the END of this list")
}

// Every registered edge must come from the single catalog literal. A second
// init() in another file could otherwise register one at a position that depends
// on filename ordering — exactly the emergent behavior making the catalog an
// explicit slice exists to prevent.
func TestEdges_AllComeFromTheCatalogLiteral(t *testing.T) {
	fromCatalog := map[string]bool{}
	for _, e := range adversarialCatalog {
		fromCatalog[e.Name] = true
	}
	for _, e := range Edges() {
		assert.True(t, fromCatalog[e.Name],
			"edge %q is registered but is not in adversarialCatalog — registration order would then depend on file layout", e.Name)
	}
	assert.Len(t, Edges(), len(adversarialCatalog))
}

func TestEdges_EachCarriesADefectClass(t *testing.T) {
	for _, e := range Edges() {
		assert.NotEmpty(t, e.Why, "edge %q must state the defect class it catches", e.Name)
		assert.NotEmpty(t, e.Entities, "edge %q must create something", e.Name)
	}
}

// An edge name must never be shaped like a spec behavior id: the two namespaces
// are separate, and an edge that looked like a behavior could be mistaken for
// one resolving a ui behavior it does not resolve.
func TestRegisterEdge_RejectsABehaviorShapedName(t *testing.T) {
	// The lowercase form passes the kebab-case grammar, so this guard is the only
	// thing standing between it and the registry. (The uppercase spelling is
	// rejected one check earlier, by the grammar itself.)
	assert.PanicsWithValue(t,
		"declare: RegisterEdge(con-001): an edge name must not be shaped like a spec behavior id",
		func() { RegisterEdge(Edge{Name: "con-001", Why: "x", Entities: []Entity{Contact("a")}}) })
	assert.PanicsWithValue(t,
		"declare: RegisterEdge(CON-001): edge names are lowercase kebab-case",
		func() { RegisterEdge(Edge{Name: "CON-001", Why: "x", Entities: []Entity{Contact("a")}}) })
}

func TestRegisterEdge_RejectsMalformedRegistrations(t *testing.T) {
	cases := map[string]Edge{
		"empty name":     {Why: "x", Entities: []Entity{Contact("a")}},
		"upper case":     {Name: "Long-History", Why: "x", Entities: []Entity{Contact("a")}},
		"underscore":     {Name: "long_history", Why: "x", Entities: []Entity{Contact("a")}},
		"no why":         {Name: "no-why", Entities: []Entity{Contact("a")}},
		"no entities":    {Name: "no-entities", Why: "x"},
		"duplicate name": {Name: "long-history", Why: "x", Entities: []Entity{Contact("a")}},
		"bad entity":     {Name: "bad-entity", Why: "x", Entities: []Entity{Contact("a", Cadence("fortnightly"))}},
		"forward ref":    {Name: "forward-ref", Why: "x", Entities: []Entity{Contact("a", SameNameAs("b")), Contact("b")}},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Panics(t, func() { RegisterEdge(e) })
		})
	}
}

func TestLookupEdge(t *testing.T) {
	e, ok := LookupEdge("merge-chain")
	require.True(t, ok)
	assert.Equal(t, "merge-chain", e.Name)

	_, ok = LookupEdge("no-such-edge")
	assert.False(t, ok)
}

// The catalog must not leak into the behavior-completeness universe: an edge is
// not a spec behavior, and counting one as resolving a behavior would let a ui
// behavior go unresolved while the completeness check passed.
func TestEdges_AreNotDeclarations(t *testing.T) {
	declared := DeclaredIDs()
	none := NoneReasons()
	for _, e := range Edges() {
		assert.False(t, declared[e.Name], "edge %q must not be a declaration", e.Name)
		_, isNone := none[e.Name]
		assert.False(t, isNone, "edge %q must not be a no-fixture resolution", e.Name)
	}
}

// --- catalog content the composed world's arithmetic depends on -------------

func TestCatalog_DeepImportQueueSplitsEvenlyAcrossBothKeyingShapes(t *testing.T) {
	e, ok := LookupEdge("deep-import-queue")
	require.True(t, ok)

	bySource := map[string]int{}
	for _, entity := range e.Entities {
		p, isCandidate := entity.(*externalCandidatePlan)
		require.True(t, isCandidate, "deep-import-queue declares only import candidates")
		bySource[p.source]++
	}
	assert.Equal(t, map[string]int{
		SourceGContacts:      deepImportQueueCountPerSource,
		SourceCorrespondence: deepImportQueueCountPerSource,
	}, bySource, "an uneven split would make the per-source pagination assertion ambiguous")
}

func TestCatalog_PageOverflowCohortIsOverFifty(t *testing.T) {
	e, ok := LookupEdge("page-overflow")
	require.True(t, ok)

	overdue := 0
	for _, pc := range e.Postconditions() {
		if pc.OverdueMember != nil && *pc.OverdueMember {
			overdue++
		}
	}
	assert.Greater(t, overdue, 50, "the cohort itself must exceed fifty overdue, not only once the rest of the world is added")
	assert.Len(t, e.Entities, pageOverflowOverdue+pageOverflowFresh)
}

func TestCatalog_AllCadencesOverdueCoversTheWholeVocabulary(t *testing.T) {
	e, ok := LookupEdge("all-cadences-overdue")
	require.True(t, ok)

	seen := map[string]bool{}
	for _, pc := range e.Postconditions() {
		require.NotNil(t, pc.Cadence)
		require.NotNil(t, pc.OverdueMember)
		assert.True(t, *pc.OverdueMember, "handle %q must be overdue", pc.Handle)
		seen[*pc.Cadence] = true
	}
	for _, c := range Cadences() {
		assert.True(t, seen[c], "cadence %q is missing from the all-cadences edge", c)
	}
	assert.Len(t, seen, len(Cadences()))
}

// The merge chain's losers must be derived as ABSENT, and the survivor as
// present — the postcondition derivation is what the satisfiability subtest
// asserts against, so it has to reflect two hops rather than one.
func TestCatalog_MergeChainDerivesBothLosersAbsent(t *testing.T) {
	e, ok := LookupEdge("merge-chain")
	require.True(t, ok)

	present := map[string]bool{}
	for _, pc := range e.Postconditions() {
		present[pc.Handle] = pc.Present == nil || *pc.Present
	}
	assert.False(t, present["a"], "a is merged into b, so it must be absent")
	assert.False(t, present["b"], "b is merged into c, so it must be absent too — the second hop is the point")
	assert.True(t, present["c"], "c survives the chain")
}

func TestCatalog_SoftDeletedParentLeavesTheOverdueRead(t *testing.T) {
	e, ok := LookupEdge("soft-deleted-parent")
	require.True(t, ok)

	for _, pc := range e.Postconditions() {
		if pc.Handle != "parent" {
			continue
		}
		require.NotNil(t, pc.Present)
		assert.False(t, *pc.Present)
		require.NotNil(t, pc.OverdueMember)
		assert.False(t, *pc.OverdueMember, "a tombstoned contact must leave the overdue read despite being overdue at seed time")
		return
	}
	t.Fatal("the soft-deleted-parent edge has no parent postcondition")
}
