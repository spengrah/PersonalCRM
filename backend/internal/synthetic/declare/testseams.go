package declare

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"personal-crm/backend/internal/synthetic/replay"
)

// Test seams for the declared-seeding path. They are compiled into the package
// rather than living in a _test.go file because the tests that need them live
// in backend/tests and backend/tests/api — EXTERNAL packages, the same reason
// SeedHostForTest is exported from the repository layer.
//
// Every seam guards itself: it PANICS unless CRM_ENV is test/testing, so a
// production build cannot arm one even by mistake. Each returns a restore func
// the caller MUST defer.

// Failpoint names. A run failpoint produces a DETERMINISTIC mid-world error so
// the partial-world recovery path and the endpoint's 500 shape can be
// exercised. (An "OverdueBy + NoMethods" declaration is not a usable substitute
// — the factory substitutes an unknown synthetic address rather than failing,
// so that combination is a Register-time validation case, not a runtime error.)
const (
	// FailpointAfterFirstEntity fires immediately after the FIRST entity of a
	// declaration is created, leaving a genuinely partial world behind.
	FailpointAfterFirstEntity = "after-first-entity"
)

// Test-hook points. A hook runs at a named point in the protocol with the live
// harness, so a test can plant rows at the exact instant a race would.
const (
	// HookAfterReplayBeforeDrain runs after every declared entity is created
	// and BEFORE Run's final Gate-B drain — the realizable injection point for
	// "a job is still pending when the seed claims success".
	HookAfterReplayBeforeDrain = "after-replay-before-drain"
	// HookAfterBandSwapBeforeRevalidate runs inside the re-salt lock swap,
	// after the effective namespace's band locks are acquired and before they
	// are revalidated — the release-acquire gap a third run could seed into.
	HookAfterBandSwapBeforeRevalidate = "after-band-swap-before-revalidate"
)

// TestHook is a named injection point's callback.
type TestHook func(ctx context.Context, h *replay.Harness) error

var (
	seamMu           sync.Mutex
	armedFailpoint   string
	armedHooks       = map[string]TestHook{}
	armedCleanupFail string
	budgetRun        = defaultRunBudget
	budgetTeardown   = defaultTeardownBudget
)

var knownFailpoints = map[string]bool{
	FailpointAfterFirstEntity: true,
}

var knownHookPoints = map[string]bool{
	HookAfterReplayBeforeDrain:        true,
	HookAfterBandSwapBeforeRevalidate: true,
}

// SetFailpointForTest arms a run failpoint. Pass "" to disarm. Panics on an
// unknown name — a typo that silently never fired would make the test that
// depends on it pass for the wrong reason.
func SetFailpointForTest(name string) (restore func()) {
	requireTestEnv("declare.SetFailpointForTest")
	if name != "" && !knownFailpoints[name] {
		panic(fmt.Sprintf("declare: unknown failpoint %q", name))
	}
	seamMu.Lock()
	prev := armedFailpoint
	armedFailpoint = name
	seamMu.Unlock()
	return func() {
		seamMu.Lock()
		armedFailpoint = prev
		seamMu.Unlock()
	}
}

// SetTestHookForTest installs a hook at a named point. Pass a nil fn to clear.
// Panics on an unknown point, for the same reason as SetFailpointForTest.
func SetTestHookForTest(point string, fn TestHook) (restore func()) {
	requireTestEnv("declare.SetTestHookForTest")
	if !knownHookPoints[point] {
		panic(fmt.Sprintf("declare: unknown test-hook point %q", point))
	}
	seamMu.Lock()
	prev, had := armedHooks[point]
	if fn == nil {
		delete(armedHooks, point)
	} else {
		armedHooks[point] = fn
	}
	seamMu.Unlock()
	return func() {
		seamMu.Lock()
		if had {
			armedHooks[point] = prev
		} else {
			delete(armedHooks, point)
		}
		seamMu.Unlock()
	}
}

// SetCleanupFailStepForTest makes the cleanup ladder fail at the named step
// instead of executing it, so the marker-retention contract (a partially failed
// ladder keeps the namespace discoverable AND occupied) can be proven rather
// than assumed. Pass "" to disarm.
func SetCleanupFailStepForTest(step string) (restore func()) {
	requireTestEnv("declare.SetCleanupFailStepForTest")
	seamMu.Lock()
	prev := armedCleanupFail
	armedCleanupFail = step
	seamMu.Unlock()
	return func() {
		seamMu.Lock()
		armedCleanupFail = prev
		seamMu.Unlock()
	}
}

// SetBudgetsForTest shrinks the run/teardown budgets so the bounded-run
// assertion does not have to wait out the production values.
func SetBudgetsForTest(run, teardown time.Duration) (restore func()) {
	requireTestEnv("declare.SetBudgetsForTest")
	seamMu.Lock()
	prevRun, prevTeardown := budgetRun, budgetTeardown
	budgetRun, budgetTeardown = run, teardown
	seamMu.Unlock()
	return func() {
		seamMu.Lock()
		budgetRun, budgetTeardown = prevRun, prevTeardown
		seamMu.Unlock()
	}
}

// WorstCaseRunResidence is the honest upper bound on how long a Run can occupy
// its caller: the run budget, plus AT MOST ONE in-flight toolkit settle timer
// (the settle waits poll on their own fixed real-time budget and do not observe
// the run context — Run checks ctx.Err() between steps, so at most one can be
// mid-flight when the deadline fires), plus teardown's own Gate-B timer, plus
// the teardown budget. Every term is a fixed constant; the point is that the
// bound is FINITE and its arithmetic true, not that it is minimal.
func WorstCaseRunResidence() time.Duration {
	seamMu.Lock()
	run, teardown := budgetRun, budgetTeardown
	seamMu.Unlock()
	return run + replay.SettleBudget() + replay.TeardownGateBBudget() + teardown
}

func currentFailpoint() string {
	seamMu.Lock()
	defer seamMu.Unlock()
	return armedFailpoint
}

func currentHook(point string) TestHook {
	seamMu.Lock()
	defer seamMu.Unlock()
	return armedHooks[point]
}

func currentCleanupFailStep() string {
	seamMu.Lock()
	defer seamMu.Unlock()
	return armedCleanupFail
}

func runBudget() time.Duration {
	seamMu.Lock()
	defer seamMu.Unlock()
	return budgetRun
}

func teardownBudget() time.Duration {
	seamMu.Lock()
	defer seamMu.Unlock()
	return budgetTeardown
}

// requireTestEnv panics unless the process is running under a test environment.
func requireTestEnv(what string) {
	switch os.Getenv("CRM_ENV") {
	case "test", "testing":
		return
	default:
		panic(fmt.Sprintf("%s is test-only support and requires CRM_ENV=test|testing (got %q)", what, os.Getenv("CRM_ENV")))
	}
}
