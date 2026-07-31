// Package declare is the single declarative seeding vocabulary for the CRM's
// test worlds: a spec behavior ID names a fixture, and the fixture is stated in
// DOMAIN terms ("a weekly contact overdue by three days") rather than as rows.
//
// A declaration is executed by Run, which builds a synthetic replay harness for
// one namespace, lowers each declared entity onto the toolkit's factory +
// harness primitives, and returns a manifest of what was created. Every write
// goes through the app's real services, so a fixture can only reach states the
// product itself can reach — in particular last_contacted is moved by a
// replayed interaction, never by a column write.
//
// Registration lives in per-domain files (dashboard.go, later contacts.go, ...)
// and runs at init. A behavior that needs NO seeded data declares that
// explicitly via RegisterNone; behaviors not yet migrated sit in the waiver
// table (waivers.go). The completeness unit test requires every ui-surface spec
// behavior to be in exactly one of those three states.
package declare

import (
	"fmt"
	"sort"
	"sync"
)

// Declaration is a fixture keyed by spec behavior ID.
type Declaration struct {
	// Behavior is the spec behavior ID this fixture provisions (e.g. "CAD-026").
	Behavior string
	// Entities are executed in declared order against a fresh per-namespace
	// generator, so ordering is self-contained within one declaration.
	Entities []Entity
}

var (
	registryMu   sync.RWMutex
	declarations = map[string]Declaration{}
	noneReasons  = map[string]string{}
)

// Register adds a declaration. It is init-time only and PANICS on a duplicate,
// empty, or malformed registration: the package is linked only into crm-api
// (whose test routes are env-gated) and every test run executes these inits, so
// a bad registration fails in CI long before it could reach a deploy.
func Register(d Declaration) {
	if d.Behavior == "" {
		panic("declare: Register with an empty Behavior id")
	}
	if len(d.Entities) == 0 {
		panic(fmt.Sprintf("declare: Register(%s) with no entities — use RegisterNone for a behavior that needs no fixture", d.Behavior))
	}
	if err := validateEntityOrder(d.Entities); err != nil {
		panic(fmt.Sprintf("declare: Register(%s): %v", d.Behavior, err))
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	assertUnresolvedLocked(d.Behavior)
	declarations[d.Behavior] = d
}

// RegisterNone records that a behavior's surface needs NO seeded data (static
// navigation, absence claims, route-injected loading/error states). It is a
// RESOLVED state, distinct from a waiver: nothing is owed. Init-time only;
// panics on a duplicate or an empty reason.
func RegisterNone(behaviorID, reason string) {
	if behaviorID == "" {
		panic("declare: RegisterNone with an empty behavior id")
	}
	if reason == "" {
		panic(fmt.Sprintf("declare: RegisterNone(%s) with an empty reason", behaviorID))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	assertUnresolvedLocked(behaviorID)
	noneReasons[behaviorID] = reason
}

// assertUnresolvedLocked panics when behaviorID already carries a registry-side
// resolution. Callers hold registryMu.
func assertUnresolvedLocked(behaviorID string) {
	if _, dup := declarations[behaviorID]; dup {
		panic(fmt.Sprintf("declare: %s is already registered as a declaration", behaviorID))
	}
	if _, dup := noneReasons[behaviorID]; dup {
		panic(fmt.Sprintf("declare: %s is already registered as no-fixture", behaviorID))
	}
}

// Lookup returns the declaration for a behavior id.
func Lookup(behaviorID string) (Declaration, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := declarations[behaviorID]
	return d, ok
}

// IsNone reports whether a behavior is registered as needing no fixture, and
// its reason.
func IsNone(behaviorID string) (string, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reason, ok := noneReasons[behaviorID]
	return reason, ok
}

// Registered returns every declaration sorted by behavior id. The sort is the
// iteration order for World() composition, so it must stay deterministic.
func Registered() []Declaration {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Declaration, 0, len(declarations))
	for _, d := range declarations {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Behavior < out[j].Behavior })
	return out
}

// NoneReasons returns a copy of the no-fixture resolutions.
func NoneReasons() map[string]string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[string]string, len(noneReasons))
	for k, v := range noneReasons {
		out[k] = v
	}
	return out
}

// DeclaredIDs returns the set of behavior ids carrying a declaration.
func DeclaredIDs() map[string]bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[string]bool, len(declarations))
	for k := range declarations {
		out[k] = true
	}
	return out
}
