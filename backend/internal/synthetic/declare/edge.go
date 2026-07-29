package declare

import (
	"fmt"
	"regexp"
	"sync"
)

// An Edge is a hostile world shape, keyed by a stable edge NAME rather than a
// spec behavior id.
//
// Edges are never asserted STATISTICALLY. An edge either exists in the world or
// it does not, and the satisfiability suite proves the difference through the
// API read path — which is the whole point of replacing a distribution-matching
// layer with named adversarial states: a distribution can be "close enough"
// forever, whereas a missing edge fails a named subtest.
//
// Edges live in their own ordered registry rather than the declaration one, so
// the ui-behavior completeness universe is untouched: an edge is not a behavior
// and must never be counted as resolving one.
type Edge struct {
	// Name is the stable key: lowercase kebab-case, unique, and deliberately NOT
	// shaped like a behavior id (a "XXX-000" name panics) so an edge can never
	// leak into the completeness universe.
	Name string
	// Why is the defect class this edge exists to catch — one line, and the thing
	// a future reader needs when deciding whether the edge still earns its cost.
	Why string
	// Entities execute in declared order against the same per-namespace
	// generator, exactly as a Declaration's do.
	Entities []Entity
}

var (
	edgeMu    sync.RWMutex
	edgeOrder []Edge
	edgeIndex = map[string]int{}
)

// edgeNamePattern is the edge-name grammar: lowercase alphanumeric segments
// joined by single hyphens.
var edgeNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// behaviorIDPattern is the SPEC behavior-id grammar. An edge name matching it is
// rejected: the two namespaces must stay visibly distinct so an edge can never
// be mistaken for — or counted as — a resolved ui behavior.
var behaviorIDPattern = regexp.MustCompile(`^[A-Za-z]{2,}-[0-9]{3}$`)

// RegisterEdge adds an adversarial edge. Init-time only, and it PANICS on a
// duplicate, empty, malformed or behavior-shaped name for the same reason
// Register does: every test run executes these inits, so a bad registration
// fails in CI long before it could reach a deploy.
func RegisterEdge(e Edge) {
	if e.Name == "" {
		panic("declare: RegisterEdge with an empty Name")
	}
	if !edgeNamePattern.MatchString(e.Name) {
		panic(fmt.Sprintf("declare: RegisterEdge(%s): edge names are lowercase kebab-case", e.Name))
	}
	if behaviorIDPattern.MatchString(e.Name) {
		panic(fmt.Sprintf("declare: RegisterEdge(%s): an edge name must not be shaped like a spec behavior id", e.Name))
	}
	if e.Why == "" {
		panic(fmt.Sprintf("declare: RegisterEdge(%s) with an empty Why — an edge whose defect class nobody can state cannot be judged worth its cost", e.Name))
	}
	if len(e.Entities) == 0 {
		panic(fmt.Sprintf("declare: RegisterEdge(%s) with no entities", e.Name))
	}
	if err := validateEntityOrder(e.Entities); err != nil {
		panic(fmt.Sprintf("declare: RegisterEdge(%s): %v", e.Name, err))
	}

	edgeMu.Lock()
	defer edgeMu.Unlock()
	if _, dup := edgeIndex[e.Name]; dup {
		panic(fmt.Sprintf("declare: edge %s is already registered", e.Name))
	}
	edgeIndex[e.Name] = len(edgeOrder)
	edgeOrder = append(edgeOrder, e)
}

// Edges returns every registered edge in REGISTRATION order.
//
// Registration order, not name order, is the composition contract (arc
// invariant I5: the world is append-only). Sorting here would silently
// renumber every PRNG draw in the composed world whenever an edge was inserted
// rather than appended, which is exactly the churn the append-only rule exists
// to make visible.
func Edges() []Edge {
	edgeMu.RLock()
	defer edgeMu.RUnlock()
	return append([]Edge(nil), edgeOrder...)
}

// LookupEdge returns the edge registered under a name.
func LookupEdge(name string) (Edge, bool) {
	edgeMu.RLock()
	defer edgeMu.RUnlock()
	i, ok := edgeIndex[name]
	if !ok {
		return Edge{}, false
	}
	return edgeOrder[i], true
}

// EdgeNames returns the registered edge names in registration order.
func EdgeNames() []string {
	edgeMu.RLock()
	defer edgeMu.RUnlock()
	out := make([]string, 0, len(edgeOrder))
	for _, e := range edgeOrder {
		out = append(out, e.Name)
	}
	return out
}
