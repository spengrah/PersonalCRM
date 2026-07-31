package declare

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// The declared-seeding path's one test seam. It is compiled into the package
// rather than living in a _test.go file because the test that needs it lives in
// backend/tests/api — an EXTERNAL package, the same reason SeedHostForTest is
// exported from the repository layer.
//
// The seam guards itself: it PANICS unless the process is a `go test` binary or
// already runs under CRM_ENV=test|testing, so a production build cannot arm it
// even by mistake. It returns a restore func the caller MUST defer.

const (
	// FailpointAfterFirstEntity fires immediately after the FIRST entity of a
	// declaration is created, leaving a genuinely partial world behind. It is
	// what lets TestSeedDeclaredEndpoint_FailureCarriesRecoveryMetadata make a
	// VALID seed request fail at the HTTP tier, which is the only way to assert
	// the endpoint's 500 body carries the namespace and the cleaned flag. (An
	// "OverdueBy + NoMethods" declaration is not a usable substitute — the
	// factory substitutes an unknown synthetic address rather than failing, so
	// that combination is a Register-time validation case, not a runtime error.)
	FailpointAfterFirstEntity = "after-first-entity"
)

var (
	seamMu         sync.Mutex
	armedFailpoint string
)

var knownFailpoints = map[string]bool{
	FailpointAfterFirstEntity: true,
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

func currentFailpoint() string {
	seamMu.Lock()
	defer seamMu.Unlock()
	return armedFailpoint
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
