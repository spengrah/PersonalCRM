package declare

import (
	"fmt"
	"sort"

	"personal-crm/backend/internal/spec"
)

// Problem kinds reported by CheckCompleteness.
const (
	// ProblemUnresolved: a ui-surface behavior has no declaration, no
	// RegisterNone, and no waiver.
	ProblemUnresolved = "unresolved"
	// ProblemDoubleResolved: a behavior carries two or more of the three
	// resolutions, so its state is ambiguous.
	ProblemDoubleResolved = "double-resolved"
	// ProblemStale: a resolution names an id outside the universe (retired,
	// non-ui, or gone).
	ProblemStale = "stale"
)

// Problem is one completeness violation.
type Problem struct {
	BehaviorID string
	Kind       string
	Detail     string
}

func (p Problem) String() string {
	return fmt.Sprintf("%s [%s]: %s", p.BehaviorID, p.Kind, p.Detail)
}

// CheckCompleteness is the pure completeness checker: every ui-surface,
// non-retired spec behavior must be in EXACTLY ONE of three states — declared
// (a Declaration), none (RegisterNone), or waived — and no resolution may name
// an id outside that universe.
//
// It takes the three resolution maps rather than reading the package registry
// so it is directly table-testable with synthetic inputs; the real test wires
// it to DeclaredIDs()/NoneReasons()/Waivers over the parsed corpus.
func CheckCompleteness(files []*spec.File, declared map[string]bool, none map[string]string, waivers map[string]string) []Problem {
	universe := map[string]bool{}
	for _, f := range files {
		if f == nil {
			continue
		}
		for i := range f.Behaviors {
			b := &f.Behaviors[i]
			if b.Surface == "ui" && b.Status != "retired" {
				universe[b.ID] = true
			}
		}
	}

	var problems []Problem

	for id := range universe {
		var states []string
		if declared[id] {
			states = append(states, "declaration")
		}
		if _, ok := none[id]; ok {
			states = append(states, "none")
		}
		if _, ok := waivers[id]; ok {
			states = append(states, "waiver")
		}
		switch {
		case len(states) == 0:
			problems = append(problems, Problem{
				BehaviorID: id,
				Kind:       ProblemUnresolved,
				Detail:     "ui-surface behavior has no declaration, no RegisterNone, and no waiver",
			})
		case len(states) > 1:
			sort.Strings(states)
			problems = append(problems, Problem{
				BehaviorID: id,
				Kind:       ProblemDoubleResolved,
				Detail:     fmt.Sprintf("resolved %d ways (%v) — exactly one is allowed", len(states), states),
			})
		}
	}

	stale := func(id, source string) {
		if universe[id] {
			return
		}
		problems = append(problems, Problem{
			BehaviorID: id,
			Kind:       ProblemStale,
			Detail:     source + " names an id that is not a live ui-surface behavior (retired, non-ui, or removed)",
		})
	}
	for id := range declared {
		stale(id, "declaration")
	}
	for id := range none {
		stale(id, "RegisterNone")
	}
	for id := range waivers {
		stale(id, "waiver")
	}

	sort.Slice(problems, func(i, j int) bool {
		if problems[i].BehaviorID != problems[j].BehaviorID {
			return problems[i].BehaviorID < problems[j].BehaviorID
		}
		return problems[i].Kind < problems[j].Kind
	})
	return problems
}
