package declare

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
)

// World step and final protocol-phase kinds.
const (
	WorldStepDeclaration = "declaration"
	WorldStepEdge        = "edge"
	WorldStepTail        = "tail"
	WorldStepValidation  = "validation"
	WorldStepDrain       = "drain"
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
	// Current names the step or final world phase that was running when World
	// returned an error. Completed steps remain in Steps; any entities completed
	// inside Current before its error remain in Order and Current.Entities.
	Current *WorldStepResult
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
	beforeContacts := stringSet(h.CreatedContactIDs())

	declarations := map[string]Declaration{}
	for _, d := range Registered() {
		declarations[d.Behavior] = d
	}

	runStep := func(step WorldStep) ([]string, []Seeded, error) {
		var handles []string
		var produced []Seeded
		var err error

		switch step.Kind {
		case WorldStepDeclaration:
			handles, produced, err = runWorldEntities(ctx, h, support, declarations[step.Key].Entities)
		case WorldStepEdge:
			edge, ok := LookupEdge(step.Key)
			if !ok {
				return nil, nil, fmt.Errorf("edge %q disappeared from the registry mid-run", step.Key)
			}
			handles, produced, err = runWorldEntities(ctx, h, support, edge.Entities)
		case WorldStepTail:
			produced, err = tail.Run(ctx, h)
		default:
			err = fmt.Errorf("unknown world step kind %q", step.Kind)
		}
		return handles, produced, err
	}

	return executeWorld(
		ctx,
		res,
		WorldPlan(tail.Name),
		runStep,
		func() []string {
			return newStringsSince(beforeContacts, h.CreatedContactIDs())
		},
		func() error {
			if hook := currentHook(HookAfterReplayBeforeDrain); hook != nil {
				return hook(ctx, h)
			}
			return nil
		},
		func() error { return h.DrainGateB(ctx) },
	)
}

type worldStepRunner func(WorldStep) ([]string, []Seeded, error)

// executeWorld is the small execution protocol behind World. Keeping the plan,
// step runner, observer and final drain injectable lets failure propagation be
// tested without constructing the full registered world.
func executeWorld(
	ctx context.Context,
	res WorldResult,
	plan []WorldStep,
	runStep worldStepRunner,
	observedContactIDs func() []string,
	beforeDrain func() error,
	drain func() error,
) (WorldResult, error) {
	for _, step := range plan {
		if err := ctx.Err(); err != nil {
			res.Current = &WorldStepResult{Kind: step.Kind, Key: step.Key}
			return res, fmt.Errorf("declare: world step %s %q: %w", step.Kind, step.Key, err)
		}
		sw := replay.NewStopwatch()
		handles, produced, err := runStep(step)
		for i, seeded := range produced {
			res.Entities[worldEntityKey(step, handles, i)] = seeded
		}
		res.Order = append(res.Order, produced...)
		if err != nil {
			// The failing step is not in Steps, but it remains explicitly visible
			// with any completed partial entities it produced before returning.
			res.Current = &WorldStepResult{
				Kind:     step.Kind,
				Key:      step.Key,
				Entities: len(produced),
				Duration: sw.Elapsed(),
			}
			return res, fmt.Errorf("declare: world step %s %q: %w", step.Kind, step.Key, err)
		}

		res.Steps = append(res.Steps, WorldStepResult{
			Kind:     step.Kind,
			Key:      step.Key,
			Entities: len(produced),
			Duration: sw.Elapsed(),
		})
	}

	if err := validateWorldContactManifest(res.Order, observedContactIDs()); err != nil {
		res.Current = &WorldStepResult{Kind: WorldStepValidation, Key: "contact-manifest"}
		return res, fmt.Errorf("declare: world contact manifest: %w", err)
	}

	// ONE drain for the whole world (see the doc comment).
	if err := beforeDrain(); err != nil {
		res.Current = &WorldStepResult{Kind: WorldStepDrain, Key: HookAfterReplayBeforeDrain}
		return res, fmt.Errorf("declare: %s hook: %w", HookAfterReplayBeforeDrain, err)
	}
	if err := drain(); err != nil {
		res.Current = &WorldStepResult{Kind: WorldStepDrain, Key: "gate-b"}
		return res, fmt.Errorf("declare: world drain: %w", err)
	}
	return res, nil
}

func stringSet(ids []uuid.UUID) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id.String()] = struct{}{}
	}
	return out
}

func newStringsSince(before map[string]struct{}, after []uuid.UUID) []string {
	out := make([]string, 0, len(after))
	for _, id := range after {
		value := id.String()
		if _, existed := before[value]; !existed {
			out = append(out, value)
		}
	}
	return out
}

func validateWorldContactManifest(order []Seeded, observed []string) error {
	reported := make(map[string]int)
	for _, seeded := range order {
		if seeded.Kind == "contact" {
			reported[seeded.ID]++
		}
	}
	actual := make(map[string]int, len(observed))
	for _, id := range observed {
		actual[id]++
	}
	if len(reported) != len(actual) {
		return fmt.Errorf("reported %d contact IDs but harness observed %d", len(reported), len(actual))
	}
	for id, count := range actual {
		if reported[id] != count {
			return fmt.Errorf("contact %s observed %d times but reported %d", id, count, reported[id])
		}
	}
	return nil
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
	err := runEntityList(ctx, h, support, entities, st)
	return st.orderHandles, st.order, err
}

// worldEntityKey qualifies a step-local handle for the composed manifest. The
// tail reports rows rather than handles, so its entries are keyed by position.
func worldEntityKey(step WorldStep, handles []string, index int) string {
	if index < len(handles) {
		return fmt.Sprintf("%s:%s/%s", step.Kind, step.Key, handles[index])
	}
	return fmt.Sprintf("%s:%s/%d", step.Kind, step.Key, index)
}
