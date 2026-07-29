package replay

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

// ErrConstructorResidue marks a harness-construction failure that LEFT the
// namespace's synthetic host row behind. newHarness removes that row
// best-effort on its own post-host failure paths; when the removal itself
// fails, the returned error wraps this sentinel so a caller can report the
// residue truthfully (errors.Is) rather than guessing. The residue is still
// reachable — the declared-seed cleanup path finds a host-only world from the
// requested namespace token.
var ErrConstructorResidue = errors.New("synthetic: harness construction left namespaced rows behind")

const (
	// ConstructorFailpointAfterHost fires immediately after the namespaced
	// synthetic host row is inserted and before the River client starts — the
	// realizable shape of the real client.Start failure path, which is otherwise
	// unreachable from a test. The best-effort host removal still runs and
	// succeeds, so the caller sees a residue-FREE constructor failure.
	ConstructorFailpointAfterHost = "after-host"
	// ConstructorFailpointAfterHostResidue fires at the same point but ALSO
	// makes the best-effort host removal fail, so the constructor returns
	// ErrConstructorResidue and the caller must report cleaned=false. Without
	// it the residue branch is unreachable from a test and "cleaned reports
	// reality" would only ever be proven in the direction that reports true.
	ConstructorFailpointAfterHostResidue = "after-host-residue"
)

var (
	constructorFailpointMu   sync.Mutex
	constructorFailpointName string
)

// SetConstructorFailpointForTest arms a harness-construction failpoint and
// returns a restore func the caller MUST defer. It is test-only support that is
// nevertheless compiled into the package (backend/tests is an external package,
// the same reason SeedHostForTest is exported) and therefore guards itself: it
// PANICS unless the process is a `go test` binary or already runs under
// CRM_ENV=test|testing, so a production build cannot arm it even by mistake.
// Pass "" to disarm.
func SetConstructorFailpointForTest(name string) (restore func()) {
	requireTestEnv("replay.SetConstructorFailpointForTest")
	constructorFailpointMu.Lock()
	prev := constructorFailpointName
	constructorFailpointName = name
	constructorFailpointMu.Unlock()
	return func() {
		constructorFailpointMu.Lock()
		constructorFailpointName = prev
		constructorFailpointMu.Unlock()
	}
}

func constructorFailpoint() string {
	constructorFailpointMu.Lock()
	defer constructorFailpointMu.Unlock()
	return constructorFailpointName
}

// forcedHostRemovalFailure returns a non-nil error when the residue failpoint is
// armed, standing in for a DELETE that genuinely fails. The real branch is
// otherwise unreachable: the delete is a single by-id statement against a row
// the constructor just inserted, so nothing a test can arrange makes it fail.
func forcedHostRemovalFailure() error {
	if constructorFailpoint() == ConstructorFailpointAfterHostResidue {
		return fmt.Errorf("synthetic: constructor failpoint %q forced the host-row removal to fail", ConstructorFailpointAfterHostResidue)
	}
	return nil
}

// requireTestEnv panics unless this is a binary built by `go test`, or a
// process already running under the test environment that gates the whole
// test-route surface. Fail-loud misuse guard for the exported test seams in
// this package; the test-binary arm cannot be set by configuration.
func requireTestEnv(what string) {
	if testing.Testing() {
		return
	}
	switch os.Getenv("CRM_ENV") {
	case "test", "testing":
		return
	default:
		panic(fmt.Sprintf("%s is test-only support: it requires a test binary or CRM_ENV=test|testing (got %q)", what, os.Getenv("CRM_ENV")))
	}
}
