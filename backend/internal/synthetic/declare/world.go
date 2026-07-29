package declare

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/replay"
)

// Step kinds a world is composed of.
const (
	WorldStepDeclaration = "declaration"
	WorldStepEdge        = "edge"
	WorldStepTail        = "tail"
)

// WorldStep is one composable unit of a world, in execution order.
type WorldStep struct {
	Kind string // "declaration" | "edge" | "tail"
	Key  string // behavior id, edge name, or the tail step's name
}

// WorldStepResult is what one executed step produced.
type WorldStepResult struct {
	Kind     string
	Key      string
	Entities int
	Duration time.Duration
}

// WorldTail is the ONE caller-supplied step that runs after every declaration
// and every edge.
//
// It exists because the pinned tour fixtures live in package synthetic (they
// draw on that package's own location pool and birthday-fixture arithmetic) and
// synthetic imports declare — so declare cannot reach them without a cycle.
// Passing them as a tail keeps ORDERING owned by World, which is what the
// composition invariant constrains, while leaving the fixtures where they are.
//
// Run reports, IN ORDER, what it created. That report is the point: the world's
// execution order is RECORDED rather than re-derived from row timestamps, which
// cannot work — several pinned fixtures are deliberately backdated, so
// created_at order is not execution order.
type WorldTail struct {
	Name string
	Run  func(context.Context, *replay.Harness) ([]Seeded, error)
}

// WorldResult is the manifest of a composed world.
type WorldResult struct {
	Namespace string
	Anchor    time.Time
	// Steps are the completed steps in execution order, so a partial run names
	// where it stopped.
	Steps []WorldStepResult
	// Entities is keyed "<kind>:<key>/<handle>" — handles are step-local, so the
	// composed manifest has to qualify them.
	Entities map[string]Seeded
	// Order is a single monotonic creation log in EXECUTION order, appended to by
	// every declaration, every edge and finally the tail.
	Order []Seeded
}

// WorldPlan is the ORDERING, computed with NO database and no harness, so the
// composition contract can be unit-tested directly instead of inferred from a
// seeded world.
//
// It is ONE sort and two appends, and the distinction is binding: declarations
// are behavior-id sorted (normalization IS the contract there — a declaration's
// position must not depend on which file registered it first), while edges keep
// the adversarial catalog's literal registration order (append-only IS the
// contract there). Re-sorting the edge segment would silently renumber every
// PRNG draw in the world whenever an edge was inserted rather than appended.
func WorldPlan(tailName string) []WorldStep {
	declarations := Registered()
	edges := Edges()
	plan := make([]WorldStep, 0, len(declarations)+len(edges)+1)
	for _, d := range declarations {
		plan = append(plan, WorldStep{Kind: WorldStepDeclaration, Key: d.Behavior})
	}
	for _, e := range edges {
		plan = append(plan, WorldStep{Kind: WorldStepEdge, Key: e.Name})
	}
	plan = append(plan, WorldStep{Kind: WorldStepTail, Key: tailName})
	return plan
}

// World executes every registered declaration (behavior-id sorted), then every
// registered edge (catalog registration order), then the tail — which is LAST by
// construction.
//
// It performs NO namespace reservation and no band claim: the caller
// (crm-admin, which owns the whole database for a --reset-and-seed) IS the
// reservation. It also drains Gate B ONCE, at the end, rather than once per
// step: Gate B is a whole-namespace predicate, so draining it per step would be
// thirteen-plus waits for the same answer. Error attribution survives that
// change because every step's own error is returned with the step NAMED, and
// WorldResult.Steps carries the log of everything that COMPLETED before it — so
// a partial run is diagnosable. That is proven by a fault-injection test rather
// than argued.
func World(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	tail WorldTail,
) (WorldResult, error) {
	if tail.Name == "" || tail.Run == nil {
		return WorldResult{}, fmt.Errorf("declare: World requires a named tail step")
	}

	res := WorldResult{
		Namespace: h.Namespace(),
		Anchor:    h.Generator().Anchor(),
		Entities:  map[string]Seeded{},
	}

	declarations := map[string]Declaration{}
	for _, d := range Registered() {
		declarations[d.Behavior] = d
	}

	for _, step := range WorldPlan(tail.Name) {
		sw := replay.NewStopwatch()
		var handles []string
		var produced []Seeded
		var err error

		switch step.Kind {
		case WorldStepDeclaration:
			handles, produced, err = runWorldEntities(ctx, h, support, declarations[step.Key].Entities)
		case WorldStepEdge:
			edge, ok := LookupEdge(step.Key)
			if !ok {
				err = fmt.Errorf("edge %q disappeared from the registry mid-run", step.Key)
				break
			}
			handles, produced, err = runWorldEntities(ctx, h, support, edge.Entities)
		case WorldStepTail:
			produced, err = tail.Run(ctx, h)
		default:
			err = fmt.Errorf("unknown world step kind %q", step.Kind)
		}
		// The failpoint fires as the STEP's OWN error, deliberately: routing it
		// down the same path a real step failure takes is what makes the
		// fault-injection test cover error PROPAGATION rather than a private
		// branch that happens to also return. Its rows are already written, so
		// the world it leaves behind is genuinely partial.
		if err == nil {
			if key := currentWorldStepFailpoint(); key != "" && key == step.Key {
				err = fmt.Errorf("failpoint %q fired", FailpointAfterWorldStep)
			}
		}
		if err != nil {
			// The failing step is NOT recorded: Steps is the log of what
			// COMPLETED, and the error is what names where the run stopped.
			return res, fmt.Errorf("declare: world step %s %q: %w", step.Kind, step.Key, err)
		}

		for i, seeded := range produced {
			res.Entities[worldEntityKey(step, handles, i)] = seeded
		}
		res.Order = append(res.Order, produced...)
		res.Steps = append(res.Steps, WorldStepResult{
			Kind:     step.Kind,
			Key:      step.Key,
			Entities: len(produced),
			Duration: sw.Elapsed(),
		})
	}

	// ONE drain for the whole world (see the doc comment).
	if hook := currentHook(HookAfterReplayBeforeDrain); hook != nil {
		if err := hook(ctx, h); err != nil {
			return res, fmt.Errorf("declare: %s hook: %w", HookAfterReplayBeforeDrain, err)
		}
	}
	if err := h.DrainGateB(ctx); err != nil {
		return res, fmt.Errorf("declare: world drain: %w", err)
	}
	return res, nil
}

// runWorldEntities executes one step's entities against the shared harness and
// returns their handles and what they created, in the same order. Handles are
// step-local, so each step gets its own run state.
func runWorldEntities(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	entities []Entity,
) ([]string, []Seeded, error) {
	st := newRunState(len(entities))
	if err := runEntityList(ctx, h, support, entities, st); err != nil {
		return nil, nil, err
	}
	return st.orderHandles, st.order, nil
}

// worldEntityKey qualifies a step-local handle for the composed manifest. The
// tail reports rows rather than handles, so its entries are keyed by position.
func worldEntityKey(step WorldStep, handles []string, index int) string {
	if index < len(handles) {
		return fmt.Sprintf("%s:%s/%s", step.Kind, step.Key, handles[index])
	}
	return fmt.Sprintf("%s:%s/%d", step.Kind, step.Key, index)
}
