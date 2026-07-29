package declare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/replay"
)

// Test seams for the declared-seeding path. They are compiled into the package
// rather than living in a _test.go file because the tests that need them live
// in backend/tests and backend/tests/api — EXTERNAL packages, the same reason
// SeedHostForTest is exported from the repository layer.
//
// Every seam guards itself: it PANICS unless the process is a `go test` binary
// or already runs under CRM_ENV=test|testing, so a production build cannot arm
// one even by mistake. Each returns a restore func the caller MUST defer.

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
	armedUnlockFail  string
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

// Unlock failpoint modes. pg_advisory_unlock has TWO failure shapes and the
// abort rule covers both, so both have to be injectable — neither can be
// produced on demand against a live healthy session.
const (
	// UnlockFailpointError is the call itself failing, the shape a lost or
	// broken connection produces.
	UnlockFailpointError = "error"
	// UnlockFailpointNotHeld is pg_advisory_unlock returning FALSE: no error,
	// but the session did not hold the lock it thought it held. That means the
	// caller's model of what it holds is wrong and the lock may still be held
	// by someone — a silent "success" here is exactly the state the abort rule
	// exists to refuse to continue from.
	UnlockFailpointNotHeld = "not-held"
)

var knownUnlockFailpoints = map[string]bool{
	UnlockFailpointError:   true,
	UnlockFailpointNotHeld: true,
}

// SetUnlockFailpointForTest makes every advisory-lock RELEASE fail in the named
// mode without actually releasing anything. It is the only way to exercise the
// abort-on-failed-release rule. Pass "" to disarm; panics on an unknown mode,
// for the same reason as SetFailpointForTest.
func SetUnlockFailpointForTest(mode string) (restore func()) {
	requireTestEnv("declare.SetUnlockFailpointForTest")
	if mode != "" && !knownUnlockFailpoints[mode] {
		panic(fmt.Sprintf("declare: unknown unlock failpoint mode %q", mode))
	}
	seamMu.Lock()
	prev := armedUnlockFail
	armedUnlockFail = mode
	seamMu.Unlock()
	return func() {
		seamMu.Lock()
		armedUnlockFail = prev
		seamMu.Unlock()
	}
}

// NamespaceFamilyForTest is the COMPLETE set of tokens one run's reservation
// covers: the requested namespace plus every salted variant re-salting can
// mint. A test that spelled this family out itself would silently stop covering
// the whole thing the day the salt budget changed, so it comes from the same
// derivation the reservation uses.
func NamespaceFamilyForTest(namespace string) []string {
	requireTestEnv("declare.NamespaceFamilyForTest")
	return append([]string{namespace}, saltVariants(namespace)...)
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

// AdvisoryKeyForTest exposes the reservation key derivation so a test can hold
// the SAME lock a run would take (the held-lock refusal case). Recomputing the
// hash test-side would prove nothing about the key the run actually uses.
func AdvisoryKeyForTest(token string) int64 {
	requireTestEnv("declare.AdvisoryKeyForTest")
	return advisoryKey(token)
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

// unlockFailpoint reports the injected release outcome. `injected` is false
// when no mode is armed, in which case the caller performs the real release.
func unlockFailpoint() (injected, released bool, err error) {
	seamMu.Lock()
	mode := armedUnlockFail
	seamMu.Unlock()
	switch mode {
	case UnlockFailpointError:
		return true, false, errors.New("declare: unlock failpoint fired")
	case UnlockFailpointNotHeld:
		// No error, but the lock was NOT released — the branch a caller that
		// only inspected `err` would walk straight past.
		return true, false, nil
	default:
		return false, false, nil
	}
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

// testSeamsAllowed is the misuse guard's pure predicate: a seam may be armed
// only from a binary built by `go test`, or from a process already running
// under the test environment that gates the whole test-route surface. The
// test-binary arm is the strong one — it cannot be set by configuration — and
// it is what lets an integration test arm a seam WITHOUT also switching the
// process onto the compressed cadence table, which would change the meaning of
// every duration the test asserts.
func testSeamsAllowed(crmEnv string, testBinary bool) bool {
	if testBinary {
		return true
	}
	switch crmEnv {
	case "test", "testing":
		return true
	default:
		return false
	}
}

// requireTestEnv panics unless test seams may be armed here.
func requireTestEnv(what string) {
	requireSeamsAllowed(what, testSeamsAllowed(os.Getenv("CRM_ENV"), testing.Testing()))
}

func requireSeamsAllowed(what string, allowed bool) {
	if !allowed {
		panic(fmt.Sprintf("%s is test-only support: it requires a test binary or CRM_ENV=test|testing (got %q)", what, os.Getenv("CRM_ENV")))
	}
}
